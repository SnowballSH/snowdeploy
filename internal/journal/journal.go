// Package journal is the durable, append-only record of every deploy the
// daemon performs. A finished entry is a receipt: it is never rewritten.
package journal

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// Actions a journal entry can record.
const (
	ActionDeploy       = "deploy"
	ActionRollback     = "rollback"
	ActionAutoRollback = "auto-rollback"
)

// Terminal states. The deploy engine's intermediate states are its own; these
// three are the only ones the journal itself reasons about.
const (
	StateHealthy    = "healthy"
	StateRolledBack = "rolled-back"
	StateFailed     = "failed"
)

// ErrNoHealthyDeploy is returned when a service has never reached healthy.
var ErrNoHealthyDeploy = errors.New("no healthy deploy recorded for service")

// ErrAlreadyFinished is returned when something tries to rewrite a receipt.
var ErrAlreadyFinished = errors.New("journal entry is already finished")

// ErrNotFound is returned for an unknown entry id.
var ErrNotFound = errors.New("journal entry not found")

// Entry is one deploy attempt from start to receipt.
type Entry struct {
	ID         int64
	Service    string
	Action     string
	Actor      string
	OldDigest  string
	NewDigest  string
	PRNumber   int
	MergeSHA   string
	State      string
	Detail     string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Journal is the SQLite-backed store.
type Journal struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS deploys (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	service     TEXT    NOT NULL,
	action      TEXT    NOT NULL,
	actor       TEXT    NOT NULL,
	old_digest  TEXT    NOT NULL DEFAULT '',
	new_digest  TEXT    NOT NULL DEFAULT '',
	pr_number   INTEGER NOT NULL DEFAULT 0,
	merge_sha   TEXT    NOT NULL DEFAULT '',
	state       TEXT    NOT NULL,
	detail      TEXT    NOT NULL DEFAULT '',
	started_at  INTEGER NOT NULL,
	finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS deploys_by_service
	ON deploys(service, started_at DESC, id DESC);
`

// Open opens or creates the journal at path.
func Open(path string) (*Journal, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(FULL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	// One writer keeps a single-host deploy record free of SQLITE_BUSY retries.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create journal schema: %w", err)
	}
	return &Journal{db: db}, nil
}

// Close releases the database handle.
func (j *Journal) Close() error {
	return j.db.Close()
}

// Begin records the start of a deploy and returns its entry id.
func (j *Journal) Begin(e Entry) (int64, error) {
	started := e.StartedAt
	if started.IsZero() {
		started = time.Now().UTC()
	}
	res, err := j.db.Exec(`
		INSERT INTO deploys
			(service, action, actor, old_digest, new_digest, pr_number,
			 merge_sha, state, detail, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Service, e.Action, e.Actor, e.OldDigest, e.NewDigest, e.PRNumber,
		e.MergeSHA, e.State, e.Detail, started.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("begin journal entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("begin journal entry: %w", err)
	}
	return id, nil
}

// Progress records an intermediate state on an unfinished entry.
func (j *Journal) Progress(id int64, state, detail string) error {
	return j.update(id, `
		UPDATE deploys SET state = ?, detail = ?
		WHERE id = ? AND finished_at IS NULL`,
		state, detail, id)
}

// Finish writes the receipt. It succeeds exactly once per entry.
func (j *Journal) Finish(id int64, state, detail string) error {
	return j.update(id, `
		UPDATE deploys SET state = ?, detail = ?, finished_at = ?
		WHERE id = ? AND finished_at IS NULL`,
		state, detail, time.Now().UTC().UnixNano(), id)
}

// SetPR records the pull request an in-flight entry opened.
func (j *Journal) SetPR(id int64, prNumber int) error {
	return j.update(id, `
		UPDATE deploys SET pr_number = ?
		WHERE id = ? AND finished_at IS NULL`,
		prNumber, id)
}

// SetMergeSHA records the merge commit an in-flight entry produced.
func (j *Journal) SetMergeSHA(id int64, sha string) error {
	return j.update(id, `
		UPDATE deploys SET merge_sha = ?
		WHERE id = ? AND finished_at IS NULL`,
		sha, id)
}

func (j *Journal) update(id int64, query string, args ...any) error {
	res, err := j.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("update journal entry %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update journal entry %d: %w", id, err)
	}
	if n == 1 {
		return nil
	}
	return j.explainNoUpdate(id)
}

// explainNoUpdate distinguishes an unknown entry from a sealed receipt.
func (j *Journal) explainNoUpdate(id int64) error {
	var finished sql.NullInt64
	err := j.db.QueryRow(`SELECT finished_at FROM deploys WHERE id = ?`, id).Scan(&finished)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("entry %d: %w", id, ErrNotFound)
	case err != nil:
		return fmt.Errorf("inspect journal entry %d: %w", id, err)
	default:
		return fmt.Errorf("entry %d: %w", id, ErrAlreadyFinished)
	}
}

