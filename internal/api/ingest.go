package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/store"
	"github.com/kaulie/event-center/internal/verify"
)

// handleGenericIngest accepts a normalised event envelope from any registered
// source: POST /v1/ingest/{source}.
//
// @Summary      通用事件注入
// @Description  归一化事件信封 → 持久化 → 按订阅扇出（push 入队、pull 可拉取）。
// @Description  省略 provider/stream 时取来源自身的默认值；type 省略 provider 前缀时自动补来源的 type_prefix。
// @Tags         ingest
// @Accept       json
// @Produce      json
// @Param        source  path  string               true  "来源 id"  example(cicd)
// @Param        event   body  model.IngestRequest  true  "事件信封（type 必填）"
// @Success      202  {object}  model.IngestResponse  "已接收"
// @Success      200  {object}  model.IngestResponse  "重复事件（dedupe_key 命中）"
// @Failure      400  {object}  api.ErrorResponse     "信封非法（如缺少 type）"
// @Failure      401  {object}  api.ErrorResponse     "来源鉴权失败"
// @Failure      403  {object}  api.ErrorResponse     "来源已禁用"
// @Failure      404  {object}  api.ErrorResponse     "来源未注册"
// @Security     SourceSecret
// @Router       /v1/ingest/{source} [post]
func (s *Server) handleGenericIngest(w http.ResponseWriter, r *http.Request) {
	sourceID := r.PathValue("source")
	ingressMark(r, func(rec *ingressRecord) { rec.Source = sourceID })

	src, err := s.store.GetSource(r.Context(), sourceID)
	if errors.Is(err, store.ErrNotFound) {
		ingressReject(r, "unknown source "+sourceID)
		writeError(w, http.StatusNotFound, "unknown source "+sourceID)
		return
	}
	if err != nil {
		ingressReject(r, "source lookup failed")
		writeError(w, http.StatusInternalServerError, "lookup source failed")
		return
	}
	if !src.Enabled {
		ingressReject(r, "source is disabled")
		writeError(w, http.StatusForbidden, "source is disabled")
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		ingressReject(r, "body rejected: "+err.Error())
		return
	}
	ingressCaptureBody(r, body)

	if err := verify.Mode(src, body, lowercaseHeaders(r)); err != nil {
		s.metrics.Inc("eventd_ingest_rejected_total", map[string]string{"provider": src.ID}, 1)
		ingressReject(r, "authentication failed")
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req model.IngestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		ingressReject(r, "invalid JSON body")
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	provider := req.Provider
	if provider == "" {
		provider = src.ID
	}
	stream := req.Stream
	if stream == "" {
		stream = src.DefaultStream
	}
	eventType := req.Type
	if eventType == "" {
		ingressReject(r, "type is required")
		writeError(w, http.StatusBadRequest, "type is required")
		return
	}
	if src.TypePrefix != "" && !strings.HasPrefix(eventType, src.TypePrefix+".") {
		eventType = src.TypePrefix + "." + eventType
	}
	headers := req.Headers
	if headers == nil {
		headers = map[string]string{}
	}

	ev := &model.Event{
		Provider:   provider,
		Type:       eventType,
		Stream:     stream,
		Subject:    req.Subject,
		SourceTime: req.SourceTime,
		DedupeKey:  req.DedupeKey,
		DataType:   req.DataType,
		Headers:    headers,
		Data:       req.Data,
	}
	s.respondIngest(w, r, ev)
}

func (s *Server) respondIngest(w http.ResponseWriter, r *http.Request, ev *model.Event) {
	stored, duplicate, err := s.svc.Ingest(r.Context(), ev)
	if err != nil {
		ingressReject(r, "persist failed: "+err.Error())
		writeStoreError(w, err, "ingest")
		return
	}
	ingressMark(r, func(rec *ingressRecord) {
		rec.EventID = stored.ID
		rec.Seq = stored.Seq
		rec.StreamSeq = stored.StreamSeq
		rec.Stream = stored.Stream
		rec.Provider = stored.Provider
		rec.Type = stored.Type
		rec.DedupeKey = stored.DedupeKey
		if duplicate {
			rec.Outcome = outcomeDuplicate
		} else {
			rec.Outcome = outcomeAccepted
		}
	})
	if duplicate {
		writeJSON(w, http.StatusOK, model.IngestResponse{
			Status:    "duplicate",
			Duplicate: true,
			Event:     stored,
		})
		return
	}
	writeJSON(w, http.StatusAccepted, model.IngestResponse{
		Status: "accepted",
		Event:  stored,
	})
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return nil, err
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return nil, errors.New("body too large")
	}
	return body, nil
}
