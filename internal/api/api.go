// Package api exposes the HTTP surface of the event center: webhook ingestion,
// cursor based consumption and administration of sources/subscriptions.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kaulie/event-center/internal/metrics"
	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/service"
	"github.com/kaulie/event-center/internal/store"
)

// githubWebhookPath is the only path that accepts GitHub deliveries. It matches
// what the repository hook is configured with; retired paths are removed rather
// than aliased, so there is exactly one public ingestion entry point.
const githubWebhookPath = "/github-events-ingress"

// maxBodyBytes caps accepted request bodies.
const maxBodyBytes = 5 << 20

// Config carries the HTTP level settings.
type Config struct {
	AdminToken      string
	PullDefaultSize int
	PullMaxSize     int
	PullWaitMax     time.Duration
	Version         string

	// Ingress audit trail.
	IngressLogBody    bool
	IngressLogBodyMax int
	IngressLogPath    string
}

// Server holds the HTTP dependencies.
type Server struct {
	svc     *service.Service
	store   *store.Store
	metrics *metrics.Registry
	log     *slog.Logger
	cfg     Config
	audit   *auditWriter
}

// New builds a Server.
func New(svc *service.Service, reg *metrics.Registry, log *slog.Logger, cfg Config) *Server {
	if cfg.PullDefaultSize <= 0 {
		cfg.PullDefaultSize = 100
	}
	if cfg.PullMaxSize < cfg.PullDefaultSize {
		cfg.PullMaxSize = cfg.PullDefaultSize
	}
	if cfg.PullWaitMax <= 0 {
		cfg.PullWaitMax = 30 * time.Second
	}
	if cfg.IngressLogBodyMax <= 0 {
		cfg.IngressLogBodyMax = 8192
	}

	audit, err := newAuditWriter(cfg.IngressLogPath)
	if err != nil {
		// A broken audit sink must not take the service down, but it is
		// important enough to be loud about it.
		log.Error("ingress audit file unavailable, falling back to journald only",
			"path", cfg.IngressLogPath, "error", err)
	} else if audit != nil {
		log.Info("ingress audit log enabled", "path", cfg.IngressLogPath)
	}

	return &Server{svc: svc, store: svc.Store(), metrics: reg, log: log, cfg: cfg, audit: audit}
}

// Close releases the audit file handle.
func (s *Server) Close() error { return s.audit.Close() }

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Ingestion. These routes carry the audit trail: every attempt is
	// logged with its outcome, correlation id and payload fingerprint.
	mux.Handle("POST "+githubWebhookPath, s.ingressLog(http.HandlerFunc(s.handleGitHubWebhook)))
	mux.Handle("POST /v1/ingest/{source}", s.ingressLog(http.HandlerFunc(s.handleGenericIngest)))

	// Consumption.
	mux.HandleFunc("GET /v1/streams", s.authConsume(s.handleListStreams))
	mux.HandleFunc("GET /v1/streams/{stream}/events", s.authConsume(s.handlePull))
	mux.HandleFunc("GET /v1/events/{id}", s.authConsume(s.handleGetEvent))
	mux.HandleFunc("POST /v1/subscriptions/{id}/ack", s.handleAck)

	// Administration.
	mux.HandleFunc("GET /v1/sources", s.authAdmin(s.handleListSources))
	mux.HandleFunc("PUT /v1/sources/{id}", s.authAdmin(s.handleUpsertSource))
	mux.HandleFunc("DELETE /v1/sources/{id}", s.authAdmin(s.handleDeleteSource))
	mux.HandleFunc("GET /v1/subscriptions", s.authAdmin(s.handleListSubscriptions))
	mux.HandleFunc("POST /v1/subscriptions", s.authAdmin(s.handleCreateSubscription))
	mux.HandleFunc("DELETE /v1/subscriptions/{id}", s.authAdmin(s.handleDeleteSubscription))
	mux.HandleFunc("POST /v1/subscriptions/{id}/pause", s.authAdmin(s.handlePauseSubscription))
	mux.HandleFunc("POST /v1/subscriptions/{id}/resume", s.authAdmin(s.handleResumeSubscription))
	mux.HandleFunc("GET /v1/deliveries", s.authAdmin(s.handleListDeliveries))
	mux.HandleFunc("POST /v1/deliveries/requeue", s.authAdmin(s.handleRequeueDeliveries))

	// Operational. /health is the path the deployment control plane probes for
	// every service; /healthz is kept as the conventional Kubernetes alias.
	mux.HandleFunc("GET /health", s.handleHealthz)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	return s.recoverPanic(s.logRequests(mux))
}

// --- middleware -----------------------------------------------------------

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.metrics.Inc("eventd_http_requests_total",
			map[string]string{"method": r.Method, "status": statusClass(rec.status)}, 1)
		// Ingest routes have their own audit trail (see ingressLog); a second
		// generic line would just double the volume.
		switch r.URL.Path {
		case "/health", "/healthz", "/readyz", "/metrics":
		default:
			if !isIngressPath(r.URL.Path) {
				s.log.Info("http request",
					"method", r.Method, "path", r.URL.Path,
					"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
			}
		}
	})
}

func isIngressPath(path string) bool {
	return path == githubWebhookPath || strings.HasPrefix(path, "/v1/ingest/")
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// --- auth -----------------------------------------------------------------

// authAdmin guards administrative endpoints with the shared admin token. When
// no token is configured the endpoints stay open (development mode).
func (s *Server) authAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken != "" && !tokenMatches(s.cfg.AdminToken, bearer(r)) {
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next(w, r)
	}
}

// authConsume allows the admin token or the API key of any active subscription.
func (s *Server) authConsume(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			next(w, r)
			return
		}
		if tokenMatches(s.cfg.AdminToken, bearer(r)) {
			next(w, r)
			return
		}
		key := apiKey(r)
		if key == "" {
			writeError(w, http.StatusUnauthorized, "missing api key")
			return
		}
		subs, err := s.store.ListSubscriptions(r.Context(), "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "lookup subscription failed")
			return
		}
		for _, sub := range subs {
			if sub.Status == model.StatusActive && sub.Secret != "" && tokenMatches(sub.Secret, key) {
				next(w, r)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid api key")
	}
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

func apiKey(r *http.Request) string {
	if v := r.Header.Get("X-API-Key"); v != "" {
		return v
	}
	return bearer(r)
}

func tokenMatches(expected, got string) bool {
	if expected == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

// --- helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeStoreError(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var invalid *model.Error
	if errors.As(err, &invalid) {
		writeError(w, http.StatusBadRequest, invalid.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, action+" failed")
}

func lowercaseHeaders(r *http.Request) map[string]string {
	out := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

func whitelistHeaders(headers map[string]string, names ...string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		if v, ok := headers[strings.ToLower(n)]; ok && v != "" {
			out[n] = v
		}
	}
	return out
}
