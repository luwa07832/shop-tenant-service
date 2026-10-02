package store

import (
	"context"
	"database/sql"
	"errors"
)

// SourceReported marks events posted through POST /v1/metering/events.
const SourceReported = "reported"

// SourceThrottle marks reservations created by allowed throttle checks.
const SourceThrottle = "throttle"

// RecordMeteringEvent appends an idempotent metering event. The append is
// conditional on the quota balance being sufficient.
func (s *Store) RecordMeteringEvent(ev MeteringEvent) (Quota, error) {
	if ev.Source == "" {
		ev.Source = SourceReported
	}
	err := withWriteTx(s.db, func(tx *sql.Conn) error {
		tenant, err := tenantForUpdate(tx, ev.TenantID)
		if err != nil {
			return err
		}
		if !writable(tenant.Status) {
			return ErrTenantSuspended
		}
		var existing int
		err = tx.QueryRowContext(context.Background(), `SELECT COUNT(1) FROM metering_events
			WHERE tenant_id = ? AND source = ? AND event_no = ?`,
			ev.TenantID, ev.Source, ev.EventNo).Scan(&existing)
		if err != nil {
			return err
		}
		if existing > 0 {
			return ErrDuplicateEvent
		}
		var limit int64
		err = tx.QueryRowContext(context.Background(), `SELECT limit_value FROM quotas
			WHERE tenant_id = ? AND period = ? AND resource = ?`,
			ev.TenantID, ev.Period, ev.Resource).Scan(&limit)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrQuotaNotFound
			}
			return err
		}
		used, err := usageForPeriod(tx, ev.TenantID, ev.Period)
		if err != nil {
			return err
		}
		if used[ev.Resource]+ev.Amount > limit {
			return ErrQuotaExceeded
		}
		_, err = tx.ExecContext(context.Background(), `INSERT INTO metering_events
			(tenant_id, source, event_no, period, resource, amount, occurred_at, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.TenantID, ev.Source, ev.EventNo, ev.Period, ev.Resource, ev.Amount,
			ev.OccurredAt, ev.OccurredAt)
		if isConstraint(err, "metering_events.") {
			return ErrDuplicateEvent
		}
		return err
	})
	if err != nil {
		return Quota{}, err
	}
	return s.GetQuota(ev.TenantID, ev.Period, ev.Resource)
}

// ThrottleCheckParams carries a throttle decision request.
type ThrottleCheckParams struct {
	TenantID string
	CheckNo  string
	Period   string
	Resource string
	Amount   int64
	Now      string
}

// CheckThrottle returns the stored decision for a repeated check number, or
// performs an atomic conditional reservation. The reservation and the saved
// result happen in one transaction, so concurrent checks never double spend.
func (s *Store) CheckThrottle(p ThrottleCheckParams) (ThrottleResult, error) {
	var result ThrottleResult
	err := withWriteTx(s.db, func(tx *sql.Conn) error {
		tenant, err := tenantForUpdate(tx, p.TenantID)
		if err != nil {
			return err
		}
		if !writable(tenant.Status) {
			return ErrTenantSuspended
		}
		var stored struct {
			allowed   int64
			remaining int64
			status    string
		}
		err = tx.QueryRowContext(context.Background(), `SELECT allowed, remaining, status FROM throttle_results
			WHERE tenant_id = ? AND check_no = ?`, p.TenantID, p.CheckNo).
			Scan(&stored.allowed, &stored.remaining, &stored.status)
		if err == nil {
			result = ThrottleResult{
				Allowed:   stored.allowed == 1,
				Remaining: stored.remaining,
				Status:    stored.status,
			}
			return nil
		}
		if err.Error() != "sql: no rows in result set" {
			return err
		}
		var limit int64
		err = tx.QueryRowContext(context.Background(), `SELECT limit_value FROM quotas
			WHERE tenant_id = ? AND period = ? AND resource = ?`,
			p.TenantID, p.Period, p.Resource).Scan(&limit)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrQuotaNotFound
			}
			return err
		}
		used, err := usageForPeriod(tx, p.TenantID, p.Period)
		if err != nil {
			return err
		}
		remaining := limit - used[p.Resource]
		allowed := remaining >= p.Amount
		allowedInt := int64(0)
		if allowed {
			remaining -= p.Amount
			allowedInt = 1
			_, err = tx.ExecContext(context.Background(), `INSERT INTO metering_events
				(tenant_id, source, event_no, period, resource, amount, occurred_at, recorded_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				p.TenantID, SourceThrottle, p.CheckNo, p.Period, p.Resource, p.Amount, p.Now, p.Now)
			if err != nil {
				return err
			}
		}
		if remaining < 0 {
			remaining = 0
		}
		status := StatusFor(limit, remaining)
		if !allowed {
			status = "exceeded"
		}
		_, err = tx.ExecContext(context.Background(), `INSERT INTO throttle_results
			(tenant_id, check_no, period, resource, amount, allowed, remaining, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.TenantID, p.CheckNo, p.Period, p.Resource, p.Amount, allowedInt, remaining, status, p.Now)
		if err != nil {
			return err
		}
		result = ThrottleResult{Allowed: allowed, Remaining: remaining, Status: status}
		return nil
	})
	if err != nil {
		return ThrottleResult{}, err
	}
	return result, nil
}
