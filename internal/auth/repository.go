package auth

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// An eligible staff member is active and holds at least one role, at an
// outlet or globally. Anyone else is treated exactly like an unknown number.
const (
	findEligibleStaffByPhone = `
		SELECT s.id, COALESCE(s.phone, ''), COALESCE(s.email, '')
		FROM staff s
		WHERE s.phone = $1
		  AND s.is_active
		  AND (EXISTS (SELECT 1 FROM staff_outlet_roles r WHERE r.staff_id = s.id)
		    OR EXISTS (SELECT 1 FROM staff_global_roles g WHERE g.staff_id = s.id))`

	findEligibleStaffByEmail = `
		SELECT s.id, COALESCE(s.phone, ''), COALESCE(s.email, '')
		FROM staff s
		WHERE s.email = $1
		  AND s.is_active
		  AND (EXISTS (SELECT 1 FROM staff_outlet_roles r WHERE r.staff_id = s.id)
		    OR EXISTS (SELECT 1 FROM staff_global_roles g WHERE g.staff_id = s.id))`

	findCustomerByPhone = `
		SELECT id, COALESCE(phone, ''), COALESCE(email, '')
		FROM customers WHERE phone = $1`

	findCustomerByEmail = `
		SELECT id, COALESCE(phone, ''), COALESCE(email, '')
		FROM customers WHERE email = $1`

	// First verify creates the account; later ones keep the original
	// verified_at.
	upsertVerifiedCustomerByPhone = `
		INSERT INTO customers (id, phone, verified_at)
		VALUES ($1, $2, now())
		ON CONFLICT (phone) DO UPDATE
		SET verified_at = COALESCE(customers.verified_at, EXCLUDED.verified_at),
		    updated_at  = now()
		RETURNING id`

	upsertVerifiedCustomerByEmail = `
		INSERT INTO customers (id, email, verified_at)
		VALUES ($1, $2, now())
		ON CONFLICT (email) DO UPDATE
		SET verified_at = COALESCE(customers.verified_at, EXCLUDED.verified_at),
		    updated_at  = now()
		RETURNING id`

	findStaffProfile = `
		SELECT id, COALESCE(phone, ''), COALESCE(email, ''), full_name
		FROM staff WHERE id = $1`

	findCustomerProfile = `
		SELECT id, COALESCE(phone, ''), COALESCE(email, ''), COALESCE(full_name, '')
		FROM customers WHERE id = $1`

	insertAuthEvent = `
		INSERT INTO auth_events
		  (id, subject_type, subject_id, identifier, event, device_id, ip_address, user_agent)
		VALUES
		  ($1, $2, NULLIF($3, '')::uuid, $4, $5, NULLIF($6, '')::uuid,
		   NULLIF($7, '')::inet, NULLIF($8, ''))`
)

type Repository struct {
	db *postgres.DB
}

func NewRepository(db *postgres.DB) *Repository { return &Repository{db: db} }

// FindEligibleStaff returns nil, nil when no eligible staff member matches.
func (r *Repository) FindEligibleStaff(ctx context.Context, id domain.Identifier) (*Subject, error) {
	q := findEligibleStaffByPhone
	if id.Kind == domain.IdentifierEmail {
		q = findEligibleStaffByEmail
	}
	return r.findSubject(ctx, q, id.Value, "staff")
}

// FindCustomer returns nil, nil when no customer has this identifier.
func (r *Repository) FindCustomer(ctx context.Context, id domain.Identifier) (*Subject, error) {
	q := findCustomerByPhone
	if id.Kind == domain.IdentifierEmail {
		q = findCustomerByEmail
	}
	return r.findSubject(ctx, q, id.Value, "customer")
}

func (r *Repository) findSubject(ctx context.Context, query, value, kind string) (*Subject, error) {
	var s Subject
	err := r.db.Pool.QueryRow(ctx, query, value).Scan(&s.ID, &s.Phone, &s.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find %s by identifier: %w", kind, err)
	}
	return &s, nil
}

// UpsertVerifiedCustomer creates the customer on first verify (signup) or
// marks an existing one verified, and returns its ID.
func (r *Repository) UpsertVerifiedCustomer(ctx context.Context, id domain.Identifier) (string, error) {
	q := upsertVerifiedCustomerByPhone
	if id.Kind == domain.IdentifierEmail {
		q = upsertVerifiedCustomerByEmail
	}
	var customerID string
	if err := r.db.Pool.QueryRow(ctx, q, uuid.NewString(), id.Value).Scan(&customerID); err != nil {
		return "", fmt.Errorf("upsert verified customer: %w", err)
	}
	return customerID, nil
}

// FindProfile returns nil, nil when the subject no longer exists.
func (r *Repository) FindProfile(ctx context.Context, aud domain.Audience, id string) (*Profile, error) {
	q := findCustomerProfile
	if aud == domain.AudienceStaff {
		q = findStaffProfile
	}
	var p Profile
	err := r.db.Pool.QueryRow(ctx, q, id).Scan(&p.ID, &p.Phone, &p.Email, &p.FullName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find %s profile: %w", aud, err)
	}
	return &p, nil
}

func (r *Repository) RecordEvent(ctx context.Context, e *Event) error {
	// X-Forwarded-For is client-supplied; store only a parseable address.
	ip := e.Meta.IP
	if net.ParseIP(ip) == nil {
		ip = ""
	}
	_, err := r.db.Pool.Exec(ctx, insertAuthEvent, uuid.NewString(), e.SubjectType,
		e.SubjectID, e.Identifier, e.Name, e.DeviceID, ip, e.Meta.UserAgent)
	if err != nil {
		return fmt.Errorf("insert auth event: %w", err)
	}
	return nil
}
