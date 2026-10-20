package auth

import (
	"context"
	"log/slog"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
)

// subjectDevice is the auth_events subject type for a POS device.
const subjectDevice = "device"

const errSessionsNotUpdated = "Change saved, but sessions could not be updated. Retry the request."

var (
	errDeviceLimit = domain.NewError(domain.CodeDeviceLimit,
		"This outlet already has its maximum number of active devices. Set replace to revoke the oldest.")
	errDeviceElsewhere = domain.NewError(domain.CodeConflict,
		"This device is enrolled at another outlet. Revoke it there first.")
)

type EnrollInput struct {
	Identifier  string
	Code        string
	OutletID    string
	Name        string
	Fingerprint string
	Replace     bool
}

// Enrollment is one device enrollment, applied in a single transaction.
type Enrollment struct {
	// DeviceID is the ID a new row gets; a re-enrolled fingerprint keeps its
	// original ID so its queued offline operations still match (FR-DEV-6).
	DeviceID    string
	OutletID    string
	Name        string
	Fingerprint string
	StaffID     string
	Replace     bool
	Limit       int
}

// DeviceRef identifies a device in auth events.
type DeviceRef struct {
	ID          string
	Fingerprint string
}

func (d DeviceRef) identifier() string {
	if d.Fingerprint != "" {
		return d.Fingerprint
	}
	return d.ID
}

type EnrollResult struct {
	DeviceID string
	// Existed is true when the fingerprint was already enrolled, so any
	// sessions it still holds must end before the new one starts.
	Existed  bool
	Replaced []DeviceRef
}

type EnrolledDevice struct {
	DeviceID string
	OutletID string
	Tokens   *TokenPair
}

// EnrollDevice redeems a device_enroll code, binds the device to the outlet,
// and opens its first pos session (FR-DEV-1..3).
func (s *Service) EnrollDevice(ctx context.Context, in EnrollInput, meta RequestMeta) (*EnrolledDevice, error) {
	id, err := domain.ParseIdentifier(in.Identifier)
	if err != nil {
		return nil, err
	}
	if err := s.allowVerify(ctx, meta.IP); err != nil {
		return nil, err
	}
	staffID, err := s.redeemEnrollCode(ctx, id, &in, meta)
	if err != nil {
		return nil, err
	}
	res, err := s.deps.Devices.EnrollDevice(ctx, &Enrollment{
		DeviceID: uuid.NewString(), OutletID: in.OutletID, Name: in.Name, Fingerprint: in.Fingerprint,
		StaffID: staffID, Replace: in.Replace, Limit: s.cfg.DeviceLimit,
	})
	if err != nil {
		return nil, err
	}
	if err := s.endOldDeviceSessions(ctx, res, meta); err != nil {
		return nil, err
	}
	ref := DeviceRef{ID: res.DeviceID, Fingerprint: in.Fingerprint}
	s.record(ctx, deviceEvent(ref, "device.enrolled", meta))
	pair, err := s.startSession(ctx, &newSession{Audience: domain.AudiencePOS, SubjectID: res.DeviceID,
		Identifier: ref.identifier(), DeviceID: res.DeviceID, OutletID: in.OutletID, Meta: meta})
	if err != nil {
		return nil, err
	}
	return &EnrolledDevice{DeviceID: res.DeviceID, OutletID: in.OutletID, Tokens: pair}, nil
}

