// Package store owns the SQLite file and every write the service performs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Sentinel errors shared with the API layer. Messages are owned by the API layer
// so storage text never reaches clients.
var (
	ErrTenantNotFound  = errors.New("tenant not found")
	ErrQuotaNotFound   = errors.New("quota not found")
	ErrInvalidInput    = errors.New("invalid input")
	ErrQuotaExceeded   = errors.New("quota exceeded")
	ErrTenantSuspended = errors.New("tenant suspended")
	ErrTenantConflict  = errors.New("tenant already exists")
	ErrDuplicateEvent  = errors.New("duplicate metering event")
	ErrIllegalStatus   = errors.New("illegal tenant status transition")
)

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tenants (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	status      TEXT NOT NULL,
	plan_ref    TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS quotas (
	tenant_id  TEXT NOT NULL,
	period     TEXT NOT NULL,
	resource   TEXT NOT NULL,
	limit_value INTEGER NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (tenant_id, period, resource),
	FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);

CREATE TABLE IF NOT EXISTS quota_changes (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id     TEXT NOT NULL,
	period        TEXT NOT NULL,
	resource      TEXT NOT NULL,
	old_limit     INTEGER,
	new_limit     INTEGER NOT NULL,
	created_at    TEXT NOT NULL,
	FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);

CREATE TABLE IF NOT EXISTS metering_events (
	tenant_id  TEXT NOT NULL,
	source     TEXT NOT NULL,
	event_no   TEXT NOT NULL,
	period     TEXT NOT NULL,
	resource   TEXT NOT NULL,
	amount     INTEGER NOT NULL,
	occurred_at TEXT NOT NULL,
	recorded_at TEXT NOT NULL,
	PRIMARY KEY (tenant_id, source, event_no),
	FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);

CREATE TABLE IF NOT EXISTS throttle_results (
	tenant_id   TEXT NOT NULL,
	check_no    TEXT NOT NULL,
	period      TEXT NOT NULL,
	resource    TEXT NOT NULL,
	amount      INTEGER NOT NULL,
	allowed     INTEGER NOT NULL,
	remaining   INTEGER NOT NULL,
	status      TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	PRIMARY KEY (tenant_id, check_no),
	FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);
`

// Tenant is the stored tenant profile.
type Tenant struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	PlanRef   string `json:"plan_ref"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Quota is one (period, resource) quota line for a tenant.
type Quota struct {
	Period    string `json:"period"`
	Resource  string `json:"resource"`
	Limit     int64  `json:"limit"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
	Status    string `json:"status"`
}

// UsageLine aggregates one resource for a billing period.
type UsageLine struct {
	Period    string `json:"period"`
	Resource  string `json:"resource"`
	Used      int64  `json:"used"`
	Limit     int64  `json:"limit"`
	Remaining int64  `json:"remaining"`
	Status    string `json:"status"`
}

// QuotaChange records one append-only quota revision.
type QuotaChange struct {
	TenantID  string `json:"tenant_id"`
	Period    string `json:"period"`
	Resource  string `json:"resource"`
	OldLimit  *int64 `json:"old_limit,omitempty"`
	NewLimit  int64  `json:"new_limit"`
	CreatedAt string `json:"created_at"`
}

// MeteringEvent is one append-only metering record.
type MeteringEvent struct {
	TenantID   string `json:"tenant_id"`
	Source     string `json:"source,omitempty"`
	EventNo    string `json:"event_no"`
	Period     string `json:"period"`
	Resource   string `json:"resource"`
	Amount     int64  `json:"amount"`
	OccurredAt string `json:"event_time"`
}

// ThrottleResult is the persisted decision for one throttle check.
type ThrottleResult struct {
	Allowed   bool   `json:"allowed"`
	Remaining int64  `json:"remaining"`
	Status    string `json:"status"`
}

// StatusFor derives ok / warning / exceeded from remaining quota.
func StatusFor(limit, remaining int64) string {
	if remaining < 0 {
		return "exceeded"
	}
	if remaining*5 <= limit {
		return "warning"
	}
	return "ok"
}

func quotaLine(period, resource string, limit, used int64) Quota {
	remaining := limit - used
	return Quota{
		Period:    period,
		Resource:  resource,
		Limit:     limit,
		Used:      used,
		Remaining: remaining,
		Status:    StatusFor(limit, remaining),
	}
}

func isConstraint(err error, prefix string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, prefix)
}

func isForeignKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// withWriteTx serializes writers with BEGIN IMMEDIATE so conditional quota
// reservations cannot interleave and double-spend. Busy locks are retried.
func withWriteTx(db *sql.DB, fn func(c *sql.Conn) error) error {
	const attempts = 8
	var lastErr error
	for i := 0; i < attempts; i++ {
		conn, err := db.Conn(context.Background())
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			conn.Close()
			if isBusy(err) && i < attempts-1 {
				lastErr = err
				continue
			}
			return err
		}
		if err := fn(conn); err != nil {
			conn.ExecContext(context.Background(), "ROLLBACK")
			conn.Close()
			if isBusy(err) && i < attempts-1 {
				lastErr = err
				continue
			}
			return err
		}
		if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
			conn.Close()
			if isBusy(err) && i < attempts-1 {
				lastErr = err
				continue
			}
			return err
		}
		conn.Close()
		return nil
	}
	return lastErr
}

func isBusy(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "database is locked") ||
		strings.Contains(err.Error(), "SQLITE_BUSY"))
}
