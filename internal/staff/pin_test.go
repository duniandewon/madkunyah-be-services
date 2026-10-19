package staff

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"golang.org/x/crypto/argon2"
)

func TestCheckPIN(t *testing.T) {
	denylist := []string{"121212"}
	tests := []struct {
		pin  string
		want domain.ErrorCode
	}{
		{"482913", ""},
		{"012345", domain.CodeWeakPIN},
		{"123456", domain.CodeWeakPIN},
		{"987654", domain.CodeWeakPIN},
		{"000000", domain.CodeWeakPIN},
		{"777777", domain.CodeWeakPIN},
		{"121212", domain.CodeWeakPIN},
		{"901234", ""}, // runs do not wrap around
		{"12345", domain.CodeInvalidInput},
		{"1234567", domain.CodeInvalidInput},
		{"12a456", domain.CodeInvalidInput},
	}
	for _, tt := range tests {
		if got := code(checkPIN(tt.pin, denylist)); got != tt.want {
			t.Errorf("checkPIN(%q): got %q, want %q", tt.pin, got, tt.want)
		}
	}
}

// TestHashPIN checks the hash is a standard PHC string that a POS-side
// argon2 implementation can verify from its own parameters.
func TestHashPIN(t *testing.T) {
	cfg := PINConfig{MemoryKiB: 1024, Time: 1, Threads: 1}
	h1, err := hashPIN("482913", cfg)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := hashPIN("482913", cfg)
	if h1 == h2 {
		t.Error("two hashes of one PIN are identical: salt is not random")
	}
	if !verifyPHC(t, h1, "482913") {
		t.Error("hash does not verify against its PIN")
	}
	if verifyPHC(t, h1, "482914") {
		t.Error("hash verifies against the wrong PIN")
	}
}

func verifyPHC(t *testing.T, encoded, pin string) bool {
	t.Helper()
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		t.Fatalf("not a PHC argon2id string: %q", encoded)
	}
	var m, tm uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &tm, &p); err != nil {
		t.Fatalf("parse params %q: %v", parts[3], err)
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	key, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		t.Fatalf("decode salt/key of %q", encoded)
	}
	got := argon2.IDKey([]byte(pin), salt, tm, m, p, uint32(len(key)))
	return bytes.Equal(got, key)
}
