package pos

import (
	"context"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

func TestRosterVersion(t *testing.T) {
	pin := "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"
	base := []RosterEntry{{StaffID: "a", FullName: "Ana", Permissions: []string{"order.create"}}}
	v1, _ := rosterVersion(base)
	again, _ := rosterVersion([]RosterEntry{{StaffID: "a", FullName: "Ana", Permissions: []string{"order.create"}}})
	if v1 != again {
		t.Fatal("same roster produced different versions")
	}
	changes := map[string][]RosterEntry{
		"pin set":         {{StaffID: "a", FullName: "Ana", PINHash: &pin, Permissions: []string{"order.create"}}},
		"permission held": {{StaffID: "a", FullName: "Ana", Permissions: []string{"order.create", "order.void.paid"}}},
		"renamed":         {{StaffID: "a", FullName: "Anna", Permissions: []string{"order.create"}}},
		"staff removed":   {},
	}
	for name, entries := range changes {
		if v, _ := rosterVersion(entries); v == v1 {
			t.Errorf("%s: version did not change", name)
		}
	}
}

type fakeRepo struct{ recorded []PINFailure }

func (f *fakeRepo) Roster(_ context.Context, _ string) ([]RosterEntry, error) { return nil, nil }

func (f *fakeRepo) RecordPINFailures(_ context.Context, _ string, fs []PINFailure) error {
	f.recorded = fs
	return nil
}

func TestRecordPINFailuresRejectsFutureTimestamps(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	repo := &fakeRepo{}
	svc := &Service{repo: repo, now: func() time.Time { return now }}

	ok := []PINFailure{{ID: "1", StaffID: "s", OccurredAt: now.Add(-48 * time.Hour)}, {ID: "2", StaffID: "s", OccurredAt: now.Add(time.Minute)}}
	if err := svc.RecordPINFailures(context.Background(), "d", ok); err != nil {
		t.Fatalf("past and slightly skewed timestamps: %v", err)
	}
	future := []PINFailure{{ID: "3", StaffID: "s", OccurredAt: now.Add(time.Hour)}}
	err := svc.RecordPINFailures(context.Background(), "d", future)
	if e := domain.AsError(err); e == nil || e.Code != domain.CodeInvalidInput {
		t.Fatalf("future timestamp: got %v, want invalid_input", err)
	}
}
