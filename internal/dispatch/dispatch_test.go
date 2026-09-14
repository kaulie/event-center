package dispatch_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kaulie/event-center/internal/dispatch"
	"github.com/kaulie/event-center/internal/metrics"
	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/service"
	"github.com/kaulie/event-center/internal/store"
	"github.com/kaulie/event-center/internal/verify"
)

// receiver is a downstream webhook that records every delivery it receives.
type receiver struct {
	mu       sync.Mutex
	requests []receivedRequest
	status   int
	failures int // number of leading requests that should fail
}

type receivedRequest struct {
	payload   dispatch.Payload
	body      []byte
	signature string
	attempt   string
	subID     string
	delivery  string
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = append(r.requests, receivedRequest{
		body:      body,
		signature: req.Header.Get("X-EventCenter-Signature"),
		attempt:   req.Header.Get("X-EventCenter-Attempt"),
		subID:     req.Header.Get("X-EventCenter-Subscription"),
		delivery:  req.Header.Get("X-EventCenter-Delivery"),
	})

	var payload dispatch.Payload
	_ = json.Unmarshal(body, &payload)
	r.requests[len(r.requests)-1].payload = payload

	if r.failures > 0 {
		r.failures--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *receiver) last() receivedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[len(r.requests)-1]
}

type harness struct {
	store    *store.Store
	svc      *service.Service
	dispatch *dispatch.Dispatcher
	metrics  *metrics.Registry
}

func newHarness(t *testing.T, cfg dispatch.Config) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "dispatch.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	reg := metrics.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(st, reg, log)
	return &harness{
		store:    st,
		svc:      svc,
		dispatch: dispatch.New(st, cfg, reg, log),
		metrics:  reg,
	}
}

func (h *harness) ingest(t *testing.T, ev *model.Event) *model.Event {
	t.Helper()
	stored, dup, err := h.svc.Ingest(context.Background(), ev)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if dup {
		t.Fatal("unexpected duplicate")
	}
	return stored
}

func subscribePush(t *testing.T, h *harness, id, endpoint, secret string, filters []string) {
	t.Helper()
	if err := h.store.CreateSubscription(context.Background(), &model.Subscription{
		ID: id, Name: id, Stream: "github",
		TypeFilters: filters,
		Delivery:    model.DeliveryPush,
		Endpoint:    endpoint,
		Secret:      secret,
	}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
}

func TestDispatcherPushesMatchingEventsInOrder(t *testing.T) {
	rec := &receiver{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h := newHarness(t, dispatch.Config{BatchSize: 10, MaxAttempts: 3, PollInterval: time.Millisecond})
	subscribePush(t, h, "sub-ci", srv.URL, "push-secret", []string{"github.push"})

	first := h.ingest(t, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github",
		Subject: "repo:kaulie/event-center",
		Data:    []byte(`{"ref":"refs/heads/main"}`),
	})
	// A non-matching event must not be pushed at all.
	h.ingest(t, &model.Event{
		Provider: "github", Type: "github.pull_request.opened", Stream: "github",
		Data: []byte(`{}`),
	})
	second := h.ingest(t, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github",
		Subject: "repo:kaulie/event-center",
		Data:    []byte(`{"ref":"refs/heads/dev"}`),
	})

	if err := h.dispatch.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if rec.count() != 1 {
		t.Fatalf("received %d requests, want 1 (batched)", rec.count())
	}
	got := rec.last()
	if got.payload.Subscription != "sub-ci" {
		t.Fatalf("payload subscription = %q", got.payload.Subscription)
	}
	if got.payload.Count != 2 || len(got.payload.Events) != 2 {
		t.Fatalf("payload count = %d, events = %d, want 2", got.payload.Count, len(got.payload.Events))
	}
	if got.payload.Events[0].Seq != first.Seq || got.payload.Events[1].Seq != second.Seq {
		t.Fatalf("events not in sequence order: %+v", got.payload.Events)
	}
	if got.delivery == "" || got.subID != "sub-ci" {
		t.Fatalf("missing delivery headers: %+v", got)
	}

	// The signature must verify against the subscription secret.
	if want := verify.SignHMACSHA256("push-secret", got.body); got.signature != want {
		t.Fatalf("signature = %q, want %q", got.signature, want)
	}
	if err := verify.HMACSHA256("push-secret", got.body, got.signature, ""); err != nil {
		t.Fatalf("payload signature does not verify: %v", err)
	}

	// Both deliveries are marked delivered and the cursor advanced.
	deliveries, err := h.store.ListDeliveries(context.Background(), "sub-ci", model.DeliveryStatusDelivered, 10)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("delivered rows = %d, want 2", len(deliveries))
	}
	sub, err := h.store.GetSubscription(context.Background(), "sub-ci")
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if sub.Cursor != second.Seq {
		t.Fatalf("cursor = %d, want %d", sub.Cursor, second.Seq)
	}

	// A second pass has nothing left to do.
	if err := h.dispatch.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain again: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("received %d requests after a second pass, want 1", rec.count())
	}
}

