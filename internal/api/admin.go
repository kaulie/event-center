package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kaulie/event-center/internal/idgen"
	"github.com/kaulie/event-center/internal/model"
)

// sourceView hides the secret but tells the caller whether one is configured.
type sourceView struct {
	model.Source
	HasSecret bool `json:"has_secret"`
}

func toSourceViews(srcs []model.Source) []sourceView {
	out := make([]sourceView, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, sourceView{Source: s, HasSecret: s.Secret != ""})
	}
	return out
}

// subscriptionView hides the secret but reports whether one is configured.
type subscriptionView struct {
	model.Subscription
	HasSecret bool `json:"has_secret"`
}

func toSubscriptionViews(subs []model.Subscription) []subscriptionView {
	out := make([]subscriptionView, 0, len(subs))
	for _, s := range subs {
		out = append(out, subscriptionView{Subscription: s, HasSecret: s.Secret != ""})
	}
	return out
}

// handleListSources returns every registered event source.
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.store.ListSources(r.Context())
	if err != nil {
		writeStoreError(w, err, "list sources")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": toSourceViews(srcs)})
}

// handleUpsertSource creates or updates a source. Omitted fields keep their
// previous value, so an existing secret survives an update that does not
// mention one.
func (s *Server) handleUpsertSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		writeError(w, http.StatusBadRequest, "source id is required")
		return
	}

	var req struct {
		Kind          string `json:"kind"`
		Secret        string `json:"secret"`
		VerifyMode    string `json:"verify_mode"`
		TypePrefix    string `json:"type_prefix"`
		DefaultStream string `json:"default_stream"`
		Enabled       *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	src := &model.Source{
		ID:            id,
		Kind:          req.Kind,
		Secret:        req.Secret,
		VerifyMode:    req.VerifyMode,
		TypePrefix:    req.TypePrefix,
		DefaultStream: req.DefaultStream,
		Enabled:       true,
	}
	if existing, err := s.store.GetSource(r.Context(), id); err == nil {
		if src.Kind == "" {
			src.Kind = existing.Kind
		}
		if src.Secret == "" {
			src.Secret = existing.Secret
		}
		if src.VerifyMode == "" {
			src.VerifyMode = existing.VerifyMode
		}
		if src.DefaultStream == "" {
			src.DefaultStream = existing.DefaultStream
		}
		if src.TypePrefix == "" {
			src.TypePrefix = existing.TypePrefix
		}
		src.CreatedAt = existing.CreatedAt
		src.Enabled = existing.Enabled
	}
	if req.Enabled != nil {
		src.Enabled = *req.Enabled
	}
	if src.Kind == "" {
		src.Kind = "webhook"
	}
	if src.DefaultStream == "" {
		src.DefaultStream = model.DefaultStream
	}
	if src.VerifyMode == "" {
		if src.Secret != "" {
			src.VerifyMode = model.VerifyHMACSHA256
		} else {
			src.VerifyMode = model.VerifyNone
		}
	}
	if src.TypePrefix == "" {
		src.TypePrefix = id
	}

	if err := s.store.UpsertSource(r.Context(), src); err != nil {
		writeStoreError(w, err, "upsert source")
		return
	}
	writeJSON(w, http.StatusOK, sourceView{Source: *src, HasSecret: src.Secret != ""})
}

// handleDeleteSource removes a source registration.
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSource(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err, "delete source")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// handleListSubscriptions returns subscriptions, optionally filtered by
// delivery mode.
func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListSubscriptions(r.Context(), r.URL.Query().Get("delivery"))
	if err != nil {
		writeStoreError(w, err, "list subscriptions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": toSubscriptionViews(subs)})
}

// handleCreateSubscription registers a downstream consumer.
func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID              string   `json:"id"`
		Name            string   `json:"name"`
		Stream          string   `json:"stream"`
		TypeFilters     []string `json:"type_filters"`
		ProviderFilters []string `json:"provider_filters"`
		SubjectPattern  string   `json:"subject_pattern"`
		Delivery        string   `json:"delivery"`
		Endpoint        string   `json:"endpoint"`
		Secret          string   `json:"secret"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Delivery == "" {
		req.Delivery = model.DeliveryPush
	}
	if req.Delivery != model.DeliveryPush && req.Delivery != model.DeliveryPull {
		writeError(w, http.StatusBadRequest, "delivery must be push or pull")
		return
	}
	if req.Delivery == model.DeliveryPush && req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "push subscription requires an endpoint")
		return
	}

	sub := &model.Subscription{
		ID:              req.ID,
		Name:            req.Name,
		Stream:          req.Stream,
		TypeFilters:     req.TypeFilters,
		ProviderFilters: req.ProviderFilters,
		SubjectPattern:  req.SubjectPattern,
		Delivery:        req.Delivery,
		Endpoint:        req.Endpoint,
		Secret:          req.Secret,
		Status:          model.StatusActive,
	}
	if sub.ID == "" {
		sub.ID = idgen.NewID("sub")
	}
	generated := false
	if sub.Secret == "" {
		sub.Secret = newSubscriptionSecret()
		generated = true
	}

	if err := s.store.CreateSubscription(r.Context(), sub); err != nil {
		writeStoreError(w, err, "create subscription")
		return
	}
	// Re-read so the response shows exactly what was persisted (empty filters
	// come back as [] rather than null).
	if stored, err := s.store.GetSubscription(r.Context(), sub.ID); err == nil {
		sub = stored
	}

	resp := map[string]any{
		"subscription": subscriptionView{Subscription: *sub, HasSecret: true},
		"has_secret":   true,
	}
	if generated {
		// The generated secret is only ever returned here.
		resp["secret"] = sub.Secret
		resp["secret_generated"] = true
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleDeleteSubscription removes a subscription and its delivery queue.
func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSubscription(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err, "delete subscription")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// handleSetSubscriptionStatus pauses or resumes a subscription.
func (s *Server) handleSetSubscriptionStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.SetSubscriptionStatus(r.Context(), r.PathValue("id"), status); err != nil {
			writeStoreError(w, err, "update subscription status")
			return
		}
		sub, err := s.store.GetSubscription(r.Context(), r.PathValue("id"))
		if err != nil {
			writeStoreError(w, err, "reload subscription")
			return
		}
		writeJSON(w, http.StatusOK, subscriptionView{Subscription: *sub, HasSecret: sub.Secret != ""})
	}
}

// handleListDeliveries reports push delivery state, including the DLQ.
func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	limit := int(intQuery(r, "limit", 100))
	items, err := s.store.ListDeliveries(r.Context(),
		r.URL.Query().Get("subscription_id"), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeStoreError(w, err, "list deliveries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": items})
}

// handleRequeueDeliveries puts failed deliveries back on the queue.
func (s *Server) handleRequeueDeliveries(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SubscriptionID string `json:"subscription_id"`
		Status         string `json:"status"`
		Limit          int    `json:"limit"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	n, err := s.store.RequeueDeliveries(r.Context(), req.SubscriptionID, req.Status, req.Limit)
	if err != nil {
		writeStoreError(w, err, "requeue deliveries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requeued": n})
}

// newSubscriptionSecret mints the secret used for incoming pull keys and for
// outbound push signatures.
func newSubscriptionSecret() string { return idgen.NewID("evtsec") }
