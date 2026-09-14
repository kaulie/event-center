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
func (s *Server) handleGenericIngest(w http.ResponseWriter, r *http.Request) {
	sourceID := r.PathValue("source")
	src, err := s.store.GetSource(r.Context(), sourceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown source "+sourceID)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup source failed")
		return
	}
	if !src.Enabled {
		writeError(w, http.StatusForbidden, "source is disabled")
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		return
	}
	if err := verify.Mode(src, body, lowercaseHeaders(r)); err != nil {
		s.metrics.Inc("eventd_ingest_rejected_total", map[string]string{"provider": src.ID}, 1)
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req model.IngestRequest
	if err := json.Unmarshal(body, &req); err != nil {
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
		writeStoreError(w, err, "ingest")
		return
	}
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
