package domain

import "testing"

func TestParseIdentifier(t *testing.T) {
	tests := []struct {
		raw  string
		kind IdentifierKind
		want string
	}{
		{"081234567890", IdentifierPhone, "+6281234567890"},
		{"6281234567890", IdentifierPhone, "+6281234567890"},
		{"+62 812-3456-7890", IdentifierPhone, "+6281234567890"},
		{" (0812) 3456 7890 ", IdentifierPhone, "+6281234567890"},
		{"+6591234567", IdentifierPhone, "+6591234567"},
		{"  Budi+promo@Example.COM ", IdentifierEmail, "budi+promo@example.com"},
		{"first.last@mail.co.id", IdentifierEmail, "first.last@mail.co.id"},
	}
	for _, tt := range tests {
		got, err := ParseIdentifier(tt.raw)
		if err != nil {
			t.Errorf("ParseIdentifier(%q) error: %v", tt.raw, err)
			continue
		}
		if got.Kind != tt.kind || got.Value != tt.want {
			t.Errorf("ParseIdentifier(%q) = %+v, want %s %q", tt.raw, got, tt.kind, tt.want)
		}
	}
}

func TestParseIdentifierRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		"", "12345", "812345678", "+62212345678", "0812", "+0812345678",
		"not-an-email@", "a@b", "Budi <budi@example.com>", "budi@@example.com",
	} {
		if _, err := ParseIdentifier(raw); err == nil {
			t.Errorf("ParseIdentifier(%q) accepted, want invalid_identifier", raw)
		} else if AsError(err).Code != CodeInvalidIdentifier {
			t.Errorf("ParseIdentifier(%q) code = %s", raw, AsError(err).Code)
		}
	}
}

func TestIdentifierMasked(t *testing.T) {
	phone := Identifier{Kind: IdentifierPhone, Value: "+6281234567789"}
	if got := phone.Masked(); got != "+62812****789" {
		t.Errorf("phone mask = %q", got)
	}
	email := Identifier{Kind: IdentifierEmail, Value: "aminah@domain.com"}
	if got := email.Masked(); got != "a***@domain.com" {
		t.Errorf("email mask = %q", got)
	}
}
