package auth

import (
	"context"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

var errIdentifierInUse = domain.NewError(domain.CodeIdentifierInUse,
	"This phone number or email is already used by another account.")

type IdentifierInput struct {
	Identifier string
	Code       string
}

// RequestIdentifierOTP sends a code to a phone or email the customer wants
// to add or switch to (FR-CUS-2). The code is bound to the customer, so only
// they can redeem it. Whether another account already holds the identifier
// is checked only at redeem time, after ownership is proven, so this cannot
// be used to probe for accounts.
func (s *Service) RequestIdentifierOTP(ctx context.Context, customerID, identifier string, meta RequestMeta) error {
	id, err := domain.ParseIdentifier(identifier)
	if err != nil {
		return err
	}
	now := s.now()
	quotaDay := now.In(s.cfg.QuotaLocation).Format(time.DateOnly)
	if err := s.checkRequestLimits(ctx, id, meta.IP, quotaDay, now); err != nil {
		return err
	}

	code, err := generateCode(s.cfg.OTPLength)
	if err != nil {
		return err
	}
	mac := s.codeMAC(PurposeCustomerIdentifier, id, customerID, code)
	if err := s.deps.OTPs.SaveOTP(ctx, string(PurposeCustomerIdentifier), id.Value, mac, now, s.cfg.OTPTTL); err != nil {
		return unavailable(err)
	}
	s.record(ctx, &Event{SubjectType: string(domain.AudienceCustomer), SubjectID: customerID,
		Identifier: id.Value, Name: "otp.requested", Meta: meta})
	// No email fallback: the code must reach the identifier being proven.
	s.deps.Dispatcher.Dispatch(&DispatchJob{To: id, Code: code, QuotaDay: quotaDay})
	return nil
}

// AddIdentifier redeems the code and sets the customer's phone or email to
// the verified value: an add if they had none of that type, otherwise a
// change (FR-CUS-2, FR-CUS-3). The old value is replaced only here, after
// the new one verifies. There is no merge: a value held by another customer
// is identifier_in_use.
func (s *Service) AddIdentifier(ctx context.Context, customerID string, in IdentifierInput, meta RequestMeta) (*Profile, error) {
	id, err := domain.ParseIdentifier(in.Identifier)
	if err != nil {
		return nil, err
	}
	if err := s.allowVerify(ctx, meta.IP); err != nil {
		return nil, err
	}
	ok, err := s.checkCode(ctx, PurposeCustomerIdentifier, id, customerID, in.Code)
	if err != nil {
		return nil, unavailable(err)
	}
	ev := &Event{SubjectType: string(domain.AudienceCustomer), SubjectID: customerID,
		Identifier: id.Value, Name: "otp.failed", Meta: meta}
	if !ok {
		s.record(ctx, ev)
		return nil, errOTPInvalid
	}
	ev.Name = "otp.verified"
	s.record(ctx, ev)
	return s.deps.Identifiers.SetCustomerIdentifier(ctx, customerID, id)
}
