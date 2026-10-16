package domain

import "time"

// Audience is the JWT aud claim. Each route group accepts exactly one, so a
// customer token can never reach a staff route by accident.
type Audience string

const (
	AudienceCustomer Audience = "customer"
	AudienceStaff    Audience = "staff"
	AudiencePOS      Audience = "pos"
)

func (a Audience) Valid() bool {
	return a == AudienceCustomer || a == AudienceStaff || a == AudiencePOS
}

// Principal is the authenticated caller resolved from an access token. It
// carries identity only; permissions are resolved per request.
type Principal struct {
	Subject   string
	Audience  Audience
	FamilyID  string
	TokenID   string
	ExpiresAt time.Time
	OutletID  string
	DeviceID  string
}
