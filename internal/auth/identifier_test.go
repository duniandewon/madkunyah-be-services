package auth

import (
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

func TestCodeMACBinding(t *testing.T) {
	s := &Service{cfg: Config{Pepper: []byte("pepper")}}
	id := domain.Identifier{Kind: domain.IdentifierPhone, Value: "+6281234567890"}

	alice := s.codeMAC(PurposeCustomerIdentifier, id, "alice", "123456")
	if alice != s.codeMAC(PurposeCustomerIdentifier, id, "alice", "123456") {
		t.Fatal("same inputs gave different MACs")
	}
	if alice == s.codeMAC(PurposeCustomerIdentifier, id, "bob", "123456") {
		t.Error("code bound to one customer matches another")
	}
	if alice == s.codeMAC(PurposeCustomerIdentifier, id, "", "123456") {
		t.Error("bound code matches unbound")
	}
	if s.codeMAC(PurposeCustomerLogin, id, "", "123456") == s.codeMAC(PurposeCustomerIdentifier, id, "", "123456") {
		t.Error("purposes share a MAC")
	}
}

func TestOpensSession(t *testing.T) {
	cases := map[Purpose]bool{
		PurposeCustomerLogin:      true,
		PurposeStaffLogin:         true,
		PurposeDeviceEnroll:       false,
		PurposeCustomerIdentifier: false,
	}
	for p, want := range cases {
		if got := p.opensSession(); got != want {
			t.Errorf("%s.opensSession() = %v, want %v", p, got, want)
		}
	}
}
