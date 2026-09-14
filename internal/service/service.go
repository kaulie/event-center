// Package service contains the application use cases that tie the store, the
// filters and the metrics together.
package service

import (
	"context"
	"log/slog"

	"github.com/kaulie/event-center/internal/metrics"
	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/store"
)

// Service is the ingest/consume facade used by the HTTP layer.
type Service struct {
	store   *store.Store
	metrics *metrics.Registry
	log     *slog.Logger
}

// New builds a Service.
func New(st *store.Store, reg *metrics.Registry, log *slog.Logger) *Service {
	return &Service{store: st, metrics: reg, log: log}
}

// Store exposes the underlying store for administrative handlers.
func (s *Service) Store() *store.Store { return s.store }

// Ingest persists an event and fans it out to the matching push
// subscriptions. It reports whether the event was a duplicate of an earlier
// one (same provider + dedupe key), in which case nothing was re-published.
func (s *Service) Ingest(ctx context.Context, ev *model.Event) (*model.Event, bool, error) {
	stored, duplicate, err := s.store.AppendEvent(ctx, ev)
	if err != nil {
		s.metrics.Inc("eventd_ingest_errors_total", map[string]string{"provider": ev.Provider}, 1)
		return nil, false, err
	}
	if duplicate {
		s.metrics.Inc("eventd_events_deduplicated_total", map[string]string{"provider": stored.Provider}, 1)
		return stored, true, nil
	}

	s.metrics.Inc("eventd_events_ingested_total",
		map[string]string{"provider": stored.Provider, "stream": stored.Stream}, 1)
	s.metrics.Inc("eventd_events_ingested_by_type_total",
		map[string]string{"type": stored.Type}, 1)

	subs, err := s.store.MatchingSubscriptions(ctx, stored, model.DeliveryPush)
	if err != nil {
		return stored, false, err
	}
	for _, sub := range subs {
		if err := s.store.EnqueueDeliveries(ctx, sub.ID, []int64{stored.Seq}); err != nil {
			s.log.Error("enqueue delivery failed",
				"subscription", sub.ID, "seq", stored.Seq, "error", err)
			continue
		}
		s.metrics.Inc("eventd_deliveries_enqueued_total",
			map[string]string{"subscription": sub.ID}, 1)
	}
	return stored, false, nil
}
