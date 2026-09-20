package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kaulie/event-center/internal/idgen"
	"github.com/kaulie/event-center/internal/model"
)

// toSourceViews hides the secret of every source but tells the caller whether
// one is configured.
func toSourceViews(srcs []model.Source) []SourceView {
	out := make([]SourceView, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, SourceView{Source: s, HasSecret: s.Secret != ""})
	}
	return out
}

// toSubscriptionViews hides the secrets of every subscription but reports
// whether one is configured.
func toSubscriptionViews(subs []model.Subscription) []SubscriptionView {
	out := make([]SubscriptionView, 0, len(subs))
	for _, s := range subs {
		out = append(out, SubscriptionView{Subscription: s, HasSecret: s.Secret != ""})
	}
	return out
}

// handleListSources returns every registered event source.
//
// @Summary  列出来源
// @Tags     admin
// @Produce  json
// @Success  200  {object}  api.SourcesResponse
// @Security AdminToken
// @Router   /v1/sources [get]
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.store.ListSources(r.Context())
	if err != nil {
		writeStoreError(w, err, "list sources")
		return
	}
	writeJSON(w, http.StatusOK, SourcesResponse{Sources: toSourceViews(srcs)})
}

// handleUpsertSource creates or updates a source. Omitted fields keep their
// previous value, so an existing secret survives an update that does not
// mention one.
//
// @Summary      创建或更新来源
// @Description  省略的字段保留原值，所以更新时不写 secret 也不会把已有的密钥清掉。
// @Tags         admin
// @Accept       json
// @Produce      json
// @Param        id     path  string                        true  "来源 id"        example(cicd)
// @Param        source body  api.SourceUpsertRequest       true  "来源定义"
// @Success      200  {object}  api.SourceView
// @Failure      400  {object}  api.ErrorResponse
// @Security     AdminToken
// @Router       /v1/sources/{id} [put]
func (s *Server) handleUpsertSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		writeError(w, http.StatusBadRequest, "source id is required")
		return
	}

	var req SourceUpsertRequest
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
	writeJSON(w, http.StatusOK, SourceView{Source: *src, HasSecret: src.Secret != ""})
}

// handleDeleteSource removes a source registration.
//
// @Summary  删除来源
// @Tags     admin
// @Param    id  path  string  true  "来源 id"
// @Success  204
// @Failure  404  {object}  api.ErrorResponse
// @Security AdminToken
// @Router   /v1/sources/{id} [delete]
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSource(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err, "delete source")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// handleListSubscriptions returns subscriptions, optionally filtered by
// delivery mode.
//
// @Summary  列出订阅
// @Tags     admin
// @Produce  json
// @Param    delivery  query  string  false  "按投递方式过滤"  Enums(push,pull)
// @Success  200  {object}  api.SubscriptionsResponse
// @Security AdminToken
// @Router   /v1/subscriptions [get]
func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListSubscriptions(r.Context(), r.URL.Query().Get("delivery"))
	if err != nil {
		writeStoreError(w, err, "list subscriptions")
		return
	}
	writeJSON(w, http.StatusOK, SubscriptionsResponse{Subscriptions: toSubscriptionViews(subs)})
}

