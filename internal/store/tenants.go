package store

import (
	"context"
	"database/sql"
	"errors"
)

// validStatuses is the closed tenant lifecycle vocabulary.
var validStatuses = map[string]bool{
	"provisioning": true,
	"active":       true,
	"suspended":    true,
	"archived":     true,
}

// allowedTransitions lists every legal lifecycle move. A state may always be
// restated (idempotent update).
var allowedTransitions = map[string]map[string]bool{
	"provisioning": {"active": true, "suspended": true, "archived": true},
	"active":       {"suspended": true, "archived": true},
	"suspended":    {"active": true, "archived": true},
	"archived":     {},
}

// tenantForUpdate loads a tenant and locks its row inside a write transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func tenantForUpdate(tx rowQuerier, id string) (Tenant, error) {
	row := tx.QueryRowContext(context.Background(), `SELECT id, name, status, plan_ref, created_at, updated_at
		FROM tenants WHERE id = ?`, id)
	return scanTenant(row)
}

func scanTenant(row *sql.Row) (Tenant, error) {
	var t Tenant
	if err := row.Scan(&t.ID, &t.Name, &t.Status, &t.PlanRef, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Tenant{}, ErrTenantNotFound
		}
		return Tenant{}, err
	}
	return t, nil
}

// CreateTenantParams carries a profile creation request.
type CreateTenantParams struct {
	ID      string
	Name    string
	Status  string
	PlanRef string
	Now     string
}

// CreateTenant inserts a new tenant profile.
func (s *Store) CreateTenant(p CreateTenantParams) (Tenant, error) {
	if !validStatuses[p.Status] {
		return Tenant{}, ErrInvalidInput
	}
	err := withWriteTx(s.db, func(tx *sql.Conn) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO tenants (id, name, status, plan_ref, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, p.ID, p.Name, p.Status, p.PlanRef, p.Now, p.Now)
		if isConstraint(err, "tenants.") {
			return ErrTenantConflict
		}
		return err
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.GetTenant(p.ID)
}

// GetTenant returns the stored profile for id.
func (s *Store) GetTenant(id string) (Tenant, error) {
	row := s.db.QueryRow(`SELECT id, name, status, plan_ref, created_at, updated_at
		FROM tenants WHERE id = ?`, id)
	return scanTenant(row)
}

// UpdateTenantParams carries an optional profile patch.
type UpdateTenantParams struct {
	ID      string
	Name    *string
	Status  *string
	PlanRef *string
	Now     string
}

// UpdateTenant applies legal status transitions and optional profile fields.
func (s *Store) UpdateTenant(p UpdateTenantParams) (Tenant, error) {
	if p.Status != nil && !validStatuses[*p.Status] {
		return Tenant{}, ErrInvalidInput
	}
	err := withWriteTx(s.db, func(tx *sql.Conn) error {
		current, err := tenantForUpdate(tx, p.ID)
		if err != nil {
			return err
		}
		if p.Status != nil && *p.Status != current.Status {
			if !allowedTransitions[current.Status][*p.Status] {
				return ErrIllegalStatus
			}
		}
		name := current.Name
		if p.Name != nil {
			name = *p.Name
		}
		status := current.Status
		if p.Status != nil {
			status = *p.Status
		}
		planRef := current.PlanRef
		if p.PlanRef != nil {
			planRef = *p.PlanRef
		}
		_, err = tx.ExecContext(context.Background(), `UPDATE tenants SET name = ?, status = ?, plan_ref = ?, updated_at = ? WHERE id = ?`,
			name, status, planRef, p.Now, p.ID)
		return err
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.GetTenant(p.ID)
}
