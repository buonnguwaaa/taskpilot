package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoSecret is returned when a secret name is unknown.
var ErrNoSecret = errors.New("secret not found")

// SecretMeta describes a secret without its value.
type SecretMeta struct {
	Name        string
	Kind        string
	Description string
	UpdatedAt   time.Time
	ExpiresAt   *time.Time
}

// SetPlain creates or replaces a plain (unencrypted) secret and logs it.
func (d *DB) SetPlain(ctx context.Context, name, value, description string, expiresAt *time.Time, actor string) error {
	if name == "" || value == "" {
		return errors.New("name and value are required")
	}
	now := Now()
	var exp any
	if expiresAt != nil {
		exp = expiresAt.UTC().Format(time.RFC3339)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existed bool
	if err := tx.QueryRowContext(ctx, `SELECT count(*) > 0 FROM secrets WHERE name = ?`, name).Scan(&existed); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO secrets (name, kind, value, description, created_by, created_at, updated_at, expires_at)
		VALUES (?, 'plain', ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			kind = 'plain', value = excluded.value,
			ciphertext = NULL, nonce = NULL, key_id = NULL,
			vault_mount = NULL, vault_path = NULL, vault_field = NULL,
			description = coalesce(nullif(excluded.description, ''), secrets.description),
			updated_at = excluded.updated_at, rotated_at = excluded.updated_at,
			expires_at = excluded.expires_at`,
		name, value, description, actor, now, now, exp)
	if err != nil {
		return err
	}
	action := "create"
	if existed {
		action = "update"
	}
	if err := logAccess(ctx, tx, name, action, actor, "", true, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// Resolve returns a secret's value for consumer and records the access.
// Only plain secrets are supported so far.
func (d *DB) Resolve(ctx context.Context, name, consumer string) (string, *SecretMeta, error) {
	var (
		kind, desc, updated string
		value, expires      sql.NullString
	)
	err := d.QueryRowContext(ctx,
		`SELECT kind, value, coalesce(description, ''), updated_at, expires_at FROM secrets WHERE name = ?`, name).
		Scan(&kind, &value, &desc, &updated, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		_ = logAccess(ctx, d, name, "resolve", "daemon", consumer, false, "not found")
		return "", nil, fmt.Errorf("%w: %s", ErrNoSecret, name)
	}
	if err != nil {
		return "", nil, err
	}
	meta := buildMeta(name, kind, desc, updated, expires)
	if kind != "plain" || !value.Valid {
		_ = logAccess(ctx, d, name, "resolve", "daemon", consumer, false, "unsupported kind "+kind)
		return "", meta, fmt.Errorf("secret %s has kind %s; only plain is implemented", name, kind)
	}
	if err := logAccess(ctx, d, name, "resolve", "daemon", consumer, true, ""); err != nil {
		return "", nil, err
	}
	return value.String, meta, nil
}

// Meta returns a secret's metadata without touching its value or the access log.
func (d *DB) Meta(ctx context.Context, name string) (*SecretMeta, error) {
	var kind, desc, updated string
	var expires sql.NullString
	err := d.QueryRowContext(ctx,
		`SELECT kind, coalesce(description, ''), updated_at, expires_at FROM secrets WHERE name = ?`, name).
		Scan(&kind, &desc, &updated, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoSecret, name)
	}
	if err != nil {
		return nil, err
	}
	return buildMeta(name, kind, desc, updated, expires), nil
}

// DeleteSecret removes a secret (bindings cascade) and logs it.
func (d *DB) DeleteSecret(ctx context.Context, name, actor string) error {
	res, err := d.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNoSecret, name)
	}
	return logAccess(ctx, d, name, "delete", actor, "", true, "")
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func logAccess(ctx context.Context, x execer, name, action, actor, consumer string, ok bool, errMsg string) error {
	_, err := x.ExecContext(ctx, `
		INSERT INTO secret_access_log (secret_id, secret_name, at, action, actor, consumer, ok, error)
		VALUES ((SELECT id FROM secrets WHERE name = ?), ?, ?, ?, ?, nullif(?, ''), ?, nullif(?, ''))`,
		name, name, Now(), action, actor, consumer, ok, errMsg)
	return err
}

func buildMeta(name, kind, desc, updated string, expires sql.NullString) *SecretMeta {
	m := &SecretMeta{Name: name, Kind: kind, Description: desc}
	m.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if expires.Valid {
		if t, err := time.Parse(time.RFC3339, expires.String); err == nil {
			m.ExpiresAt = &t
		}
	}
	return m
}
