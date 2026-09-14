package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/kaulie/event-center/internal/idgen"
	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/verify"
)

func (d *Dispatcher) send(ctx context.Context, sub *model.Subscription, deliveries []model.Delivery, events []model.Event) {
	seqs := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		seqs = append(seqs, dl.EventSeq)
	}
	// LeaseDeliveries bumped attempt in the database; the in-memory copies still
	// hold the previous value, so the real attempt number is +1.
	attempt := deliveries[0].Attempt + 1
	deliveryID := idgen.NewID("dlv")

	body, err := json.Marshal(Payload{
		Subscription: sub.ID,
		DeliveryID:   deliveryID,
		Attempt:      attempt,
		Count:        len(events),
		Events:       events,
	})
	if err != nil {
		d.log.Error("encode push payload failed", "subscription", sub.ID, "error", err)
		_ = d.store.MarkFailed(ctx, sub.ID, seqs, "encode payload: "+err.Error(), nil, true)
		return
	}

	status, errMsg, retryable := d.post(ctx, sub, body, deliveryID, attempt)
	if status == 0 && errMsg == "" {
		// Success.
		if err := d.store.MarkDelivered(ctx, sub.ID, seqs); err != nil {
			d.log.Error("mark delivered failed", "subscription", sub.ID, "error", err)
			return
		}
		d.metrics.Inc("eventd_push_delivered_total", map[string]string{"subscription": sub.ID}, 1)
		d.metrics.Inc("eventd_push_events_delivered_total", map[string]string{"subscription": sub.ID}, int64(len(events)))
		return
	}

	dead := !retryable || attempt >= d.cfg.MaxAttempts
	var nextRetry *time.Time
	if !dead {
		at := d.now().Add(backoff(attempt, d.cfg.BaseBackoff, d.cfg.MaxBackoff))
		nextRetry = &at
	}
	if err := d.store.MarkFailed(ctx, sub.ID, seqs, errMsg, nextRetry, dead); err != nil {
		d.log.Error("mark failed errored", "subscription", sub.ID, "error", err)
	}
	if dead {
		d.metrics.Inc("eventd_push_dead_total", map[string]string{"subscription": sub.ID}, int64(len(seqs)))
		d.log.Warn("deliveries parked in dead letter queue",
			"subscription", sub.ID, "count", len(seqs), "attempt", attempt, "error", errMsg)
		return
	}
	d.metrics.Inc("eventd_push_retry_total", map[string]string{"subscription": sub.ID}, int64(len(seqs)))
}

// post performs one HTTP push. It returns a zero status without error message
// on success, otherwise the HTTP status (0 for transport errors) and a
// human readable reason. retryable reports whether another attempt makes sense.
func (d *Dispatcher) post(ctx context.Context, sub *model.Subscription, body []byte, deliveryID string, attempt int) (int, string, bool) {
	reqCtx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error(), false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", d.cfg.UserAgent)
	req.Header.Set("X-EventCenter-Subscription", sub.ID)
	req.Header.Set("X-EventCenter-Delivery", deliveryID)
	req.Header.Set("X-EventCenter-Attempt", fmt.Sprint(attempt))
	req.Header.Set("X-EventCenter-Timestamp", fmt.Sprint(d.now().Unix()))
	if sub.Secret != "" {
		req.Header.Set("X-EventCenter-Signature", verify.SignHMACSHA256(sub.Secret, body))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err.Error(), true
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0, "", true
	}
	msg := fmt.Sprintf("endpoint responded %d", resp.StatusCode)
	retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusRequestTimeout
	return resp.StatusCode, msg, retryable
}

// backoff returns base * 2^(attempt-1) capped at max with a small jitter.
func backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := math.Min(float64(attempt-1), 10)
	d := time.Duration(float64(base) * math.Pow(2, shift))
	if d > max || d <= 0 {
		d = max
	}
	jitter := time.Duration(time.Now().UnixNano() % int64(d/10+1))
	return d + jitter
}
