package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const clockLeeway = 60 * time.Second

var (
	errTokenInvalid = domain.NewError(domain.CodeTokenInvalid, "Token is invalid.")
	errTokenExpired = domain.NewError(domain.CodeTokenExpired, "Token has expired.")
)

type accessClaims struct {
	jwt.RegisteredClaims
	Family   string `json:"fam"`
	OutletID string `json:"outlet_id,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
}

// JWK is one public key as published at /.well-known/jwks.json.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

// Signer issues and verifies ES256 access tokens. Verification accepts the
// current key and, during a rotation, the previous one.
type Signer struct {
	key     *ecdsa.PrivateKey
	kid     string
	issuer  string
	ttl     time.Duration
	keys    map[string]*ecdsa.PublicKey
	jwks    []JWK
	nowFunc func() time.Time
}

func NewSigner(privatePEM, publicPEM, previousPublicPEM, issuer string, ttl time.Duration) (*Signer, error) {
	priv, err := jwt.ParseECPrivateKeyFromPEM([]byte(privatePEM))
	if err != nil {
		return nil, fmt.Errorf("parse jwt private key: %w", err)
	}
	pub, err := jwt.ParseECPublicKeyFromPEM([]byte(publicPEM))
	if err != nil {
		return nil, fmt.Errorf("parse jwt public key: %w", err)
	}
	if priv.Curve != elliptic.P256() || !priv.PublicKey.Equal(pub) {
		return nil, errors.New("jwt keys must be a matching ECDSA P-256 pair")
	}

	s := &Signer{key: priv, issuer: issuer, ttl: ttl, keys: map[string]*ecdsa.PublicKey{}, nowFunc: time.Now}
	if s.kid, err = s.addKey(pub); err != nil {
		return nil, err
	}
	if previousPublicPEM != "" {
		prev, err := jwt.ParseECPublicKeyFromPEM([]byte(previousPublicPEM))
		if err != nil {
			return nil, fmt.Errorf("parse previous jwt public key: %w", err)
		}
		if _, err := s.addKey(prev); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// addKey registers a verification key under its RFC 7638 thumbprint.
func (s *Signer) addKey(pub *ecdsa.PublicKey) (string, error) {
	raw, err := pub.Bytes()
	if err != nil || len(raw) != 65 || pub.Curve != elliptic.P256() {
		return "", errors.New("jwt public key must be ECDSA P-256")
	}
	x := base64.RawURLEncoding.EncodeToString(raw[1:33])
	y := base64.RawURLEncoding.EncodeToString(raw[33:65])
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	kid := base64.RawURLEncoding.EncodeToString(sum[:])

	s.keys[kid] = pub
	s.jwks = append(s.jwks, JWK{Kty: "EC", Crv: "P-256", X: x, Y: y, Kid: kid, Use: "sig", Alg: "ES256"})
	return kid, nil
}

func (s *Signer) JWKS() []JWK { return s.jwks }

func (s *Signer) TTL() time.Duration { return s.ttl }

// Issue signs an access token. It carries identity only: roles and
// permissions are resolved per request so changes apply immediately.
func (s *Signer) Issue(aud domain.Audience, subject, family, outletID, deviceID string) (string, time.Time, error) {
	now := s.nowFunc()
	exp := now.Add(s.ttl)
	claims := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   subject,
			Audience:  jwt.ClaimStrings{string(aud)},
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		Family:   family,
		OutletID: outletID,
		DeviceID: deviceID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, exp, nil
}

// Verify validates signature, algorithm, issuer and expiry, and requires the
// token's single audience to be one of allowed. Every failure other than
// expiry is reported as token_invalid, never saying why.
func (s *Signer) Verify(raw string, allowed ...domain.Audience) (*domain.Principal, error) {
	var claims accessClaims
	_, err := jwt.ParseWithClaims(raw, &claims, s.keyFor,
		jwt.WithValidMethods([]string{jwt.SigningMethodES256.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithLeeway(clockLeeway),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(s.nowFunc),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, errTokenExpired
		}
		return nil, errTokenInvalid
	}
	if len(claims.Audience) != 1 || !slices.Contains(allowed, domain.Audience(claims.Audience[0])) {
		return nil, errTokenInvalid
	}
	if claims.Subject == "" || claims.ID == "" || claims.Family == "" {
		return nil, errTokenInvalid
	}
	return &domain.Principal{
		Subject:   claims.Subject,
		Audience:  domain.Audience(claims.Audience[0]),
		FamilyID:  claims.Family,
		TokenID:   claims.ID,
		ExpiresAt: claims.ExpiresAt.Time,
		OutletID:  claims.OutletID,
		DeviceID:  claims.DeviceID,
	}, nil
}

func (s *Signer) keyFor(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	key, ok := s.keys[kid]
	if !ok {
		return nil, errors.New("unknown kid")
	}
	return key, nil
}
