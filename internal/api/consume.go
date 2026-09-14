package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/store"
)

// handlePull implements cursor based consumption:
//
//	GET /v1/streams/{stream}/events?after=<cursor>&limit=<n>&wait=<duration>
//
// "after" is a stream_seq for a concrete stream and the global seq for the
// pseudo stream "all". When wait is set the request is held open (long poll)
// until events arrive or the wait budget elapses, so consumers can tail the log
// without busy looping.
func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	stream := r.PathValue("stream")
	after := int64(intQuery(r, "after", intQuery(r, "cursor", 0)))
	limit := int(intQuery(r, "limit", int64(s.cfg.PullDefaultSize)))
	if limit > s.cfg.PullMaxSize {
		limit = s.cfg.PullMaxSize
	}
	if limit <= 0 {
		limit = s.cfg.PullDefaultSize
	}

	wait := s.waitDuration(r)
	ctx := r.Context()
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}

	deadline := time.Now().Add(wait)
	for {
		res, err := s.store.ListEvents(ctx, stream, after, limit)
		if err != nil {
			if ctx.Err() != nil {
				writeJSON(w, http.StatusOK, &model.ListResult{Events: []model.Event{}, NextCursor: after})
				return
			}
			writeStoreError(w, err, "list events")
			return
		}
		if len(res.Events) > 0 {
			s.metrics.Inc("eventd_pull_events_total", map[string]string{"stream": stream}, int64(len(res.Events)))
			writeJSON(w, http.StatusOK, res)
			return
		}
		if wait <= 0 || time.Now().After(deadline) || ctx.Err() != nil {
			writeJSON(w, http.StatusOK, res)
			return
		}
		select {
		case <-ctx.Done():
			writeJSON(w, http.StatusOK, &model.ListResult{Events: []model.Event{}, NextCursor: after})
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// handleGetEvent returns a single event by id.
func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := s.store.GetEvent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err, "get event")
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// handleListStreams reports the known streams and the global sequence head.
func (s *Server) handleListStreams(w http.ResponseWriter, r *http.Request) {
	streams, err := s.store.ListStreams(r.Context())
	if err != nil {
		writeStoreError(w, err, "list streams")
		return
	}
	_, maxSeq, _, err := s.store.Stats(r.Context())
	if err != nil {
		writeStoreError(w, err, "read stats")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"streams":    streams,
		"global_seq": maxSeq,
		"stream_all": model.StreamAll,
	})
}

// handleAck commits a pull consumer cursor.
//
// POST /v1/subscriptions/{id}/ack  {"cursor": 42}
//
// Callers authenticate with the admin token or with the subscription's own API
// key, and the cursor only ever moves forward.
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	subID := r.PathValue("id")
	sub, err := s.store.GetSubscription(r.Context(), subID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown subscription")
		return
	}
	if err != nil {
		writeStoreError(w, err, "lookup subscription")
		return
	}

	authorized := s.cfg.AdminToken != "" && tokenMatches(s.cfg.AdminToken, bearer(r))
	if !authorized && sub.Secret != "" {
		authorized = tokenMatches(sub.Secret, apiKey(r))
	}
	if !authorized && s.cfg.AdminToken == "" && sub.Secret == "" {
		authorized = true
	}
	if !authorized {
		writeError(w, http.StatusUnauthorized, "invalid credentials for subscription")
		return
	}

	var req struct {
		Cursor *int64 `json:"cursor"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Cursor == nil {
		writeError(w, http.StatusBadRequest, "cursor is required")
		return
	}
	if err := s.store.AdvanceCursor(r.Context(), subID, *req.Cursor); err != nil {
		writeStoreError(w, err, "ack")
		return
	}
	updated, err := s.store.GetSubscription(r.Context(), subID)
	if err != nil {
		writeStoreError(w, err, "reload subscription")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) waitDuration(r *http.Request) time.Duration {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if secs, convErr := strconv.Atoi(raw); convErr == nil {
			d = time.Duration(secs) * time.Second
		} else {
			return 0
		}
	}
	if d < 0 {
		return 0
	}
	if d > s.cfg.PullWaitMax {
		return s.cfg.PullWaitMax
	}
	return d
}

func intQuery(r *http.Request, name string, def int64) int64 {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}
