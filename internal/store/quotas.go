package store

import (
	"context"
	"database/sql"
)

func writable(status string) bool {
	return status == "active" || status == "provisioning"
}

// usageForPeriod returns consumed amounts by resource for a tenant/period across
// every event source (reported events and throttle reservations share the
// same billing-period ledger).
func usageForPeriod(q querier, tenantID, period string) (map[string]int64, error) {
	rows, err := q.QueryContext(context.Background(), `SELECT resource, COALESCE(SUM(amount), 0)
		FROM metering_events WHERE tenant_id = ? AND period = ? GROUP BY resource`,
		tenantID, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[string]int64{}
	for rows.Next() {
		var resource string
		var amount int64
		if err := rows.Scan(&resource, &amount); err != nil {
			return nil, err
		}
		used[resource] = amount
	}
	return used, rows.Err()
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// PutQuota appends a quota revision and recomputes the period balance.
func (s *Store) PutQuota(tenantID, period, resource string, limit int64, now string) (Quota, error) {
	err := withWriteTx(s.db, func(tx *sql.Conn) error {
		tenant, err := tenantForUpdate(tx, tenantID)
		if err != nil {
			return err
		}
		if !writable(tenant.Status) {
			return ErrTenantSuspended
		}
		var old sql.NullInt64
		err = tx.QueryRowContext(context.Background(), `SELECT limit_value FROM quotas
			WHERE tenant_id = ? AND period = ? AND resource = ?`,
			tenantID, period, resource).Scan(&old)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		_, err = tx.ExecContext(context.Background(), `INSERT INTO quotas (tenant_id, period, resource, limit_value, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(tenant_id, period, resource)
			DO UPDATE SET limit_value = excluded.limit_value, updated_at = excluded.updated_at`,
			tenantID, period, resource, limit, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(context.Background(), `INSERT INTO quota_changes (tenant_id, period, resource, old_limit, new_limit, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			tenantID, period, resource, nullableOld(old), limit, now)
		return err
	})
	if err != nil {
		return Quota{}, err
	}
	return s.GetQuota(tenantID, period, resource)
}

func nullableOld(old sql.NullInt64) any {
	if old.Valid {
		return old.Int64
	}
	return nil
}

// GetQuota returns one recomputed quota line.
func (s *Store) GetQuota(tenantID, period, resource string) (Quota, error) {
	var limit int64
	err := s.db.QueryRow(`SELECT limit_value FROM quotas
		WHERE tenant_id = ? AND period = ? AND resource = ?`,
		tenantID, period, resource).Scan(&limit)
	if err == sql.ErrNoRows {
		return Quota{}, ErrQuotaNotFound
	}
	if err != nil {
		return Quota{}, err
	}
	used, err := usageForPeriod(s.db, tenantID, period)
	if err != nil {
		return Quota{}, err
	}
	return quotaLine(period, resource, limit, used[resource]), nil
}

// ListQuotas returns every recomputed quota line for a period.
func (s *Store) ListQuotas(tenantID, period string) ([]Quota, error) {
	if _, err := s.GetTenant(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT resource, limit_value FROM quotas
		WHERE tenant_id = ? AND period = ? ORDER BY resource`, tenantID, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used, err := usageForPeriod(s.db, tenantID, period)
	if err != nil {
		return nil, err
	}
	var out []Quota
	for rows.Next() {
		var resource string
		var limit int64
		if err := rows.Scan(&resource, &limit); err != nil {
			return nil, err
		}
		out = append(out, quotaLine(period, resource, limit, used[resource]))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrQuotaNotFound
	}
	return out, nil
}

// Usage returns period aggregates in the same billing-period terms as quota reads.
func (s *Store) Usage(tenantID, period string) ([]UsageLine, error) {
	quotas, err := s.ListQuotas(tenantID, period)
	if err != nil {
		return nil, err
	}
	out := make([]UsageLine, 0, len(quotas))
	for _, q := range quotas {
		out = append(out, UsageLine{
			Period:    q.Period,
			Resource:  q.Resource,
			Used:      q.Used,
			Limit:     q.Limit,
			Remaining: q.Remaining,
			Status:    q.Status,
		})
	}
	return out, nil
}
