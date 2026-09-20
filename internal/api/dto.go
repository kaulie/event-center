package api

import "github.com/kaulie/event-center/internal/model"

// The types in this file are the wire shapes of the HTTP API. The swag
// annotations on the handlers reference them, so the generated contract
// (api/swagger.json) is derived from the same types the handlers actually
// encode and decode — the annotations, not a hand written spec file, are the
// source of truth.

// ErrorResponse is the uniform error body. Every non-2xx response uses it.
type ErrorResponse struct {
	Error string `json:"error" example:"unknown source github"`
}

// SourceView is a source as exposed by the admin API: the stored row minus the
// secret, plus a flag saying whether a secret is configured.
type SourceView struct {
	model.Source
	HasSecret bool `json:"has_secret"`
}

// SourcesResponse is the body of GET /v1/sources.
type SourcesResponse struct {
	Sources []SourceView `json:"sources"`
}

// SourceUpsertRequest is the body of PUT /v1/sources/{id}. Omitted fields keep
// their previous value, so an existing secret survives an update that does not
// mention one.
type SourceUpsertRequest struct {
	Kind          string `json:"kind" example:"webhook"`
	Secret        string `json:"secret" example:"s3cr3t"`
	VerifyMode    string `json:"verify_mode" example:"hmac_sha256" enums:"hmac_sha256,bearer,none"`
	TypePrefix    string `json:"type_prefix" example:"cicd"`
	DefaultStream string `json:"default_stream" example:"cicd"`
	Enabled       *bool  `json:"enabled" example:"true"`
}

// SubscriptionView is a subscription minus its secret, plus a flag saying
// whether a secret is configured.
type SubscriptionView struct {
	model.Subscription
	HasSecret bool `json:"has_secret"`
}

// SubscriptionsResponse is the body of GET /v1/subscriptions.
type SubscriptionsResponse struct {
	Subscriptions []SubscriptionView `json:"subscriptions"`
}

// SubscriptionCreateRequest is the body of POST /v1/subscriptions.
type SubscriptionCreateRequest struct {
	ID              string   `json:"id" example:"sub_01J8Q7ZC4K9W2M3N4P5Q6R7S"`
	Name            string   `json:"name" example:"ci-on-main-push"`
	Stream          string   `json:"stream" example:"github"`
	TypeFilters     []string `json:"type_filters" example:"github.push"`
	ProviderFilters []string `json:"provider_filters" example:"github"`
	SubjectPattern  string   `json:"subject_pattern" example:"repo:kaulie/*"`
	Delivery        string   `json:"delivery" example:"push" enums:"push,pull"`
	Endpoint        string   `json:"endpoint" example:"https://ci.example.com/hooks/event-center"`
	Secret          string   `json:"secret"`
}

// SubscriptionCreateResponse is the body of a successful POST /v1/subscriptions.
// A generated secret is only ever returned here (and only once), hence
// secret_generated.
type SubscriptionCreateResponse struct {
	Subscription    SubscriptionView `json:"subscription"`
	HasSecret       bool             `json:"has_secret"`
	Secret          string           `json:"secret,omitempty"`
	SecretGenerated bool             `json:"secret_generated,omitempty"`
}

// StreamsResponse is the body of GET /v1/streams: every known stream with its
// stream_seq head, plus the global seq head.
type StreamsResponse struct {
	Streams   map[string]int64 `json:"streams"`
	GlobalSeq int64            `json:"global_seq"`
	StreamAll string           `json:"stream_all" example:"all"`
}

// AckRequest is the body of POST /v1/subscriptions/{id}/ack.
type AckRequest struct {
	Cursor *int64 `json:"cursor" example:"100"`
}

// DeliveriesResponse is the body of GET /v1/deliveries (DLQ included).
type DeliveriesResponse struct {
	Deliveries []model.Delivery `json:"deliveries"`
}

// RequeueRequest is the body of POST /v1/deliveries/requeue.
type RequeueRequest struct {
	SubscriptionID string `json:"subscription_id"`
	Status         string `json:"status" example:"dead" enums:"pending,inflight,delivered,dead"`
	Limit          int    `json:"limit" example:"100"`
}

// RequeueResponse is the body of POST /v1/deliveries/requeue.
type RequeueResponse struct {
	Requeued int64 `json:"requeued" example:"3"`
}
