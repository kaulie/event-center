package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/kaulie/event-center/internal/model"
)

// EnqueueDeliveries adds push work items for a subscription. Existing rows are
// left untouched, which makes fan-out idempotent.
func (s *Store) EnqueueDeliveries(ctx context.Context, subscriptionID string, seqs []int64) error {
	if len(seqs) == 0 {
		return nil
	}
	now := formatTime(time.Now().UTC())

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO deliveries (subscription_id, event_seq, attempt, status, last_error, updated_at)
		 VALUES (?, ?, 0, ?, '', ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, seq := range seqs {
		if _, err := stmt.ExecContext(ctx, subscriptionID, seq, model.DeliveryStatusPending, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecoverInflight returns in-flight rows to the pending queue. It runs at
// dispatcher start-up so work leased by a previous process is not lost.
func (s *Store) RecoverInflight(ctx context.Context) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := s.db.ExecContext(ctx,
		`UPDATE deliveries SET status = ?, updated_at = ? WHERE status = ?`,
		model.DeliveryStatusPending, formatTime(time.Now().UTC()), model.DeliveryStatusInflight)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DueSubscriptionIDs lists subscriptions that currently have deliverable work.
func (s *Store) DueSubscriptionIDs(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT subscription_id FROM deliveries
		 WHERE status = ? AND (next_retry_at IS NULL OR next_retry_at <= ?)
		 ORDER BY subscription_id LIMIT ?`,
		model.DeliveryStatusPending, formatTime(time.Now().UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LeaseDeliveries moves up to limit due rows of a subscription to in-flight and
// returns them together with the events they reference, so the dispatcher can
// send one batch.
func (s *Store) LeaseDeliveries(ctx context.Context, subscriptionID string, limit int) ([]model.Delivery, []model.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx,
		`SELECT subscription_id, event_seq, attempt, status, last_error, next_retry_at, updated_at
		 FROM deliveries
		 WHERE subscription_id = ? AND status = ?
		   AND (next_retry_at IS NULL OR next_retry_at <= ?)
		 ORDER BY event_seq ASC LIMIT ?`,
		subscriptionID, model.DeliveryStatusPending, formatTime(now), limit)
	if err != nil {
		return nil, nil, err
	}
	deliveries, err := scanDeliveries(rows)
	if err != nil {
		return nil, nil, err
	}
	if len(deliveries) == 0 {
		return nil, nil, nil
	}

	seqs := make([]int64, 0, len(deliveries))
	for _, d := range deliveries {
		seqs = append(seqs, d.EventSeq)
	}

	s.writeMu.Lock()
	err = func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")
		args := []any{model.DeliveryStatusInflight, formatTime(now), model.DeliveryStatusPending, subscriptionID}
		for _, seq := range seqs {
			args = append(args, seq)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE deliveries SET status = ?, attempt = attempt + 1, updated_at = ?
			 WHERE status = ? AND subscription_id = ? AND event_seq IN (`+placeholders+`)`, args...)
		if err != nil {
			return err
		}
		return tx.Commit()
	}()
	s.writeMu.Unlock()
	if err != nil {
		return nil, nil, err
	}

	events, err := s.eventsBySeq(ctx, seqs)
	if err != nil {
		return nil, nil, err
	}
	return deliveries, events, nil
}

func scanDeliveries(rows *sql.Rows) ([]model.Delivery, error) {
	out := []model.Delivery{}
	for rows.Next() {
		var (
			d         model.Delivery
			nextRetry sql.NullString
			updatedAt string
		)
		if err := rows.Scan(&d.SubscriptionID, &d.EventSeq, &d.Attempt, &d.Status,
			&d.LastError, &nextRetry, &updatedAt); err != nil {
			return nil, err
		}
		d.NextRetryAt = nullTime(nextRetry.String)
		if t, err := parseTime(updatedAt); err == nil {
			d.UpdatedAt = t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDelivered records a successful push and advances the subscription cursor
// past the delivered events.
func (s *Store) MarkDelivered(ctx context.Context, subscriptionID string, seqs []int64) error {
	if len(seqs) == 0 {
		return nil
	}
	now := formatTime(time.Now().UTC())
	args := []any{model.DeliveryStatusDelivered, now, subscriptionID}
	for _, seq := range seqs {
		args = append(args, seq)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`UPDATE deliveries SET status = ?, last_error = '', next_retry_at = NULL, updated_at = ?
		 WHERE subscription_id = ? AND event_seq IN (`+placeholders(len(seqs))+`)`, args...)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE subscriptions SET cursor = ? WHERE id = ? AND cursor < ?`,
		seqs[len(seqs)-1], subscriptionID, seqs[len(seqs)-1]); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkFailed reschedules failed deliveries with a backoff, or parks them in
// the dead letter state once the attempt budget is exhausted.
func (s *Store) MarkFailed(ctx context.Context, subscriptionID string, seqs []int64, errMsg string, nextRetry *time.Time, dead bool) error {
	if len(seqs) == 0 {
		return nil
	}
	status := model.DeliveryStatusPending
	var retry any
	if dead {
		status = model.DeliveryStatusDead
	} else if nextRetry != nil {
		retry = formatTime(*nextRetry)
	}
	if len(errMsg) > 1000 {
		errMsg = errMsg[:1000]
	}

	args := []any{status, errMsg, retry, formatTime(time.Now().UTC()), subscriptionID}
	for _, seq := range seqs {
		args = append(args, seq)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE deliveries SET status = ?, last_error = ?, next_retry_at = ?, updated_at = ?
		 WHERE subscription_id = ? AND event_seq IN (`+placeholders(len(seqs))+`)`, args...)
	return err
}

// ListDeliveries returns delivery records, newest attempts first.
func (s *Store) ListDeliveries(ctx context.Context, subscriptionID, status string, limit int) ([]model.Delivery, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT subscription_id, event_seq, attempt, status, last_error, next_retry_at, updated_at
	          FROM deliveries WHERE 1 = 1`
	args := []any{}
	if subscriptionID != "" {
		query += ` AND subscription_id = ?`
		args = append(args, subscriptionID)
	}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY updated_at DESC, event_seq DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeliveries(rows)
}

// RequeueDeliveries resets dead or failed deliveries so the dispatcher retries
// them. A subscriptionID of "" requeues matching rows for every subscription.
func (s *Store) RequeueDeliveries(ctx context.Context, subscriptionID, status string, limit int) (int64, error) {
	if limit <= 0 {
		limit = 100
	}
	if status == "" {
		status = model.DeliveryStatusDead
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	query := `UPDATE deliveries SET status = ?, attempt = 0, last_error = '', next_retry_at = NULL, updated_at = ?
	          WHERE rowid IN (
	            SELECT rowid FROM deliveries WHERE status = ?`
	args := []any{model.DeliveryStatusPending, formatTime(time.Now().UTC()), status}
	if subscriptionID != "" {
		query += ` AND subscription_id = ?`
		args = append(args, subscriptionID)
	}
	query += ` ORDER BY event_seq ASC LIMIT ?)`
	args = append(args, limit)

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func (s *Store) eventsBySeq(ctx context.Context, seqs []int64) ([]model.Event, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")
	args := make([]any, 0, len(seqs))
	for _, seq := range seqs {
		args = append(args, seq)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE seq IN (`+placeholders+`) ORDER BY seq ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Event{}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}
