package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kaulie/event-center/internal/model"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

const eventColumns = `seq, stream_seq, id, stream, provider, type, subject, source_time, received_at, dedupe_key, headers, data_type, data`

type rowScanner interface{ Scan(dest ...any) error }

func scanEvent(sc rowScanner) (*model.Event, error) {
	var (
		ev         model.Event
		sourceTime sql.NullString
		receivedAt string
		headers    string
		data       string
	)
	err := sc.Scan(
		&ev.Seq, &ev.StreamSeq, &ev.ID, &ev.Stream, &ev.Provider, &ev.Type, &ev.Subject,
		&sourceTime, &receivedAt, &ev.DedupeKey, &headers, &ev.DataType, &data,
	)
	if err != nil {
		return nil, err
	}
	ev.SourceTime = nullTime(sourceTime.String)
	if t, err := parseTime(receivedAt); err == nil {
		ev.ReceivedAt = t
	}
	ev.Headers = map[string]string{}
	if headers != "" {
		_ = json.Unmarshal([]byte(headers), &ev.Headers)
	}
	ev.Data = json.RawMessage(data)
	return &ev, nil
}

// AppendEvent persists one event and returns it with its assigned sequences.
//
// The whole operation runs inside a single write transaction guarded by
// writeMu, which makes the commit order identical to the sequence order and
// keeps stream sequencing gapless. When the event carries a DedupeKey that was
// already stored for the same provider, the existing event is returned with
// duplicate=true and nothing new is written.
func (s *Store) AppendEvent(ctx context.Context, ev *model.Event) (*model.Event, bool, error) {
	now := time.Now().UTC()
	if err := ev.Normalize(now); err != nil {
		return nil, false, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	if ev.DedupeKey != "" {
		existing, err := scanEvent(tx.QueryRowContext(ctx,
			`SELECT `+eventColumns+` FROM events WHERE provider = ? AND dedupe_key = ?`,
			ev.Provider, ev.DedupeKey))
		switch {
		case err == nil:
			return existing, true, nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, false, err
		}
	}

	var streamSeq int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO streams (name, last_seq, created_at) VALUES (?, 1, ?)
		 ON CONFLICT(name) DO UPDATE SET last_seq = last_seq + 1
		 RETURNING last_seq`,
		ev.Stream, formatTime(now)).Scan(&streamSeq)
	if err != nil {
		return nil, false, fmt.Errorf("assign stream sequence: %w", err)
	}

	headers, err := json.Marshal(ev.Headers)
	if err != nil {
		return nil, false, err
	}
	var sourceTime any
	if ev.SourceTime != nil {
		sourceTime = formatTime(*ev.SourceTime)
	}

	var seq int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO events (stream_seq, id, stream, provider, type, subject, source_time,
		                     received_at, dedupe_key, headers, data_type, data)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING seq`,
		streamSeq, ev.ID, ev.Stream, ev.Provider, ev.Type, ev.Subject, sourceTime,
		formatTime(ev.ReceivedAt), ev.DedupeKey, string(headers), ev.DataType, string(ev.Data),
	).Scan(&seq)
	if err != nil {
		return nil, false, fmt.Errorf("insert event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}

	ev.Seq = seq
	ev.StreamSeq = streamSeq
	return ev, false, nil
}

// GetEvent loads one event by its id.
func (s *Store) GetEvent(ctx context.Context, id string) (*model.Event, error) {
	ev, err := scanEvent(s.db.QueryRowContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ev, err
}

// ListEvents returns events of a stream ordered by their sequence.
//
// For a concrete stream "after" is compared against stream_seq; for the pseudo
// stream "all" it is compared against the global seq. The returned cursor
// points at the last event of the page so callers can feed it straight back as
// "after" to resume.
func (s *Store) ListEvents(ctx context.Context, stream string, after int64, limit int) (*model.ListResult, error) {
	if limit <= 0 {
		limit = 100
	}
	all := stream == "" || stream == model.StreamAll

	var (
		rows *sql.Rows
		err  error
	)
	if all {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+eventColumns+` FROM events WHERE seq > ? ORDER BY seq ASC LIMIT ?`,
			after, limit+1)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+eventColumns+` FROM events WHERE stream = ? AND stream_seq > ? ORDER BY stream_seq ASC LIMIT ?`,
			stream, after, limit+1)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &model.ListResult{Events: []model.Event{}, NextCursor: after}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		if len(res.Events) == limit {
			res.HasMore = true
			break
		}
		res.Events = append(res.Events, *ev)
		if all {
			res.NextCursor = ev.Seq
		} else {
			res.NextCursor = ev.StreamSeq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// ListStreams returns the known streams with their latest stream sequence.
func (s *Store) ListStreams(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, last_seq FROM streams ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var name string
		var last int64
		if err := rows.Scan(&name, &last); err != nil {
			return nil, err
		}
		out[name] = last
	}
	return out, rows.Err()
}

// Stats returns counters used by the metrics endpoint.
func (s *Store) Stats(ctx context.Context) (events, maxSeq, pendingDeliveries int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(seq), 0) FROM events`).Scan(&events, &maxSeq)
	if err != nil {
		return 0, 0, 0, err
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deliveries WHERE status = ?`,
		model.DeliveryStatusPending).Scan(&pendingDeliveries)
	return events, maxSeq, pendingDeliveries, err
}

// PurgeOlderThan deletes events (and their delivery records) received before
// the given time. It is a no-op when the cutoff is zero.
func (s *Store) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	if cutoff.IsZero() {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	cutoffStr := formatTime(cutoff)
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM deliveries WHERE event_seq IN (SELECT seq FROM events WHERE received_at < ?)`,
		cutoffStr); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE received_at < ?`, cutoffStr)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
