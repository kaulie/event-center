package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/kaulie/event-center/internal/model"
)

// UpsertSource creates or updates an event source registration.
func (s *Store) UpsertSource(ctx context.Context, src *model.Source) error {
	if src.ID == "" {
		return model.ErrInvalid("source id is required")
	}
	if src.CreatedAt.IsZero() {
		src.CreatedAt = time.Now().UTC()
	}
	if src.VerifyMode == "" {
		src.VerifyMode = model.VerifyNone
	}
	if src.DefaultStream == "" {
		src.DefaultStream = model.DefaultStream
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sources (id, kind, secret, verify_mode, type_prefix, default_stream, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   kind = excluded.kind,
		   secret = excluded.secret,
		   verify_mode = excluded.verify_mode,
		   type_prefix = excluded.type_prefix,
		   default_stream = excluded.default_stream,
		   enabled = excluded.enabled`,
		src.ID, src.Kind, src.Secret, src.VerifyMode, src.TypePrefix, src.DefaultStream,
		boolToInt(src.Enabled), formatTime(src.CreatedAt))
	return err
}

func scanSource(sc rowScanner) (*model.Source, error) {
	var (
		src       model.Source
		enabled   int
		createdAt string
	)
	if err := sc.Scan(&src.ID, &src.Kind, &src.Secret, &src.VerifyMode, &src.TypePrefix,
		&src.DefaultStream, &enabled, &createdAt); err != nil {
		return nil, err
	}
	src.Enabled = enabled != 0
	if t, err := parseTime(createdAt); err == nil {
		src.CreatedAt = t
	}
	return &src, nil
}

// GetSource loads one source by id.
func (s *Store) GetSource(ctx context.Context, id string) (*model.Source, error) {
	src, err := scanSource(s.db.QueryRowContext(ctx,
		`SELECT id, kind, secret, verify_mode, type_prefix, default_stream, enabled, created_at
		 FROM sources WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return src, err
}

// ListSources returns every registered source.
func (s *Store) ListSources(ctx context.Context) ([]model.Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, secret, verify_mode, type_prefix, default_stream, enabled, created_at
		 FROM sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Source{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *src)
	}
	return out, rows.Err()
}

// DeleteSource removes a source registration.
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM sources WHERE id = ?`, id)
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
