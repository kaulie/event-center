package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kaulie/event-center/internal/idgen"
)

// Outcome values of an ingress attempt.
const (
	outcomeAccepted  = "accepted"
	outcomeDuplicate = "duplicate"
	outcomeRejected  = "rejected"
	outcomeError     = "error"
)

// ingressRecord is the audit trail of one inbound event injection.
//
// It exists so that "why is this data missing / wrong" is answerable after the
// fact: an accepted event can be looked up by id or seq, and a rejected request
// (invalid signature, malformed JSON, unknown source) leaves a record even
// though nothing was persisted.
type ingressRecord struct {
	RequestID string
	Path      string
	Source    string
	Method    string
	RemoteIP  string
	UserAgent string

	Status     int
	Outcome    string
	Reason     string
	DurationMS int64

	// Correlation with the stored event, for accepted/duplicate requests.
	EventID   string
	Seq       int64
	StreamSeq int64
	Stream    string
	Provider  string
	Type      string
	DedupeKey string

	// Payload fingerprint. The hash is always logged; the body itself is kept
	// only for rejected requests (there is no other copy of those) or when
	// explicitly enabled.
	BodySHA256 string
	BodyBytes  int
	Body       []byte
}

type ingressCtxKey struct{}

// ingressFrom returns the record being built for this request, if any.
func ingressFrom(r *http.Request) *ingressRecord {
	rec, _ := r.Context().Value(ingressCtxKey{}).(*ingressRecord)
	return rec
}

// ingressMark mutates the current ingress record if there is one, so handlers
// do not care whether audit logging is wired up.
func ingressMark(r *http.Request, mutate func(*ingressRecord)) {
	if rec := ingressFrom(r); rec != nil {
		mutate(rec)
	}
}

func ingressReject(r *http.Request, reason string) {
	ingressMark(r, func(rec *ingressRecord) {
		rec.Outcome = outcomeRejected
		rec.Reason = reason
	})
}

func ingressCaptureBody(r *http.Request, body []byte) {
	sum := sha256.Sum256(body)
	ingressMark(r, func(rec *ingressRecord) {
		rec.BodySHA256 = hex.EncodeToString(sum[:])
		rec.BodyBytes = len(body)
		rec.Body = body
	})
}

// ingressRecorder captures the status code written by a handler.
type ingressRecorder struct {
	http.ResponseWriter
	status int
}

func (r *ingressRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *ingressRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// auditWriter appends one JSON line per ingress attempt to a file so the trail
// survives independently of journald rotation and retention policy.
type auditWriter struct {
	mu   sync.Mutex
	file *os.File
}

func newAuditWriter(path string) (*auditWriter, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &auditWriter{file: f}, nil
}

func (a *auditWriter) write(line map[string]any) {
	if a == nil || a.file == nil {
		return
	}
	encoded, err := json.Marshal(line)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.file.Write(append(encoded, '\n'))
}

func (a *auditWriter) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.file.Close()
}

// ingressLog wraps the ingest endpoints with an audit trail.
func (s *Server) ingressLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &ingressRecord{
			RequestID: requestID(r),
			Path:      r.URL.Path,
			Method:    r.Method,
			RemoteIP:  clientIP(r),
			UserAgent: r.Header.Get("User-Agent"),
		}
		// Echo the id back so a caller can quote it when reporting a problem.
		w.Header().Set("X-Request-ID", rec.RequestID)

		rw := &ingressRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), ingressCtxKey{}, rec)))
		rec.DurationMS = time.Since(start).Milliseconds()

		if rec.Status == 0 {
			rec.Status = rw.status
			if rec.Status == 0 {
				rec.Status = http.StatusOK
			}
		}
		switch {
		case rec.Outcome == "" && rec.Status >= 500:
			rec.Outcome = outcomeError
		case rec.Outcome == "" && rec.Status >= 400:
			rec.Outcome = outcomeRejected
		case rec.Outcome == "":
			rec.Outcome = outcomeAccepted
		}
		s.emitIngress(rec)
	})
}

func (s *Server) emitIngress(rec *ingressRecord) {
	attrs := []any{
		"request_id", rec.RequestID,
		"path", rec.Path,
		"source", rec.Source,
		"status", rec.Status,
		"outcome", rec.Outcome,
		"duration_ms", rec.DurationMS,
		"remote_ip", rec.RemoteIP,
		"body_sha256", rec.BodySHA256,
		"body_bytes", rec.BodyBytes,
	}
	if rec.UserAgent != "" {
		attrs = append(attrs, "user_agent", rec.UserAgent)
	}
	if rec.EventID != "" {
		attrs = append(attrs, "event_id", rec.EventID, "seq", rec.Seq, "stream_seq", rec.StreamSeq)
	}
	if rec.Provider != "" {
		attrs = append(attrs, "provider", rec.Provider, "type", rec.Type, "stream", rec.Stream)
	}
	if rec.DedupeKey != "" {
		attrs = append(attrs, "dedupe_key", rec.DedupeKey)
	}
	if rec.Reason != "" {
		attrs = append(attrs, "reason", rec.Reason)
	}

	// Keep the payload when nothing else holds a copy of it, or when the
	// operator explicitly asked for full bodies.
	keepBody := rec.Body != nil && (rec.Outcome == outcomeRejected || s.cfg.IngressLogBody)
	if keepBody {
		attrs = append(attrs, "body_truncated", len(rec.Body) > s.cfg.IngressLogBodyMax)
		attrs = append(attrs, "body", string(truncate(rec.Body, s.cfg.IngressLogBodyMax)))
	}

	switch rec.Outcome {
	case outcomeRejected:
		s.log.Warn("ingress rejected", attrs...)
	case outcomeError:
		s.log.Error("ingress failed", attrs...)
	default:
		s.log.Info("ingress accepted", attrs...)
	}

	if s.audit == nil {
		return
	}
	line := map[string]any{
		"time":        time.Now().UTC().Format(time.RFC3339Nano),
		"request_id":  rec.RequestID,
		"path":        rec.Path,
		"source":      rec.Source,
		"status":      rec.Status,
		"outcome":     rec.Outcome,
		"duration_ms": rec.DurationMS,
		"remote_ip":   rec.RemoteIP,
		"body_sha256": rec.BodySHA256,
		"body_bytes":  rec.BodyBytes,
	}
	for key, value := range map[string]any{
		"event_id":   rec.EventID,
		"seq":        rec.Seq,
		"stream_seq": rec.StreamSeq,
		"provider":   rec.Provider,
		"type":       rec.Type,
		"stream":     rec.Stream,
		"dedupe_key": rec.DedupeKey,
		"reason":     rec.Reason,
	} {
		if s, ok := value.(string); ok && s == "" {
			continue
		}
		if n, ok := value.(int64); ok && n == 0 {
			continue
		}
		line[key] = value
	}
	if keepBody {
		line["body"] = string(truncate(rec.Body, s.cfg.IngressLogBodyMax))
	}
	s.audit.write(line)
}

func truncate(body []byte, max int) []byte {
	if max > 0 && len(body) > max {
		return body[:max]
	}
	return body
}

// requestID honours an inbound correlation id, otherwise mints one.
func requestID(r *http.Request) string {
	for _, header := range []string{"X-Request-ID", "X-Correlation-ID", "X-GitHub-Delivery"} {
		if v := strings.TrimSpace(r.Header.Get(header)); v != "" {
			return v
		}
	}
	return idgen.NewID("req")
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
			return first
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
