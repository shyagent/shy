// Package store is the SQLite database for accounts, install keys, and traces.
//
// Account numbers are the credential: 16 digits, shown in full, with no reset.
// Install API keys are stored as SHA-256 digests; the plaintext is returned
// once when the install is created. A later one-time token exchange can mint
// a key without that reveal. Add it as its own table and handler, and keep
// InstallByKey as the check on POST /v1/trace when a key is presented.
// A request with no key is stored with NULL account and install ids.
//
// prompt_tokens and completion_tokens are the billing seam. The payload
// column is the history of agent actions.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
)

const sessionTTL = 30 * 24 * time.Hour

// ErrNotFound is returned when a row is missing, revoked, or expired.
// Callers should not distinguish those cases on the public API.
var ErrNotFound = errors.New("not found")

// Account is a Mullvad-style numbered account.
type Account struct {
	ID        int64
	Number    string
	CreatedAt string
	Status    string
}

// Session is a dashboard login.
type Session struct {
	AccountID int64
	Number    string
	CSRF      string
}

// Flash is shown once on the next dashboard load.
type Flash struct {
	NewAccount  bool   `json:"new_account,omitempty"`
	Key         string `json:"key,omitempty"`
	InstallName string `json:"install,omitempty"`
}

// Install is one shy installation under an account.
type Install struct {
	ID         int64
	AccountID  int64
	Name       string
	KeyPrefix  string
	CreatedAt  string
	LastSeenAt string
	RevokedAt  string
}

// Trace is one stored agent run.
type Trace struct {
	ID               int64
	AccountID        int64
	InstallID        int64
	InstallName      string
	ReceivedAt       string
	Workflow         string
	Status           string
	SessionID        string
	InputPreview     string
	OutputPreview    string
	PromptTokens     int64
	CompletionTokens int64
	SpanCount        int
	NeatlogsTraceID  string
	ForwardStatus    string
	ForwardError     string
	Payload          string
}

// NewTrace is the insert shape. Forwarding fields start empty.
type NewTrace struct {
	AccountID        int64
	InstallID        int64
	Workflow         string
	Status           string
	SessionID        string
	InputPreview     string
	OutputPreview    string
	PromptTokens     int64
	CompletionTokens int64
	SpanCount        int
	Payload          string
}

// Store is one SQLite file. Open sets WAL and a single connection.
type Store struct {
	db *sql.DB
}

// Open creates the file if needed and applies the schema.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=rwc"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps PRAGMA foreign_keys (per connection) on, and keeps
	// writers from tripping over each other. Forwarding to Neatlogs happens
	// outside any query.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.configure(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) configure() error {
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("sqlite journal_mode=%s, want wal", mode)
	}
	if _, err := s.db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		return err
	}
	if fk != 1 {
		return fmt.Errorf("sqlite foreign_keys=%d, want 1", fk)
	}
	return nil
}

