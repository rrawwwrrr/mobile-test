package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	serial       TEXT    NOT NULL,
	model        TEXT    NOT NULL,
	finished_at  TEXT    NOT NULL,
	passing      INTEGER NOT NULL DEFAULT 0,
	failing      INTEGER NOT NULL DEFAULT 0,
	pending      INTEGER NOT NULL DEFAULT 0,
	found        INTEGER NOT NULL DEFAULT 0,
	boot_ok      INTEGER NOT NULL DEFAULT 0,
	boot_seconds REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_runs_serial ON runs(serial);
CREATE INDEX IF NOT EXISTS idx_runs_finished ON runs(finished_at);
`

// Run holds the result of one test cycle for one device.
type Run struct {
	ID          int64
	Serial      string
	Model       string
	FinishedAt  time.Time
	Passing     int
	Failing     int
	Pending     int
	Found       bool
	BootOK      bool
	BootSeconds float64
}

// Verdict returns "PASS", "FAIL", or "N/A".
func (r Run) Verdict() string {
	if !r.Found {
		return "N/A"
	}
	if r.Failing > 0 {
		return "FAIL"
	}
	return "PASS"
}

// DeviceLabel returns "serial (model)" or just "serial".
func (r Run) DeviceLabel() string {
	if r.Model != "" {
		return fmt.Sprintf("%s (%s)", r.Serial, r.Model)
	}
	return r.Serial
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite supports only one writer at a time
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Insert saves a test run to the database.
func (s *Store) Insert(r Run) error {
	_, err := s.db.Exec(`
		INSERT INTO runs (serial, model, finished_at, passing, failing, pending, found, boot_ok, boot_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Serial,
		r.Model,
		r.FinishedAt.UTC().Format(time.RFC3339),
		r.Passing,
		r.Failing,
		r.Pending,
		boolToInt(r.Found),
		boolToInt(r.BootOK),
		r.BootSeconds,
	)
	return err
}

// List returns the most recent `limit` runs across all devices (newest first).
func (s *Store) List(serial string, limit int) ([]Run, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if serial == "" {
		rows, err = s.db.Query(`
			SELECT id, serial, model, finished_at, passing, failing, pending, found, boot_ok, boot_seconds
			FROM runs ORDER BY finished_at DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT id, serial, model, finished_at, passing, failing, pending, found, boot_ok, boot_seconds
			FROM runs WHERE serial = ? ORDER BY finished_at DESC LIMIT ?`, serial, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// Devices returns all unique serial numbers that have runs, ordered by most recent.
func (s *Store) Devices() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT serial FROM runs ORDER BY MAX(finished_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var serials []string
	for rows.Next() {
		var serial string
		if err := rows.Scan(&serial); err != nil {
			return nil, err
		}
		serials = append(serials, serial)
	}
	return serials, rows.Err()
}

func scanRuns(rows *sql.Rows) ([]Run, error) {
	var runs []Run
	for rows.Next() {
		var r Run
		var finishedAt string
		var found, bootOK int
		if err := rows.Scan(
			&r.ID, &r.Serial, &r.Model, &finishedAt,
			&r.Passing, &r.Failing, &r.Pending,
			&found, &bootOK, &r.BootSeconds,
		); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, finishedAt)
		r.FinishedAt = t.Local()
		r.Found = found != 0
		r.BootOK = bootOK != 0
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
