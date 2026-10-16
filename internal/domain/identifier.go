package domain

import (
	"net/mail"
	"regexp"
	"strings"
)

type IdentifierKind string

const (
	IdentifierPhone IdentifierKind = "phone"
	IdentifierEmail IdentifierKind = "email"
)

// Identifier is a normalized phone number (E.164) or email address. Every
// lookup, Redis key and stored value uses the normalized form, so "0812…" and
// "+62812…" share one cooldown and one quota.
type Identifier struct {
	Kind  IdentifierKind
	Value string
}

var (
	e164Re       = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	idMobileRe   = regexp.MustCompile(`^\+628[0-9]{7,11}$`)
	phoneStripRe = regexp.MustCompile(`[\s\-().]`)
)

var ErrInvalidIdentifier = NewError(CodeInvalidIdentifier, "Enter a valid phone number or email address.")

// ParseIdentifier detects the identifier type from its format and normalizes
// it. Only the format is checked; existence is never revealed here.
func ParseIdentifier(raw string) (Identifier, error) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "@") {
		return parseEmail(s)
	}
	return parsePhone(s)
}

func parseEmail(s string) (Identifier, error) {
	s = strings.ToLower(s)
	if len(s) > 254 {
		return Identifier{}, ErrInvalidIdentifier
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Name != "" || addr.Address != s {
		return Identifier{}, ErrInvalidIdentifier
	}
	at := strings.LastIndexByte(s, '@')
	if !strings.Contains(s[at+1:], ".") {
		return Identifier{}, ErrInvalidIdentifier
	}
	return Identifier{Kind: IdentifierEmail, Value: s}, nil
}

// parsePhone maps Indonesian local formats (08…, 628…, +62 8…) to +628… and
// accepts any other number already in international + form.
func parsePhone(s string) (Identifier, error) {
	s = phoneStripRe.ReplaceAllString(s, "")
	switch {
	case strings.HasPrefix(s, "+"):
	case strings.HasPrefix(s, "0"):
		s = "+62" + s[1:]
	case strings.HasPrefix(s, "62"):
		s = "+" + s
	default:
		return Identifier{}, ErrInvalidIdentifier
	}
	if !e164Re.MatchString(s) {
		return Identifier{}, ErrInvalidIdentifier
	}
	if strings.HasPrefix(s, "+62") && !idMobileRe.MatchString(s) {
		return Identifier{}, ErrInvalidIdentifier
	}
	return Identifier{Kind: IdentifierPhone, Value: s}, nil
}

// Masked is the only form an identifier may take in logs:
// +62812****789, a***@domain.com.
func (id Identifier) Masked() string {
	if id.Kind == IdentifierEmail {
		at := strings.LastIndexByte(id.Value, '@')
		if at <= 0 {
			return "***"
		}
		return id.Value[:1] + "***" + id.Value[at:]
	}
	if len(id.Value) <= 9 {
		return "****"
	}
	return id.Value[:6] + "****" + id.Value[len(id.Value)-3:]
}
