// Package pos serves the POS device: the offline roster bundle and the
// events the device records while offline.
package pos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

// futureLeeway tolerates POS clock drift on event timestamps.
const futureLeeway = 5 * time.Minute

// RosterEntry is what the POS needs to unlock a staff member and check
// step-up permissions offline (FR-DEV-4). PINHash is nil until a PIN is set.
type RosterEntry struct {
	StaffID     string   `json:"staff_id"`
	FullName    string   `json:"full_name"`
	PINHash     *string  `json:"pin_hash"`
	Permissions []string `json:"permissions"`
}

type Roster struct {
	OutletID string        `json:"outlet_id"`
	Version  string        `json:"version"`
	Staff    []RosterEntry `json:"staff"`
}

// PINFailure is one wrong PIN entered on the device (FR-PIN-7). ID is
// generated on the device so a replayed batch is not recorded twice.
type PINFailure struct {
	ID         string
	StaffID    string
	OccurredAt time.Time
}

type repository interface {
	Roster(ctx context.Context, outletID string) ([]RosterEntry, error)
	RecordPINFailures(ctx context.Context, deviceID string, failures []PINFailure) error
}

type Service struct {
	repo repository
	now  func() time.Time
}

func NewService(repo repository) *Service { return &Service{repo: repo, now: time.Now} }

// Roster returns the outlet's roster. Its version is a hash of the contents,
// so it changes exactly when anything the POS relies on changes: a PIN, a
// role, a name, or who is active.
func (s *Service) Roster(ctx context.Context, outletID string) (*Roster, error) {
	entries, err := s.repo.Roster(ctx, outletID)
	if err != nil {
		return nil, err
	}
	version, err := rosterVersion(entries)
	if err != nil {
		return nil, err
	}
	return &Roster{OutletID: outletID, Version: version, Staff: entries}, nil
}

// rosterVersion hashes entries, which arrive in a fixed order with sorted
// permissions.
func rosterVersion(entries []RosterEntry) (string, error) {
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("encode roster: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16]), nil
}

// RecordPINFailures stores wrong-PIN events synced from the device. They
// are recorded for reporting only and never block anyone.
func (s *Service) RecordPINFailures(ctx context.Context, deviceID string, failures []PINFailure) error {
	limit := s.now().Add(futureLeeway)
	for i, f := range failures {
		if f.OccurredAt.After(limit) {
			return domain.ValidationError(map[string]string{
				fmt.Sprintf("failures[%d].occurred_at", i): "must not be in the future",
			})
		}
	}
	return s.repo.RecordPINFailures(ctx, deviceID, failures)
}
