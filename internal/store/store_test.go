package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func append(t *testing.T, st *store.Store, ev *model.Event) *model.Event {
	t.Helper()
	stored, dup, err := st.AppendEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if dup {
		t.Fatalf("unexpected duplicate for %s", ev.Type)
	}
	return stored
}

func TestAppendEventAssignsMonotonicSequences(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		ev := append(t, st, &model.Event{
			Provider: "github",
			Type:     "github.push",
			Stream:   "github",
			Data:     []byte(`{"n":1}`),
		})
		if ev.Seq != int64(i+1) {
			t.Fatalf("global seq = %d, want %d", ev.Seq, i+1)
		}
		if ev.StreamSeq != int64(i+1) {
			t.Fatalf("stream seq = %d, want %d", ev.StreamSeq, i+1)
		}
		if ev.ID == "" {
			t.Fatal("event id must be generated")
		}
	}

	// A different stream gets its own independent stream sequence but shares
	// the global one.
	other := append(t, st, &model.Event{
		Provider: "k8s", Type: "k8s.event.warning", Stream: "k8s", Data: []byte(`{}`),
	})
	if other.StreamSeq != 1 {
		t.Fatalf("k8s stream seq = %d, want 1", other.StreamSeq)
	}
	if other.Seq != 6 {
		t.Fatalf("k8s global seq = %d, want 6", other.Seq)
	}

	// Seq must be strictly increasing in insertion order.
	res, err := st.ListEvents(ctx, "all", 0, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(res.Events) != 6 {
		t.Fatalf("got %d events, want 6", len(res.Events))
	}
	for i := 1; i < len(res.Events); i++ {
		if res.Events[i].Seq <= res.Events[i-1].Seq {
			t.Fatalf("seq not increasing: %d then %d", res.Events[i-1].Seq, res.Events[i].Seq)
		}
	}
}

func TestAppendEventDeduplicatesByProviderAndKey(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	first := append(t, st, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github",
		DedupeKey: "delivery-1", Data: []byte(`{"a":1}`),
	})

	again, duplicate, err := st.AppendEvent(ctx, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github",
		DedupeKey: "delivery-1", Data: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("append duplicate: %v", err)
	}
	if !duplicate {
		t.Fatal("second append must be reported as a duplicate")
	}
	if again.Seq != first.Seq {
		t.Fatalf("duplicate returned seq %d, want %d", again.Seq, first.Seq)
	}

	// A different provider may reuse the same delivery id.
	other := append(t, st, &model.Event{
		Provider: "gitlab", Type: "gitlab.push", Stream: "gitlab",
		DedupeKey: "delivery-1", Data: []byte(`{}`),
	})
	if other.Seq == first.Seq {
		t.Fatal("dedupe key must be scoped per provider")
	}
}

func TestAppendEventWithoutDedupeKeyNeverDeduplicates(t *testing.T) {
	st := newStore(t)
	for i := 0; i < 3; i++ {
		append(t, st, &model.Event{
			Provider: "github", Type: "github.ping", Stream: "github", Data: []byte(`{}`),
		})
	}
	res, err := st.ListEvents(context.Background(), "github", 0, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(res.Events))
	}
}

func TestListEventsPaginationUsesCursor(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		append(t, st, &model.Event{
			Provider: "github", Type: "github.push", Stream: "github", Data: []byte(`{}`),
		})
	}

	page1, err := st.ListEvents(ctx, "github", 0, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Events) != 2 || !page1.HasMore {
		t.Fatalf("page1 = %d events, hasMore=%v", len(page1.Events), page1.HasMore)
	}

	page2, err := st.ListEvents(ctx, "github", page1.NextCursor, 10)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Events) != 3 {
		t.Fatalf("page2 = %d events, want 3", len(page2.Events))
	}
	for _, ev := range page2.Events {
		if ev.StreamSeq <= page1.NextCursor {
			t.Fatalf("page2 contains event %d at or before cursor %d", ev.StreamSeq, page1.NextCursor)
		}
	}

	empty, err := st.ListEvents(ctx, "github", page2.NextCursor, 10)
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if len(empty.Events) != 0 || empty.NextCursor != page2.NextCursor {
		t.Fatalf("expected empty page keeping cursor %d, got %+v", page2.NextCursor, empty)
	}
}

