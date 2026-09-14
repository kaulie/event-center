package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/kaulie/event-center/internal/filter"
	"github.com/kaulie/event-center/internal/model"
)

const subColumns = `id, name, stream, type_filters, provider_filters, subject_pattern,
	delivery, endpoint, secret, status, cursor, created_at`

func scanSubscription(sc rowScanner) (*model.Subscription, error) {
	var (
		sub                      model.Subscription
		typeFilters, provFilters string
		createdAt                string
	)
	if err := sc.Scan(&sub.ID, &sub.Name, &sub.Stream, &typeFilters, &provFilters,
		&sub.SubjectPattern, &sub.Delivery, &sub.Endpoint, &sub.Secret, &sub.Status,
		&sub.Cursor, &createdAt); err != nil {
		return nil, err
	}
	sub.TypeFilters = []string{}
	sub.ProviderFilters = []string{}
	_ = json.Unmarshal([]byte(typeFilters), &sub.TypeFilters)
	_ = json.Unmarshal([]byte(provFilters), &sub.ProviderFilters)
	if t, err := parseTime(createdAt); err == nil {
		sub.CreatedAt = t
	}
	return &sub, nil
}

// CreateSubscription persists a new subscription.
func (s *Store) CreateSubscription(ctx context.Context, sub *model.Subscription) error {
	if sub.ID == "" {
		return model.ErrInvalid("subscription id is required")
	}
	if sub.Name == "" {
		sub.Name = sub.ID
	}
	if sub.Delivery == "" {
		sub.Delivery = model.DeliveryPush
	}
	if sub.Status == "" {
		sub.Status = model.StatusActive
	}
	if sub.Stream == "" {
		sub.Stream = model.StreamAll
	}
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = time.Now().UTC()
	}
	if sub.Delivery == model.DeliveryPush && sub.Endpoint == "" {
		return model.ErrInvalid("push subscription requires an endpoint")
	}
	typeFilters, _ := json.Marshal(nonNil(sub.TypeFilters))
	provFilters, _ := json.Marshal(nonNil(sub.ProviderFilters))

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscriptions (id, name, stream, type_filters, provider_filters,
		   subject_pattern, delivery, endpoint, secret, status, cursor, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, sub.Name, sub.Stream, string(typeFilters), string(provFilters),
		sub.SubjectPattern, sub.Delivery, sub.Endpoint, sub.Secret, sub.Status,
		sub.Cursor, formatTime(sub.CreatedAt))
	return err
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// GetSubscription loads one subscription by id.
func (s *Store) GetSubscription(ctx context.Context, id string) (*model.Subscription, error) {
	sub, err := scanSubscription(s.db.QueryRowContext(ctx,
		`SELECT `+subColumns+` FROM subscriptions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sub, err
}

// ListSubscriptions returns subscriptions, optionally filtered by delivery mode.
func (s *Store) ListSubscriptions(ctx context.Context, delivery string) ([]model.Subscription, error) {
	query := `SELECT ` + subColumns + ` FROM subscriptions`
	args := []any{}
	if delivery != "" {
		query += ` WHERE delivery = ?`
		args = append(args, delivery)
	}
	query += ` ORDER BY created_at, id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Subscription{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

// DeleteSubscription removes a subscription and its delivery queue.
func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE subscription_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSubscriptionStatus updates the lifecycle status of a subscription.
func (s *Store) SetSubscriptionStatus(ctx context.Context, id, status string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdvanceCursor moves a subscription cursor forward; it never moves backwards
// so a late ack cannot rewind a consumer.
func (s *Store) AdvanceCursor(ctx context.Context, id string, cursor int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET cursor = ? WHERE id = ? AND cursor < ?`, cursor, id, cursor)
	if err != nil {
		return err
	}
	if _, err := res.RowsAffected(); err != nil {
		return err
	}
	return nil
}

// MatchingSubscriptions returns the subscriptions of the given delivery mode
// that address the event.
//
// Paused subscriptions are included on purpose: fan-out is unconditional and
// the dispatcher is what gates delivery on status, so pausing buffers a
// backlog instead of dropping events, and resuming drains it in order.
func (s *Store) MatchingSubscriptions(ctx context.Context, ev *model.Event, delivery string) ([]model.Subscription, error) {
	query := `SELECT ` + subColumns + ` FROM subscriptions
	          WHERE status <> ? AND (stream = ? OR stream = ?)`
	args := []any{model.StatusDisabled, ev.Stream, model.StreamAll}
	if delivery != "" {
		query += ` AND delivery = ?`
		args = append(args, delivery)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Subscription{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		if filter.Matches(sub, ev) {
			out = append(out, *sub)
		}
	}
	return out, rows.Err()
}
