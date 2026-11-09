package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenMigratesAndIsPrivate(t *testing.T) {
	home := t.TempDir()
	d, err := Open(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.SchemaVersion(context.Background()); err != nil || v != 2 {
		t.Fatalf("schema version = %d, %v", v, err)
	}
	d.Close()
	st, err := os.Stat(filepath.Join(home, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("db mode = %v, want 0600", st.Mode().Perm())
	}
	// Re-opening must not re-run the migration.
	d2, err := Open(context.Background(), home)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	d2.Close()
}

func TestPlainSecretLifecycle(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	if _, _, err := d.Resolve(ctx, "x/y", "test"); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("missing secret: %v", err)
	}
	if err := d.SetPlain(ctx, "x/y", "v1", "desc", &exp, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPlain(ctx, "x/y", "v2", "", nil, "daemon"); err != nil {
		t.Fatal(err)
	}
	v, meta, err := d.Resolve(ctx, "x/y", "source:test")
	if err != nil || v != "v2" {
		t.Fatalf("resolve = %q, %v", v, err)
	}
	if meta.Description != "desc" || meta.ExpiresAt != nil {
		t.Fatalf("meta = %+v (update without expiry clears it; empty description keeps the old one)", meta)
	}
	list, err := d.ListSecrets(ctx)
	if err != nil || len(list) != 1 || list[0].Kind != "plain" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	var actions []string
	rows, err := d.Query(`SELECT action || ':' || ok FROM secret_access_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	want := []string{"resolve:0", "create:1", "update:1", "resolve:1"}
	if len(actions) != len(want) {
		t.Fatalf("log = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("log = %v, want %v", actions, want)
		}
	}
	var leaked int
	d.QueryRow(`SELECT count(*) FROM secret_access_log WHERE coalesce(error,'') LIKE '%v2%'`).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("secret value leaked into the access log")
	}

	if err := d.DeleteSecret(ctx, "x/y", "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Meta(ctx, "x/y"); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestSchemaRejectsMalformedSecrets(t *testing.T) {
	d := open(t)
	now := Now()
	bad := []string{
		`INSERT INTO secrets (name, kind, created_at, updated_at) VALUES ('a', 'plain', ?, ?)`,
		`INSERT INTO secrets (name, kind, value, vault_path, created_at, updated_at) VALUES ('b', 'plain', 'v', 'p', ?, ?)`,
		`INSERT INTO secrets (name, kind, value, created_at, updated_at) VALUES ('c', 'vault_ref', 'v', ?, ?)`,
	}
	for _, q := range bad {
		if _, err := d.Exec(q, now, now); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}
