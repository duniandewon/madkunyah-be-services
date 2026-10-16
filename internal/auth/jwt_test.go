package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/golang-jwt/jwt/v5"
)

func testKeyPEMs(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	priv := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return string(priv), string(pub)
}

func testSigner(t *testing.T) *Signer {
	t.Helper()
	priv, pub := testKeyPEMs(t)
	s, err := NewSigner(priv, pub, "", "test-issuer", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func errCode(err error) domain.ErrorCode {
	if de := domain.AsError(err); de != nil {
		return de.Code
	}
	return ""
}

// AC-11: required claims, ES256, 15-minute lifetime, no role claims.
func TestIssueClaims(t *testing.T) {
	s := testSigner(t)
	raw, _, err := s.Issue(domain.AudienceStaff, "sub-1", "fam-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Method.Alg() != "ES256" || tok.Header["kid"] != s.kid {
		t.Errorf("header = %v", tok.Header)
	}
	c := tok.Claims.(jwt.MapClaims)
	for _, k := range []string{"iss", "aud", "sub", "jti", "iat", "exp", "fam"} {
		if _, ok := c[k]; !ok {
			t.Errorf("missing claim %q", k)
		}
	}
	for _, k := range []string{"roles", "role", "permissions", "perms"} {
		if _, ok := c[k]; ok {
			t.Errorf("unexpected claim %q", k)
		}
	}
	if c["exp"].(float64)-c["iat"].(float64) != 900 {
		t.Errorf("exp - iat = %v", c["exp"].(float64)-c["iat"].(float64))
	}
}

// AC-15 (unit level): a token for one audience is token_invalid on another.
func TestVerifyAudience(t *testing.T) {
	s := testSigner(t)
	auds := []domain.Audience{domain.AudienceCustomer, domain.AudienceStaff, domain.AudiencePOS}
	for _, issued := range auds {
		raw, _, _ := s.Issue(issued, "sub", "fam", "", "")
		for _, route := range auds {
			_, err := s.Verify(raw, route)
			if issued == route && err != nil {
				t.Errorf("%s on %s: %v", issued, route, err)
			}
			if issued != route && errCode(err) != domain.CodeTokenInvalid {
				t.Errorf("%s on %s: got %v, want token_invalid", issued, route, err)
			}
		}
	}
}

// AC-17: alg none, HS256 keyed with the public key, and unknown kid are all
// rejected.
func TestVerifyRejectsForgedTokens(t *testing.T) {
	s := testSigner(t)
	_, pubPEM := testKeyPEMs(t)
	now := time.Now()
	claims := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "test-issuer", Subject: "sub", ID: "jti",
			Audience:  jwt.ClaimStrings{"staff"},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
		Family: "fam",
	}

	none := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	none.Header["kid"] = s.kid
	noneRaw, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)

	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	hs.Header["kid"] = s.kid
	hsRaw, _ := hs.SignedString([]byte(pubPEM))

	otherPriv, otherPub := testKeyPEMs(t)
	foreign, err := NewSigner(otherPriv, otherPub, "", "test-issuer", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	foreignRaw, _, _ := foreign.Issue(domain.AudienceStaff, "sub", "fam", "", "")

	for name, raw := range map[string]string{"none": noneRaw, "hs256": hsRaw, "unknown kid": foreignRaw} {
		if _, err := s.Verify(raw, domain.AudienceStaff); errCode(err) != domain.CodeTokenInvalid {
			t.Errorf("%s: got %v, want token_invalid", name, err)
		}
	}
}

func TestVerifyExpired(t *testing.T) {
	s := testSigner(t)
	s.nowFunc = func() time.Time { return time.Now().Add(-time.Hour) }
	raw, _, _ := s.Issue(domain.AudienceCustomer, "sub", "fam", "", "")
	s.nowFunc = time.Now
	if _, err := s.Verify(raw, domain.AudienceCustomer); errCode(err) != domain.CodeTokenExpired {
		t.Errorf("got %v, want token_expired", err)
	}
}

func TestVerifyAcceptsPreviousKey(t *testing.T) {
	oldPriv, oldPub := testKeyPEMs(t)
	old, err := NewSigner(oldPriv, oldPub, "", "test-issuer", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := old.Issue(domain.AudienceStaff, "sub", "fam", "", "")

	newPriv, newPub := testKeyPEMs(t)
	rotated, err := NewSigner(newPriv, newPub, oldPub, "test-issuer", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Verify(raw, domain.AudienceStaff); err != nil {
		t.Errorf("token from retired key rejected: %v", err)
	}
	if len(rotated.JWKS()) != 2 {
		t.Errorf("jwks has %d keys, want 2", len(rotated.JWKS()))
	}
}