// handleCreateSubscription registers a downstream consumer.
//
// @Summary      创建订阅
// @Description  push 必须给 endpoint；不给 secret 时生成一个，**只在这里返回一次**。
// @Tags         admin
// @Accept       json
// @Produce      json
// @Param        subscription  body  api.SubscriptionCreateRequest  true  "订阅定义"
// @Success      201  {object}  api.SubscriptionCreateResponse
// @Failure      400  {object}  api.ErrorResponse  "参数非法（如 push 缺少 endpoint）"
// @Security     AdminToken
// @Router       /v1/subscriptions [post]
func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	var req SubscriptionCreateRequest
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

	resp := SubscriptionCreateResponse{
		Subscription: SubscriptionView{Subscription: *sub, HasSecret: true},
		HasSecret:    true,
	}
	if generated {
		// The generated secret is only ever returned here.
		resp.Secret = sub.Secret
		resp.SecretGenerated = true
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleDeleteSubscription removes a subscription and its delivery queue.
//
// @Summary  删除订阅（含其投递队列）
// @Tags     admin
// @Param    id  path  string  true  "订阅 id"
// @Success  204
// @Failure  404  {object}  api.ErrorResponse
// @Security AdminToken
// @Router   /v1/subscriptions/{id} [delete]
func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSubscription(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err, "delete subscription")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// handlePauseSubscription stops delivery for a subscription without dropping
// its queue: events keep being enqueued and are sent in order after resume.
//
// @Summary  暂停投递（仍继续入队，resume 后按序补投）
// @Tags     admin
// @Produce  json
// @Param    id  path  string  true  "订阅 id"
// @Success  200  {object}  api.SubscriptionView
// @Failure  404  {object}  api.ErrorResponse
// @Security AdminToken
// @Router   /v1/subscriptions/{id}/pause [post]
func (s *Server) handlePauseSubscription(w http.ResponseWriter, r *http.Request) {
	s.setSubscriptionStatus(w, r, model.StatusPaused)
}

// handleResumeSubscription re-enables delivery for a paused subscription.
//
// @Summary  恢复投递
// @Tags     admin
// @Produce  json
// @Param    id  path  string  true  "订阅 id"
// @Success  200  {object}  api.SubscriptionView
// @Failure  404  {object}  api.ErrorResponse
// @Security AdminToken
// @Router   /v1/subscriptions/{id}/resume [post]
func (s *Server) handleResumeSubscription(w http.ResponseWriter, r *http.Request) {
	s.setSubscriptionStatus(w, r, model.StatusActive)
}

// handleSetSubscriptionStatus is the shared implementation of pause/resume.
func (s *Server) setSubscriptionStatus(w http.ResponseWriter, r *http.Request, status string) {
	if err := s.store.SetSubscriptionStatus(r.Context(), r.PathValue("id"), status); err != nil {
		writeStoreError(w, err, "update subscription status")
		return
	}
	sub, err := s.store.GetSubscription(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err, "reload subscription")
		return
	}
	writeJSON(w, http.StatusOK, SubscriptionView{Subscription: *sub, HasSecret: sub.Secret != ""})
}

// handleListDeliveries reports push delivery state, including the DLQ.
//
// @Summary  查询投递记录（含 DLQ）
// @Tags     admin
// @Produce  json
// @Param    subscription_id  query  string   false  "只查某个订阅"      example(sub_01J8Q7ZC4K9W2M3N4P5Q6R7S)
// @Param    status           query  string   false  "按投递状态过滤"    Enums(pending,inflight,delivered,dead)
// @Param    limit            query  integer  false  "最多返回条数"      default(100)
// @Success  200  {object}  api.DeliveriesResponse
// @Security AdminToken
// @Router   /v1/deliveries [get]
func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	limit := int(intQuery(r, "limit", 100))
	items, err := s.store.ListDeliveries(r.Context(),
		r.URL.Query().Get("subscription_id"), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeStoreError(w, err, "list deliveries")
		return
	}
	writeJSON(w, http.StatusOK, DeliveriesResponse{Deliveries: items})
}

// handleRequeueDeliveries puts failed deliveries back on the queue.
//
// @Summary  重投失败的投递（默认重投 dead）
// @Tags     admin
// @Accept   json
// @Produce  json
// @Param    requeue  body  api.RequeueRequest  false  "过滤条件；省略即重投所有 dead"
// @Success  200  {object}  api.RequeueResponse
// @Security AdminToken
// @Router   /v1/deliveries/requeue [post]
func (s *Server) handleRequeueDeliveries(w http.ResponseWriter, r *http.Request) {
	var req RequeueRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	n, err := s.store.RequeueDeliveries(r.Context(), req.SubscriptionID, req.Status, req.Limit)
	if err != nil {
		writeStoreError(w, err, "requeue deliveries")
		return
	}
	writeJSON(w, http.StatusOK, RequeueResponse{Requeued: n})
}

// newSubscriptionSecret mints the secret used for incoming pull keys and for
// outbound push signatures.
func newSubscriptionSecret() string { return idgen.NewID("evtsec") }