func (s *Store) migrate() error {
	if err := execStmts(s.db, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := s.allowAnonymousTraces(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// allowAnonymousTraces rebuilds traces when account_id is NOT NULL.
// Anonymous submissions store NULL account and install ids.
func (s *Store) allowAnonymousTraces() error {
	var notnull int
	err := s.db.QueryRow(`SELECT "notnull" FROM pragma_table_info('traces') WHERE name = 'account_id'`).Scan(&notnull)
	if err != nil {
		return err
	}
	if notnull == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const rebuild = `
CREATE TABLE traces_anon (
	id INTEGER PRIMARY KEY,
	account_id INTEGER REFERENCES accounts(id),
	install_id INTEGER REFERENCES installs(id),
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
INSERT INTO traces_anon SELECT * FROM traces;
DROP TABLE traces;
ALTER TABLE traces_anon RENAME TO traces;
CREATE INDEX IF NOT EXISTS traces_account_received ON traces(account_id, received_at DESC, id DESC);
`
	if err := execStmts(tx, rebuild); err != nil {
		return err
	}
	return tx.Commit()
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func execStmts(db execer, script string) error {
	for _, stmt := range strings.Split(script, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// CreateAccount inserts a numbered account and a session for it.
// The returned token is the raw session cookie value.
func (s *Store) CreateAccount(ctx context.Context) (Account, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, "", err
	}
	defer tx.Rollback()

	created := now()
	var acc Account
	for attempt := 0; attempt < 5; attempt++ {
		number, err := newAccountNumber()
		if err != nil {
			return Account{}, "", err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO accounts(number, created_at, status) VALUES (?, ?, 'active')`,
			number, created)
		if isConstraint(err) {
			continue
		}
		if err != nil {
			return Account{}, "", err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Account{}, "", err
		}
		acc = Account{ID: id, Number: number, CreatedAt: created, Status: "active"}
		break
	}
	if acc.ID == 0 {
		return Account{}, "", errors.New("could not allocate an account number")
	}
	raw, err := insertSession(ctx, tx, acc.ID)
	if err != nil {
		return Account{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, "", err
	}
	return acc, raw, nil
}

// AccountByNumber looks up an account. The number is the credential.
func (s *Store) AccountByNumber(ctx context.Context, number string) (Account, error) {
	var acc Account
	err := s.db.QueryRowContext(ctx,
		`SELECT id, number, created_at, status FROM accounts WHERE number = ?`, number).
		Scan(&acc.ID, &acc.Number, &acc.CreatedAt, &acc.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	return acc, nil
}

// CreateSession starts a dashboard session for an existing account.
func (s *Store) CreateSession(ctx context.Context, accountID int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now()); err != nil {
		return "", err
	}
	raw, err := insertSession(ctx, tx, accountID)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return raw, nil
}

func insertSession(ctx context.Context, tx *sql.Tx, accountID int64) (string, error) {
	raw, err := randomToken()
	if err != nil {
		return "", err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", err
	}
	expires := time.Now().UTC().Add(sessionTTL).Format(time.RFC3339)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions(token_hash, account_id, csrf, flash, created_at, expires_at) VALUES (?, ?, ?, '', ?, ?)`,
		hashSecret(raw), accountID, csrf, now(), expires)
	if err != nil {
		return "", err
	}
	return raw, nil
}

// Session resolves a raw cookie token.
func (s *Store) Session(ctx context.Context, raw string) (Session, error) {
	var sess Session
	var status, expires string
	err := s.db.QueryRowContext(ctx, `
		SELECT a.id, a.number, a.status, s.csrf, s.expires_at
		FROM sessions s
		JOIN accounts a ON a.id = s.account_id
		WHERE s.token_hash = ?`, hashSecret(raw)).
		Scan(&sess.AccountID, &sess.Number, &status, &sess.CSRF, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	exp, err := time.Parse(time.RFC3339, expires)
	if err != nil || !exp.After(time.Now()) || status != "active" {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashSecret(raw))
		return Session{}, ErrNotFound
	}
	return sess, nil
}

// DeleteSession drops one login.
func (s *Store) DeleteSession(ctx context.Context, raw string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashSecret(raw))
	return err
}

// SetFlash stores a one-time dashboard message on the session.
func (s *Store) SetFlash(ctx context.Context, raw string, f Flash) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET flash = ? WHERE token_hash = ?`, string(b), hashSecret(raw))
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

// TakeFlash returns the flash and clears it.
func (s *Store) TakeFlash(ctx context.Context, raw string) (Flash, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Flash{}, err
	}
	defer tx.Rollback()
	hash := hashSecret(raw)
	var payload, expires string
	err = tx.QueryRowContext(ctx, `SELECT flash, expires_at FROM sessions WHERE token_hash = ?`, hash).
		Scan(&payload, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Flash{}, ErrNotFound
	}
	if err != nil {
		return Flash{}, err
	}
	exp, err := time.Parse(time.RFC3339, expires)
	if err != nil || !exp.After(time.Now()) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash); err != nil {
			return Flash{}, err
		}
		if err := tx.Commit(); err != nil {
			return Flash{}, err
		}
		return Flash{}, ErrNotFound
	}
	if payload != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET flash = '' WHERE token_hash = ?`, hash); err != nil {
			return Flash{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Flash{}, err
	}
	if payload == "" {
		return Flash{}, nil
	}
	var f Flash
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return Flash{}, nil
	}
	return f, nil
}

// CreateInstall mints an API key. raw is shown once and is not stored.
func (s *Store) CreateInstall(ctx context.Context, accountID int64, name string) (Install, string, error) {
	raw, err := newAPIKey()
	if err != nil {
		return Install{}, "", err
	}
	name = cleanName(name)
	created := now()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO installs(account_id, name, key_hash, key_prefix, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		accountID, name, hashSecret(raw), prefix(raw), created)
	if err != nil {
		return Install{}, "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Install{}, "", err
	}
	return Install{
		ID:        id,
		AccountID: accountID,
		Name:      name,
		KeyPrefix: prefix(raw),
		CreatedAt: created,
	}, raw, nil
}

// ListInstalls returns installs for an account, active ones first.
func (s *Store) ListInstalls(ctx context.Context, accountID int64) ([]Install, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, name, key_prefix, created_at,
		       COALESCE(last_seen_at, ''), COALESCE(revoked_at, '')
		FROM installs
		WHERE account_id = ?
		ORDER BY (revoked_at IS NOT NULL), id DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Install
	for rows.Next() {
		var in Install
		if err := rows.Scan(&in.ID, &in.AccountID, &in.Name, &in.KeyPrefix, &in.CreatedAt, &in.LastSeenAt, &in.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// RevokeInstall disables a key. Revoking twice is fine. Another account's id is ErrNotFound.
func (s *Store) RevokeInstall(ctx context.Context, accountID, id int64) error {
	var owner int64
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT account_id, revoked_at FROM installs WHERE id = ?`, id).
		Scan(&owner, &revoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != accountID) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if revoked.Valid {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE installs SET revoked_at = ? WHERE id = ? AND account_id = ?`, now(), id, accountID)
	return err
}

// InstallByKey resolves a plaintext install key. Revoked keys and inactive
// accounts are ErrNotFound.
func (s *Store) InstallByKey(ctx context.Context, rawKey string) (Install, Account, error) {
	var in Install
	var acc Account
	err := s.db.QueryRowContext(ctx, `
		SELECT i.id, i.account_id, i.name, i.key_prefix, i.created_at,
		       COALESCE(i.last_seen_at, ''), COALESCE(i.revoked_at, ''),
		       a.id, a.number, a.created_at, a.status
		FROM installs i
		JOIN accounts a ON a.id = i.account_id
		WHERE i.key_hash = ?`, hashSecret(rawKey)).
		Scan(&in.ID, &in.AccountID, &in.Name, &in.KeyPrefix, &in.CreatedAt, &in.LastSeenAt, &in.RevokedAt,
			&acc.ID, &acc.Number, &acc.CreatedAt, &acc.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Install{}, Account{}, ErrNotFound
	}
	if err != nil {
		return Install{}, Account{}, err
	}
	if in.RevokedAt != "" || acc.Status != "active" {
		return Install{}, Account{}, ErrNotFound
	}
	return in, acc, nil
}

// TouchInstall records that a key was just used.
func (s *Store) TouchInstall(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE installs SET last_seen_at = ? WHERE id = ?`, now(), id)
	return err
}

// InsertTrace stores a payload before it is forwarded. Status starts at pending.
// A zero AccountID stores NULL account and install ids (an anonymous submission).
func (s *Store) InsertTrace(ctx context.Context, n NewTrace) (int64, error) {
	var accountID, installID any
	if n.AccountID != 0 {
		accountID = n.AccountID
		installID = n.InstallID
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO traces (
			account_id, install_id, received_at, workflow, status, session_id,
			input_preview, output_preview, prompt_tokens, completion_tokens, span_count,
			neatlogs_trace_id, forward_status, forward_error, payload
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 'pending', '', ?)`,
		accountID, installID, now(), n.Workflow, n.Status, n.SessionID,
		n.InputPreview, n.OutputPreview, n.PromptTokens, n.CompletionTokens, n.SpanCount,
		n.Payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MarkForward records the Neatlogs result. status is sent, failed, or skipped.
func (s *Store) MarkForward(ctx context.Context, id int64, status, neatlogsID, errMsg string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE traces
		SET forward_status = ?, neatlogs_trace_id = ?, forward_error = ?
		WHERE id = ?`, status, neatlogsID, errMsg, id)
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

// ListTraces returns the newest traces for an account, without payloads.
func (s *Store) ListTraces(ctx context.Context, accountID int64, limit int) ([]Trace, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.account_id, t.install_id, i.name, t.received_at, t.workflow, t.status,
		       t.session_id, t.input_preview, t.output_preview, t.prompt_tokens, t.completion_tokens,
		       t.span_count, t.neatlogs_trace_id, t.forward_status, t.forward_error
		FROM traces t
		JOIN installs i ON i.id = t.install_id
		WHERE t.account_id = ?
		ORDER BY t.received_at DESC, t.id DESC
		LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trace
	for rows.Next() {
		tr, err := scanTrace(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// Trace loads one payload, scoped to the account.
func (s *Store) Trace(ctx context.Context, accountID, id int64) (Trace, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.account_id, t.install_id, i.name, t.received_at, t.workflow, t.status,
		       t.session_id, t.input_preview, t.output_preview, t.prompt_tokens, t.completion_tokens,
		       t.span_count, t.neatlogs_trace_id, t.forward_status, t.forward_error, t.payload
		FROM traces t
		JOIN installs i ON i.id = t.install_id
		WHERE t.id = ? AND t.account_id = ?`, id, accountID)
	tr, err := scanTrace(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return Trace{}, ErrNotFound
	}
	return tr, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTrace(sc scanner, withPayload bool) (Trace, error) {
	var tr Trace
	dest := []any{
		&tr.ID, &tr.AccountID, &tr.InstallID, &tr.InstallName, &tr.ReceivedAt, &tr.Workflow, &tr.Status,
		&tr.SessionID, &tr.InputPreview, &tr.OutputPreview, &tr.PromptTokens, &tr.CompletionTokens,
		&tr.SpanCount, &tr.NeatlogsTraceID, &tr.ForwardStatus, &tr.ForwardError,
	}
	if withPayload {
		dest = append(dest, &tr.Payload)
	}
	err := sc.Scan(dest...)
	return tr, err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func newAccountNumber() (string, error) {
	const digits = 16
	out := make([]byte, digits)
	buf := make([]byte, 32)
	filled := 0
	for filled < digits {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b >= 250 { // 25*10, rejection sampling avoids modulo bias
				continue
			}
			out[filled] = '0' + b/25
			filled++
			if filled == digits {
				break
			}
		}
	}
	return string(out), nil
}

func newAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "shy_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func prefix(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

func cleanName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	r := []rune(name)
	if len(r) > 40 {
		r = r[:40]
	}
	name = strings.TrimSpace(string(r))
	if name == "" {
		return "install"
	}
	return name
}

func isConstraint(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()&0xff == 19 // SQLITE_CONSTRAINT
	}
	return false
}

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id INTEGER PRIMARY KEY,
	number TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'active'
) STRICT;

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	account_id INTEGER NOT NULL REFERENCES accounts(id),
	csrf TEXT NOT NULL,
	flash TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS installs (
	id INTEGER PRIMARY KEY,
	account_id INTEGER NOT NULL REFERENCES accounts(id),
	name TEXT NOT NULL,
	key_hash TEXT NOT NULL UNIQUE,
	key_prefix TEXT NOT NULL,
	created_at TEXT NOT NULL,
	last_seen_at TEXT,
	revoked_at TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS installs_account ON installs(account_id);

CREATE TABLE IF NOT EXISTS traces (
	id INTEGER PRIMARY KEY,
	account_id INTEGER REFERENCES accounts(id),
	install_id INTEGER REFERENCES installs(id),
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

CREATE INDEX IF NOT EXISTS traces_account_received ON traces(account_id, received_at DESC, id DESC);
`