// redeemEnrollCode checks the code and the enroller's device.enroll
// permission, then the device limit, and only then consumes the code: a
// 409 device_limit_reached can be retried with replace using the same code.
// Every code or permission failure is the same otp_invalid.
func (s *Service) redeemEnrollCode(ctx context.Context, id domain.Identifier, in *EnrollInput, meta RequestMeta) (string, error) {
	stored, match, err := s.matchCode(ctx, PurposeDeviceEnroll, id, "", in.Code)
	if err != nil {
		return "", unavailable(err)
	}
	var enroller *Subject
	if match {
		if enroller, err = s.findEnroller(ctx, id, in.OutletID); err != nil {
			return "", err
		}
	}
	ev := &Event{SubjectType: string(domain.AudienceStaff), Identifier: id.Value, Name: "otp.failed", Meta: meta}
	if enroller == nil {
		if staff, err := s.deps.Subjects.FindEligibleStaff(ctx, id); err == nil {
			ev.SubjectID = subjectID(staff)
		}
		s.record(ctx, ev)
		return "", errOTPInvalid
	}
	if err := s.checkDeviceLimit(ctx, in); err != nil {
		return "", err
	}
	consumed, err := s.consumeCode(ctx, PurposeDeviceEnroll, id, stored)
	if err != nil {
		return "", unavailable(err)
	}
	ev.SubjectID = enroller.ID
	if consumed {
		ev.Name = "otp.verified"
	}
	s.record(ctx, ev)
	if !consumed {
		return "", errOTPInvalid
	}
	return enroller.ID, nil
}

// checkDeviceLimit is an early, read-only check so the code survives a
// limit error. The enrollment transaction checks again under a lock.
func (s *Service) checkDeviceLimit(ctx context.Context, in *EnrollInput) error {
	if in.Replace {
		return nil
	}
	n, err := s.deps.Devices.CountOtherActiveDevices(ctx, in.OutletID, in.Fingerprint)
	if err != nil {
		return err
	}
	if n >= s.cfg.DeviceLimit {
		return errDeviceLimit
	}
	return nil
}

// endOldDeviceSessions runs after the enrollment commits. The device's own
// earlier sessions must end before its new one starts, or no session is
// issued. Replaced devices are refused by Postgres on their next refresh
// regardless, so failing to clear their Redis sessions is only logged.
func (s *Service) endOldDeviceSessions(ctx context.Context, res *EnrollResult, meta RequestMeta) error {
	for _, d := range res.Replaced {
		if err := s.deps.Revoker.RevokeDevice(ctx, d.ID); err != nil {
			slog.ErrorContext(ctx, "revoke replaced device sessions", "device_id", d.ID, "error", err)
		}
		s.record(ctx, deviceEvent(d, "session.revoked", meta))
	}
	if !res.Existed {
		return nil
	}
	if err := s.deps.Revoker.RevokeSubject(ctx, string(domain.AudiencePOS), res.DeviceID); err != nil {
		return unavailable(err)
	}
	return nil
}

// RevokeDevice deactivates a device and ends its sessions (FR-DEV-5). Its
// next refresh, and any pos request, gets device_revoked. Repeating it
// re-runs the session revocation, so a partial failure can be retried.
func (s *Service) RevokeDevice(ctx context.Context, actor *domain.StaffAccess, deviceID string, meta RequestMeta) error {
	outletID, err := s.deps.Tracker.DeviceOutlet(ctx, deviceID)
	if err != nil {
		return err
	}
	if err := actor.CheckOutlet(outletID, domain.PermDeviceRevoke); err != nil {
		return err
	}
	fingerprint, changed, err := s.deps.Devices.RevokeDevice(ctx, actor.StaffID, deviceID)
	if err != nil {
		return err
	}
	if err := s.deps.Revoker.RevokeDevice(ctx, deviceID); err != nil {
		return domain.WrapError(domain.CodeAuthUnavailable, errSessionsNotUpdated, err)
	}
	if changed {
		s.record(ctx, deviceEvent(DeviceRef{ID: deviceID, Fingerprint: fingerprint}, "session.revoked", meta))
	}
	return nil
}

func deviceEvent(d DeviceRef, name string, meta RequestMeta) *Event {
	return &Event{SubjectType: subjectDevice, SubjectID: d.ID, Identifier: d.identifier(),
		Name: name, DeviceID: d.ID, Meta: meta}
}
