package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	lockCustomer = `
		SELECT id, COALESCE(phone, ''), COALESCE(email, ''), COALESCE(full_name, '')
		FROM customers WHERE id = $1
		FOR UPDATE`

	setCustomerPhone = `UPDATE customers SET phone = $2, updated_at = now() WHERE id = $1`

	setCustomerEmail = `UPDATE customers SET email = $2, updated_at = now() WHERE id = $1`

	insertCustomerAudit = `
		INSERT INTO audit_logs (id, actor_id, actor_type, action, entity_type, entity_id, before, after)
		VALUES ($1, $2, 'customer', 'customer.identifier_changed', 'customer', $2, $3, $4)`
)

// identifierState is the audited before/after of an identifier change. Only
// the changed type is set; an add has an empty before.
type identifierState struct {
	Phone string `json:"phone,omitempty"`
	Email string `json:"email,omitempty"`
}

// SetCustomerIdentifier replaces the customer's phone or email with id and
// audits the change, in one transaction. Setting the value they already
// have is a no-op. The unique constraint is the final word on whether
// another customer holds it.
func (r *Repository) SetCustomerIdentifier(ctx context.Context, customerID string, id domain.Identifier) (*Profile, error) {
	var p Profile
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, lockCustomer, customerID).Scan(&p.ID, &p.Phone, &p.Email, &p.FullName)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock customer: %w", err)
		}

		q, current := setCustomerPhone, &p.Phone
		if id.Kind == domain.IdentifierEmail {
			q, current = setCustomerEmail, &p.Email
		}
		if *current == id.Value {
			return nil
		}
		before, after := identifierState{}, identifierState{}
		if id.Kind == domain.IdentifierEmail {
			before.Email, after.Email = p.Email, id.Value
		} else {
			before.Phone, after.Phone = p.Phone, id.Value
		}

		if _, err := tx.Exec(ctx, q, customerID, id.Value); err != nil {
			if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgUniqueViolation {
				return errIdentifierInUse
			}
			return fmt.Errorf("set customer identifier: %w", err)
		}
		*current = id.Value
		if _, err := tx.Exec(ctx, insertCustomerAudit, uuid.NewString(), customerID, before, after); err != nil {
			return fmt.Errorf("insert customer audit log: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}
