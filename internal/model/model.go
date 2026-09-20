// Package model defines the core domain types of the event center.
package model

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kaulie/event-center/internal/idgen"
)

// DefaultStream is used when an event does not specify a stream.
const DefaultStream = "default"

// Normalize fills in defaults and makes the envelope safe to persist. It
// mutates the event in place and returns an error when required fields are
// missing.
func (e *Event) Normalize(now time.Time) error {
	if e.Provider == "" {
		return ErrInvalid("provider is required")
	}
	if e.Type == "" {
		return ErrInvalid("type is required")
	}
	if e.ID == "" {
		e.ID = idgen.NewID("evt")
	}
	if e.Stream == "" {
		e.Stream = DefaultStream
	}
	if e.DataType == "" {
		e.DataType = "application/json"
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = now.UTC()
	} else {
		e.ReceivedAt = e.ReceivedAt.UTC()
	}
	if e.SourceTime != nil {
		t := e.SourceTime.UTC()
		e.SourceTime = &t
	}
	if len(e.Data) == 0 {
		e.Data = json.RawMessage("null")
	}
	if !json.Valid(e.Data) {
		// Preserve the payload verbatim as a JSON string instead of rejecting
		// the event: retaining is more valuable than strictness.
		encoded, err := json.Marshal(string(e.Data))
		if err != nil {
			return ErrInvalid("data is not valid JSON: " + err.Error())
		}
		e.Data = encoded
	}
	if e.ID == "" || strings.TrimSpace(e.Stream) == "" {
		return ErrInvalid("stream must not be blank")
	}
	return nil
}

// Error is a validation error raised by Normalize.
type Error struct{ msg string }

// ErrInvalid builds a validation error.
func ErrInvalid(msg string) error { return &Error{msg: msg} }

func (e *Error) Error() string { return e.msg }

// Delivery modes for a subscription.
const (
	DeliveryPush = "push"
	DeliveryPull = "pull"
)

// Subscription status.
const (
	StatusActive   = "active"
	StatusPaused   = "paused"
	StatusDisabled = "disabled"
)

// Delivery record status.
const (
	DeliveryStatusPending   = "pending"
	DeliveryStatusInflight  = "inflight"
	DeliveryStatusDelivered = "delivered"
	DeliveryStatusDead      = "dead"
)

// Verify modes for an event source.
const (
	VerifyHMACSHA256 = "hmac_sha256"
	VerifyBearer     = "bearer"
	VerifyNone       = "none"
)

// StreamAll is the pseudo stream that matches every stream.
const StreamAll = "all"

// Event is the normalized envelope persisted for every injected event.
//
// Seq is a globally monotonically increasing sequence assigned by the store at
// commit time; StreamSeq is monotonically increasing inside a single stream.
// Together they let downstream consumers either replay the whole log (Seq) or
// tail one stream in strict order (StreamSeq).
type Event struct {
	Seq        int64             `json:"seq"`
	StreamSeq  int64             `json:"stream_seq"`
	ID         string            `json:"id"`
	Stream     string            `json:"stream"`
	Provider   string            `json:"provider"`
	Type       string            `json:"type"`
	Subject    string            `json:"subject,omitempty"`
	SourceTime *time.Time        `json:"source_time,omitempty"`
	ReceivedAt time.Time         `json:"received_at"`
	DedupeKey  string            `json:"dedupe_key,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	DataType   string            `json:"data_type"`
	// swaggertype keeps swag from choking on json.RawMessage when it builds the
	// contract from the annotations (the tag is inert at runtime).
	Data json.RawMessage `json:"data" swaggertype:"object"`
}

// Source describes an external event producer (github, k8s, ...).
type Source struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	Secret        string    `json:"-"`
	VerifyMode    string    `json:"verify_mode"`
	TypePrefix    string    `json:"type_prefix"`
	DefaultStream string    `json:"default_stream"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
}

// Subscription is a downstream consumer of the event log.
type Subscription struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Stream          string    `json:"stream"`
	TypeFilters     []string  `json:"type_filters"`
	ProviderFilters []string  `json:"provider_filters"`
	SubjectPattern  string    `json:"subject_pattern,omitempty"`
	Delivery        string    `json:"delivery"`
	Endpoint        string    `json:"endpoint,omitempty"`
	Secret          string    `json:"-"`
	Status          string    `json:"status"`
	Cursor          int64     `json:"cursor"`
	CreatedAt       time.Time `json:"created_at"`
}

// Delivery tracks one (subscription, event) push attempt.
type Delivery struct {
	SubscriptionID string     `json:"subscription_id"`
	EventSeq       int64      `json:"event_seq"`
	Attempt        int        `json:"attempt"`
	Status         string     `json:"status"`
	LastError      string     `json:"last_error,omitempty"`
	NextRetryAt    *time.Time `json:"next_retry_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// IngestRequest is the generic injection payload accepted by
// POST /v1/ingest/{source}.
type IngestRequest struct {
	Type       string            `json:"type"`
	Provider   string            `json:"provider"`
	Stream     string            `json:"stream"`
	Subject    string            `json:"subject"`
	SourceTime *time.Time        `json:"source_time"`
	DedupeKey  string            `json:"dedupe_key"`
	DataType   string            `json:"data_type"`
	Headers    map[string]string `json:"headers"`
	// swaggertype: see the note on Event.Data.
	Data json.RawMessage `json:"data" swaggertype:"object"`
}

// IngestResult is returned by every ingest endpoint.
type IngestResult struct {
	Event     *Event `json:"event"`
	Duplicate bool   `json:"duplicate"`
}

// IngestResponse wraps the result with a status field for HTTP responses.
type IngestResponse struct {
	Status    string `json:"status"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Event     *Event `json:"event,omitempty"`
}

// ListResult is the response of the pull API.
type ListResult struct {
	Events     []Event `json:"events"`
	NextCursor int64   `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}
