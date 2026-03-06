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
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	serial        TEXT    NOT NULL,
	model         TEXT    NOT NULL,
	finished_at   TEXT    NOT NULL,
	passing       INTEGER NOT NULL DEFAULT 0,
	failing       INTEGER NOT NULL DEFAULT 0,
	pending       INTEGER NOT NULL DEFAULT 0,
	found         INTEGER NOT NULL DEFAULT 0,
	boot_ok       INTEGER NOT NULL DEFAULT 0,
	boot_seconds  REAL    NOT NULL DEFAULT 0,
	total_seconds REAL    NOT NULL DEFAULT 0,
	test_seconds  REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_runs_serial   ON runs(serial);
CREATE INDEX IF NOT EXISTS idx_runs_finished ON runs(finished_at);
`

// migrations adds columns to existing databases that predate the current schema.
var migrations = []string{
	`ALTER TABLE runs ADD COLUMN total_seconds REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE runs ADD COLUMN test_seconds  REAL NOT NULL DEFAULT 0`,
}

// Run holds the result of one test cycle for one device.
type Run struct {
	ID           int64
	Serial       string
	Model        string
	FinishedAt   time.Time
	Passing      int
	Failing      int
	Pending      int
	Found        bool
	BootOK       bool
	BootSeconds  float64
	TotalSeconds float64
	TestSeconds  float64
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
	// Apply migrations (ignore errors — column may already exist).
	for _, m := range migrations {
		db.Exec(m) //nolint:errcheck
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Insert saves a test run to the database.
func (s *Store) Insert(r Run) error {
	_, err := s.db.Exec(`
		INSERT INTO runs
		  (serial, model, finished_at, passing, failing, pending, found, boot_ok,
		   boot_seconds, total_seconds, test_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Serial,
		r.Model,
		r.FinishedAt.UTC().Format(time.RFC3339),
		r.Passing,
		r.Failing,
		r.Pending,
		boolToInt(r.Found),
		boolToInt(r.BootOK),
		r.BootSeconds,
		r.TotalSeconds,
		r.TestSeconds,
	)
	return err
}

// List returns the most recent `limit` runs (newest first).
// If serial is non-empty, only runs for that device are returned.
func (s *Store) List(serial string, limit int) ([]Run, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if serial == "" {
		rows, err = s.db.Query(`
			SELECT id, serial, model, finished_at, passing, failing, pending, found,
			       boot_ok, boot_seconds, total_seconds, test_seconds
			FROM runs ORDER BY finished_at DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT id, serial, model, finished_at, passing, failing, pending, found,
			       boot_ok, boot_seconds, total_seconds, test_seconds
			FROM runs WHERE serial = ? ORDER BY finished_at DESC LIMIT ?`, serial, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// DeviceStats holds aggregated statistics for one device.
type DeviceStats struct {
	Serial     string
	Model      string
	TotalRuns  int     // total number of completed test cycles
	FailedRuns int     // cycles where at least one test failed
	TotalTests int     // sum of passing + failing across all runs
	TotalFail  int     // sum of failing tests across all runs
	AvgBoot    float64 // average reboot time in seconds (successful reboots only)
	MinBoot    float64
	MaxBoot    float64
}

// PassRate returns the percentage of passing tests (0–100).
func (s DeviceStats) PassRate() float64 {
	if s.TotalTests == 0 {
		return 0
	}
	return float64(s.TotalTests-s.TotalFail) / float64(s.TotalTests) * 100
}

// DeviceLabel returns "serial (model)" or just "serial".
func (s DeviceStats) DeviceLabel() string {
	if s.Model != "" {
		return fmt.Sprintf("%s (%s)", s.Serial, s.Model)
	}
	return s.Serial
}

// Stats returns per-device aggregated statistics, ordered by most recent run.
func (s *Store) Stats() ([]DeviceStats, error) {
	rows, err := s.db.Query(`
		SELECT
			serial,
			MAX(model) AS model,
			COUNT(*)   AS total_runs,
			SUM(CASE WHEN found=1 AND failing>0 THEN 1 ELSE 0 END) AS failed_runs,
			SUM(passing + failing) AS total_tests,
			SUM(failing)           AS total_fail,
			AVG(CASE WHEN boot_ok=1 THEN boot_seconds END) AS avg_boot,
			MIN(CASE WHEN boot_ok=1 THEN boot_seconds END) AS min_boot,
			MAX(CASE WHEN boot_ok=1 THEN boot_seconds END) AS max_boot
		FROM runs
		GROUP BY serial
		ORDER BY MAX(finished_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []DeviceStats
	for rows.Next() {
		var st DeviceStats
		var avgBoot, minBoot, maxBoot sql.NullFloat64
		if err := rows.Scan(
			&st.Serial, &st.Model,
			&st.TotalRuns, &st.FailedRuns,
			&st.TotalTests, &st.TotalFail,
			&avgBoot, &minBoot, &maxBoot,
		); err != nil {
			return nil, err
		}
		st.AvgBoot = avgBoot.Float64
		st.MinBoot = minBoot.Float64
		st.MaxBoot = maxBoot.Float64
		stats = append(stats, st)
	}
	return stats, rows.Err()
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
			&found, &bootOK,
			&r.BootSeconds, &r.TotalSeconds, &r.TestSeconds,
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