func TestDispatcherRetriesWithBackoffThenDeadLetters(t *testing.T) {
	rec := &receiver{failures: 100}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h := newHarness(t, dispatch.Config{
		BatchSize: 5, MaxAttempts: 3,
		BaseBackoff: 5 * time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		PollInterval: time.Millisecond,
	})
	subscribePush(t, h, "sub-flaky", srv.URL, "", []string{"**"})

	h.ingest(t, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github", Data: []byte(`{}`),
	})

	ctx := context.Background()
	for i := 0; i < 6; i++ {
		if err := h.dispatch.DrainOnce(ctx); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rec.count() != 3 {
		t.Fatalf("endpoint called %d times, want 3 (max attempts)", rec.count())
	}

	dead, err := h.store.ListDeliveries(ctx, "sub-flaky", model.DeliveryStatusDead, 10)
	if err != nil {
		t.Fatalf("list dead: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead rows = %d, want 1", len(dead))
	}
	if dead[0].Attempt != 3 {
		t.Fatalf("attempts = %d, want 3", dead[0].Attempt)
	}

	// The subscription cursor must not move for undelivered events.
	sub, err := h.store.GetSubscription(ctx, "sub-flaky")
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if sub.Cursor != 0 {
		t.Fatalf("cursor = %d, want 0", sub.Cursor)
	}

	// Requeue the DLQ and let the endpoint recover.
	requeued, err := h.store.RequeueDeliveries(ctx, "sub-flaky", model.DeliveryStatusDead, 10)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("requeued = %d, want 1", requeued)
	}
	rec.mu.Lock()
	rec.failures = 0
	rec.mu.Unlock()

	if err := h.dispatch.DrainOnce(ctx); err != nil {
		t.Fatalf("drain after requeue: %v", err)
	}
	delivered, err := h.store.ListDeliveries(ctx, "sub-flaky", model.DeliveryStatusDelivered, 10)
	if err != nil {
		t.Fatalf("list delivered: %v", err)
	}
	if len(delivered) != 1 {
		t.Fatalf("delivered rows = %d, want 1", len(delivered))
	}
}

func TestDispatcherStopsOnNonRetryableStatus(t *testing.T) {
	rec := &receiver{status: http.StatusBadRequest}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h := newHarness(t, dispatch.Config{BatchSize: 5, MaxAttempts: 5, PollInterval: time.Millisecond})
	subscribePush(t, h, "sub-400", srv.URL, "", []string{"**"})

	h.ingest(t, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github", Data: []byte(`{}`),
	})

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := h.dispatch.DrainOnce(ctx); err != nil {
			t.Fatalf("drain: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec.count() != 1 {
		t.Fatalf("endpoint called %d times, want 1: a 4xx must not be retried", rec.count())
	}
	dead, err := h.store.ListDeliveries(ctx, "sub-400", model.DeliveryStatusDead, 10)
	if err != nil {
		t.Fatalf("list dead: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead rows = %d, want 1", len(dead))
	}
}

func TestDispatcherIgnoresPausedSubscriptions(t *testing.T) {
	rec := &receiver{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h := newHarness(t, dispatch.Config{BatchSize: 5, PollInterval: time.Millisecond})
	subscribePush(t, h, "sub-paused", srv.URL, "", []string{"**"})
	ctx := context.Background()
	if err := h.store.SetSubscriptionStatus(ctx, "sub-paused", model.StatusPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}

	h.ingest(t, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github", Data: []byte(`{}`),
	})
	if err := h.dispatch.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("paused subscription received %d requests, want 0", rec.count())
	}

	// Resuming delivers the backlog.
	if err := h.store.SetSubscriptionStatus(ctx, "sub-paused", model.StatusActive); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := h.dispatch.DrainOnce(ctx); err != nil {
		t.Fatalf("drain after resume: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("after resume got %d requests, want 1", rec.count())
	}
}