func TestAppendEventNormalizesEnvelope(t *testing.T) {
	st := newStore(t)
	ev := append(t, st, &model.Event{
		Provider: "generic",
		Type:     "generic.thing.happened",
		Data:     []byte(`not json`),
	})
	if ev.Stream != model.DefaultStream {
		t.Fatalf("stream = %q, want %q", ev.Stream, model.DefaultStream)
	}
	if ev.DataType != "application/json" {
		t.Fatalf("data_type = %q", ev.DataType)
	}
	if string(ev.Data) != `"not json"` {
		t.Fatalf("non JSON data should be preserved as a string, got %s", ev.Data)
	}
	if ev.ReceivedAt.IsZero() {
		t.Fatal("received_at must be set")
	}
}

func TestAppendEventRequiresProviderAndType(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if _, _, err := st.AppendEvent(ctx, &model.Event{Type: "github.push"}); err == nil {
		t.Fatal("expected error when provider is missing")
	}
	if _, _, err := st.AppendEvent(ctx, &model.Event{Provider: "github"}); err == nil {
		t.Fatal("expected error when type is missing")
	}
}

func TestSubscriptionsMatchFilters(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.CreateSubscription(ctx, &model.Subscription{
		ID: "sub-pr", Name: "pr watcher", Stream: "github",
		TypeFilters:     []string{"github.pull_request.*"},
		ProviderFilters: []string{"github"},
		SubjectPattern:  "repo:kaulie/*",
		Delivery:        model.DeliveryPush,
		Endpoint:        "http://example.test/hook",
	}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if err := st.CreateSubscription(ctx, &model.Subscription{
		ID: "sub-all", Name: "everything", Stream: model.StreamAll,
		Delivery: model.DeliveryPull,
	}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	prEvent := append(t, st, &model.Event{
		Provider: "github", Type: "github.pull_request.opened", Stream: "github",
		Subject: "repo:kaulie/event-center", Data: []byte(`{}`),
	})
	pushSubs, err := st.MatchingSubscriptions(ctx, prEvent, model.DeliveryPush)
	if err != nil {
		t.Fatalf("match push: %v", err)
	}
	if len(pushSubs) != 1 || pushSubs[0].ID != "sub-pr" {
		t.Fatalf("push matches = %+v, want sub-pr", pushSubs)
	}

	pushEvent := append(t, st, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github",
		Subject: "repo:kaulie/other", Data: []byte(`{}`),
	})
	pushSubs, err = st.MatchingSubscriptions(ctx, pushEvent, model.DeliveryPush)
	if err != nil {
		t.Fatalf("match push: %v", err)
	}
	if len(pushSubs) != 0 {
		t.Fatalf("push event should not match the type filter, got %+v", pushSubs)
	}

	pullSubs, err := st.MatchingSubscriptions(ctx, pushEvent, model.DeliveryPull)
	if err != nil {
		t.Fatalf("match pull: %v", err)
	}
	if len(pullSubs) != 1 || pullSubs[0].ID != "sub-all" {
		t.Fatalf("pull matches = %+v, want sub-all", pullSubs)
	}
}

func TestCursorOnlyMovesForward(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.CreateSubscription(ctx, &model.Subscription{
		ID: "sub-1", Stream: model.StreamAll, Delivery: model.DeliveryPull,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.AdvanceCursor(ctx, "sub-1", 10); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := st.AdvanceCursor(ctx, "sub-1", 4); err != nil {
		t.Fatalf("advance back: %v", err)
	}
	sub, err := st.GetSubscription(ctx, "sub-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sub.Cursor != 10 {
		t.Fatalf("cursor = %d, want 10", sub.Cursor)
	}
}

func TestPurgeOlderThanKeepsRecentEvents(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	oldEvent := append(t, st, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github", Data: []byte(`{}`),
		ReceivedAt: now.Add(-48 * time.Hour),
	})
	fresh := append(t, st, &model.Event{
		Provider: "github", Type: "github.push", Stream: "github", Data: []byte(`{}`),
		ReceivedAt: now.Add(-1 * time.Minute),
	})

	n, err := st.PurgeOlderThan(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d events, want 1", n)
	}
	if _, err := st.GetEvent(ctx, fresh.ID); err != nil {
		t.Fatalf("fresh event should survive: %v", err)
	}
	if _, err := st.GetEvent(ctx, oldEvent.ID); err == nil {
		t.Fatal("old event should have been purged")
	}
}