const selectColumns = `
	id, service, action, actor, old_digest, new_digest, pr_number,
	merge_sha, state, detail, started_at, finished_at`

// Recent returns a service's entries newest first, bounded by n.
func (j *Journal) Recent(service string, n int) ([]Entry, error) {
	if n <= 0 {
		n = 20
	}
	rows, err := j.db.Query(`
		SELECT`+selectColumns+`
		FROM deploys WHERE service = ?
		ORDER BY started_at DESC, id DESC LIMIT ?`, service, n)
	if err != nil {
		return nil, fmt.Errorf("read journal for %s: %w", service, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read journal for %s: %w", service, err)
	}
	return out, nil
}

// Unfinished returns every entry that never received a receipt, oldest first.
// After an ungraceful death these rows are the deploys the dying process was
// carrying: no goroutine in the new process is driving them, so they stay
// in-flight forever unless someone closes them out.
func (j *Journal) Unfinished() ([]Entry, error) {
	rows, err := j.db.Query(`
		SELECT` + selectColumns + `
		FROM deploys WHERE finished_at IS NULL
		ORDER BY started_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("read unfinished journal entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unfinished journal entries: %w", err)
	}
	return out, nil
}

// LastHealthyDigest is the digest of the newest deploy that reached healthy.
func (j *Journal) LastHealthyDigest(service string) (string, error) {
	var digest string
	err := j.db.QueryRow(`
		SELECT new_digest FROM deploys
		WHERE service = ? AND state = ? AND new_digest != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`,
		service, StateHealthy).Scan(&digest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%s: %w", service, ErrNoHealthyDeploy)
	case err != nil:
		return "", fmt.Errorf("read last healthy digest for %s: %w", service, err)
	}
	return digest, nil
}

// PreviousHealthyDigest is the newest digest that reached healthy and is not
// notDigest. This — not the newest healthy digest — is what a rollback with no
// explicit target means: the newest healthy digest is usually the one running.
func (j *Journal) PreviousHealthyDigest(service, notDigest string) (string, error) {
	var digest string
	err := j.db.QueryRow(`
		SELECT new_digest FROM deploys
		WHERE service = ? AND state = ? AND new_digest != '' AND new_digest != ?
		ORDER BY started_at DESC, id DESC LIMIT 1`,
		service, StateHealthy, notDigest).Scan(&digest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%s (other than %s): %w", service, notDigest, ErrNoHealthyDeploy)
	case err != nil:
		return "", fmt.Errorf("read previous healthy digest for %s: %w", service, err)
	}
	return digest, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEntry(s scanner) (Entry, error) {
	var (
		e        Entry
		started  int64
		finished sql.NullInt64
	)
	if err := s.Scan(&e.ID, &e.Service, &e.Action, &e.Actor, &e.OldDigest,
		&e.NewDigest, &e.PRNumber, &e.MergeSHA, &e.State, &e.Detail,
		&started, &finished); err != nil {
		return Entry{}, fmt.Errorf("scan journal entry: %w", err)
	}
	e.StartedAt = time.Unix(0, started).UTC()
	if finished.Valid {
		e.FinishedAt = time.Unix(0, finished.Int64).UTC()
	}
	return e, nil
}
