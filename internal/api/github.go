package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/store"
	"github.com/kaulie/event-center/internal/verify"
)

// githubPayload is the subset of the GitHub webhook payload the adapter needs
// to build a subject and a sub-typed event name. The full payload is stored
// verbatim, so nothing is lost by only decoding a few fields.
type githubPayload struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	Number     int    `json:"number"`
	Repository struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Organization struct {
		Login string `json:"login"`
	} `json:"organization"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// handleGitHubWebhook implements POST /webhooks/github.
//
// The event type is derived from the X-GitHub-Event header ("push",
// "pull_request", ...) and the payload action, producing names such as
// github.push or github.pull_request.opened. X-GitHub-Delivery is used as the
// dedupe key so GitHub retries do not create duplicates.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	ingressMark(r, func(rec *ingressRecord) { rec.Source = "github" })

	src, err := s.store.GetSource(r.Context(), "github")
	if errors.Is(err, store.ErrNotFound) {
		ingressReject(r, "github source is not configured")
		writeError(w, http.StatusServiceUnavailable, "github source is not configured")
		return
	}
	if err != nil {
		ingressReject(r, "source lookup failed")
		writeError(w, http.StatusInternalServerError, "lookup source failed")
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		ingressReject(r, "body rejected: "+err.Error())
		return
	}
	ingressCaptureBody(r, body)
	headers := lowercaseHeaders(r)

	// GitHub signs with the source secret; fall back to the global secret when
	// the source row carries none.
	verifyErr := verify.HMACSHA256(src.Secret, body, headers["x-hub-signature-256"], headers["x-hub-signature"])
	if verifyErr != nil {
		s.metrics.Inc("eventd_ingest_rejected_total", map[string]string{"provider": "github"}, 1)
		ingressReject(r, "signature verification failed: "+verifyErr.Error())
		writeError(w, http.StatusUnauthorized, verifyErr.Error())
		return
	}

	ghEvent := headers["x-github-event"]
	if ghEvent == "" {
		ingressReject(r, "missing X-GitHub-Event header")
		writeError(w, http.StatusBadRequest, "missing X-GitHub-Event header")
		return
	}

	var payload githubPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		ingressReject(r, "invalid JSON payload")
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	prefix := src.TypePrefix
	if prefix == "" {
		prefix = "github"
	}
	eventType := prefix + "." + ghEvent
	if payload.Action != "" {
		eventType += "." + payload.Action
	}

	stream := src.DefaultStream
	if stream == "" {
		stream = model.DefaultStream
	}

	ev := &model.Event{
		Provider:  "github",
		Type:      eventType,
		Stream:    stream,
		Subject:   githubSubject(payload),
		DedupeKey: headers["x-github-delivery"],
		DataType:  "application/json",
		Headers: whitelistHeaders(headers,
			"X-GitHub-Event", "X-GitHub-Delivery", "X-GitHub-Hook-ID", "User-Agent", "Content-Type"),
		Data: json.RawMessage(body),
	}
	s.respondIngest(w, r, ev)
}

func githubSubject(p githubPayload) string {
	switch {
	case p.Repository.FullName != "":
		return "repo:" + p.Repository.FullName
	case p.Organization.Login != "":
		return "org:" + p.Organization.Login
	default:
		return ""
	}
}
