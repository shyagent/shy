package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "shy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWALAndForeignKeys(t *testing.T) {
	s := testOpen(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode %s", mode)
	}
	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys %d", fk)
	}
	_, err := s.db.Exec(`INSERT INTO installs(account_id, name, key_hash, key_prefix, created_at) VALUES (999, 'x', 'h', 'p', 't')`)
	if err == nil {
		t.Fatal("expected foreign key failure")
	}
}

func TestSecretsAreHashed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	acc, rawSession, err := s.CreateAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(acc.Number) != 16 {
		t.Fatalf("number %q", acc.Number)
	}
	inst, key, err := s.CreateInstall(ctx, acc.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	var hash, prefix string
	if err := s.db.QueryRow(`SELECT key_hash, key_prefix FROM installs WHERE id = ?`, inst.ID).Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if hash != hashSecret(key) {
		t.Fatal("key hash mismatch")
	}
	if strings.Contains(hash, key) || !strings.HasPrefix(key, prefix) {
		t.Fatalf("hash %s prefix %s", hash, prefix)
	}
	got, gotAcc, err := s.InstallByKey(ctx, key)
	if err != nil || got.ID != inst.ID || gotAcc.Number != acc.Number {
		t.Fatalf("lookup: %+v %+v %v", got, gotAcc, err)
	}
	if _, _, err := s.InstallByKey(ctx, key+"nope"); err != ErrNotFound {
		t.Fatalf("bad key: %v", err)
	}
	if err := s.RevokeInstall(ctx, acc.ID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InstallByKey(ctx, key); err != ErrNotFound {
		t.Fatalf("revoked key: %v", err)
	}

	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(name)
		if err != nil {
			if os.IsNotExist(err) && strings.HasSuffix(name, "-wal") {
				continue
			}
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(key)) {
			t.Fatalf("plaintext api key in %s", name)
		}
		if bytes.Contains(b, []byte(rawSession)) {
			t.Fatalf("session token in %s", name)
		}
	}
}

func TestUniqueAccountNumber(t *testing.T) {
	s := testOpen(t)
	acc, _, err := s.CreateAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO accounts(number, created_at, status) VALUES (?, 't', 'active')`, acc.Number)
	if err == nil {
		t.Fatal("expected unique violation")
	}
	if !isConstraint(err) {
		t.Fatalf("not a constraint error: %T %v", err, err)
	}
}

func TestAnonymousTraceIsUnowned(t *testing.T) {
	s := testOpen(t)
	ctx := context.Background()
	acc, _, err := s.CreateAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.InsertTrace(ctx, NewTrace{Workflow: "anon", Status: "OK", Payload: `{"name":"anon"}`})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTraces(ctx, acc.ID, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("list %+v %v", list, err)
	}
	var accountID, installID *int64
	if err := s.db.QueryRow(`SELECT account_id, install_id FROM traces WHERE id = ?`, id).Scan(&accountID, &installID); err != nil {
		t.Fatal(err)
	}
	if accountID != nil || installID != nil {
		t.Fatalf("account %v install %v", accountID, installID)
	}
}

func TestMigrateRelaxesTraceOwners(t *testing.T) {
	s := testOpen(t)
	ctx := context.Background()
	acc, _, err := s.CreateAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst, _, err := s.CreateInstall(ctx, acc.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	owned, err := s.InsertTrace(ctx, NewTrace{
		AccountID: acc.ID, InstallID: inst.ID, Workflow: "owned", Status: "OK", Payload: `{"name":"owned"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	const old = `
CREATE TABLE traces_old (
	id INTEGER PRIMARY KEY,
	account_id INTEGER NOT NULL REFERENCES accounts(id),
	install_id INTEGER NOT NULL REFERENCES installs(id),
	received_at TEXT NOT NULL,
	workflow TEXT NOT NULL,
	status TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	input_preview TEXT NOT NULL DEFAULT '',
	output_preview TEXT NOT NULL DEFAULT '',
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	span_count INTEGER NOT NULL DEFAULT 0,
	neatlogs_trace_id TEXT NOT NULL DEFAULT '',
	forward_status TEXT NOT NULL,
	forward_error TEXT NOT NULL DEFAULT '',
	payload TEXT NOT NULL
) STRICT;
INSERT INTO traces_old SELECT * FROM traces;
DROP TABLE traces;
ALTER TABLE traces_old RENAME TO traces;
`
	if err := execStmts(s.db, old); err != nil {
		t.Fatal(err)
	}
	if err := s.allowAnonymousTraces(); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTraces(ctx, acc.ID, 10)
	if err != nil || len(list) != 1 || list[0].ID != owned || list[0].Workflow != "owned" {
		t.Fatalf("%+v %v", list, err)
	}
	if _, err := s.InsertTrace(ctx, NewTrace{Workflow: "anon", Status: "OK", Payload: `{}`}); err != nil {
		t.Fatal(err)
	}
}

func TestFlashOnce(t *testing.T) {
	s := testOpen(t)
	ctx := context.Background()
	_, raw, err := s.CreateAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetFlash(ctx, raw, Flash{Key: "shy_secret", InstallName: "laptop"}); err != nil {
		t.Fatal(err)
	}
	f, err := s.TakeFlash(ctx, raw)
	if err != nil || f.Key != "shy_secret" || f.InstallName != "laptop" {
		t.Fatalf("%+v %v", f, err)
	}
	f, err = s.TakeFlash(ctx, raw)
	if err != nil || f.Key != "" {
		t.Fatalf("second take %+v %v", f, err)
	}
}
