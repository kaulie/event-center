// Package dispatch pushes events to downstream subscriber webhooks.
//
// Delivery is at-least-once: a batch is retried with exponential backoff until
// the subscriber answers 2xx, and is parked in the dead letter state once the
// attempt budget is exhausted. Consumers therefore must be idempotent, which
// the event id and dedupe key make possible.
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/kaulie/event-center/internal/metrics"
	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/store"
)

// Config tunes the dispatcher.
type Config struct {
	BatchSize    int
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	PollInterval time.Duration
	Timeout      time.Duration
	Concurrency  int
	UserAgent    string
}

// Payload is the JSON body posted to subscriber endpoints.
type Payload struct {
	Subscription string        `json:"subscription"`
	DeliveryID   string        `json:"delivery_id"`
	Attempt      int           `json:"attempt"`
	Count        int           `json:"count"`
	Events       []model.Event `json:"events"`
}

// Dispatcher drains the delivery queue of every push subscription.
type Dispatcher struct {
	store   *store.Store
	cfg     Config
	log     *slog.Logger
	metrics *metrics.Registry
	client  *http.Client
	now     func() time.Time
}

// New builds a Dispatcher.
func New(st *store.Store, cfg Config, reg *metrics.Registry, log *slog.Logger) *Dispatcher {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 6
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 10 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "event-center-dispatcher/1"
	}
	return &Dispatcher{
		store:   st,
		cfg:     cfg,
		log:     log,
		metrics: reg,
		client:  &http.Client{Timeout: cfg.Timeout},
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Run drives the delivery loop until the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	if recovered, err := d.store.RecoverInflight(ctx); err != nil {
		d.log.Error("recover in-flight deliveries failed", "error", err)
	} else if recovered > 0 {
		d.log.Info("requeued in-flight deliveries", "count", recovered)
	}

	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.DrainOnce(ctx); err != nil {
				d.log.Error("delivery pass failed", "error", err)
			}
		}
	}
}

// DrainOnce runs a single delivery pass; it is exported so tests can drive the
// dispatcher deterministically.
func (d *Dispatcher) DrainOnce(ctx context.Context) error {
	subs, err := d.store.DueSubscriptionIDs(ctx, d.cfg.Concurrency*4)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}

	sem := make(chan struct{}, d.cfg.Concurrency)
	done := make(chan struct{}, len(subs))
	for _, id := range subs {
		sem <- struct{}{}
		go func(subscriptionID string) {
			defer func() { <-sem; done <- struct{}{} }()
			if err := d.deliverSubscription(ctx, subscriptionID); err != nil {
				d.log.Error("delivery failed", "subscription", subscriptionID, "error", err)
			}
		}(id)
	}
	for range subs {
		<-done
	}
	return nil
}

func (d *Dispatcher) deliverSubscription(ctx context.Context, subscriptionID string) error {
	sub, err := d.store.GetSubscription(ctx, subscriptionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if sub.Status != model.StatusActive || sub.Delivery != model.DeliveryPush || sub.Endpoint == "" {
		return nil
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		deliveries, events, err := d.store.LeaseDeliveries(ctx, subscriptionID, d.cfg.BatchSize)
		if err != nil {
			return err
		}
		if len(deliveries) == 0 {
			return nil
		}
		if len(events) != len(deliveries) {
			// Events were purged between enqueue and delivery: park the
			// orphans so the queue does not spin.
			seqs := make([]int64, 0, len(deliveries))
			for _, dl := range deliveries {
				seqs = append(seqs, dl.EventSeq)
			}
			if err := d.store.MarkFailed(ctx, subscriptionID, seqs,
				"event no longer retained", nil, true); err != nil {
				return err
			}
			continue
		}
		d.send(ctx, sub, deliveries, events)
	}
}
