package staff

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"slices"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"golang.org/x/crypto/argon2"
)

const (
	pinLength     = 6
	pinSaltBytes  = 16
	pinKeyBytes   = 32
	pinDigitsOnly = "must be exactly 6 digits"
)

var errWeakPIN = domain.NewError(domain.CodeWeakPIN,
	"This PIN is too easy to guess. Avoid repeated digits, sequences and common PINs.")

// PINConfig sets the PIN rules and the argon2id cost. The POS verifies the
// hash locally against the roster, so the cost must stay under 500 ms on
// POS hardware (NFR-PERF-4).
type PINConfig struct {
	Denylist  []string
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// checkPIN enforces FR-PIN-1 and FR-PIN-2: six digits, not all the same,
// not an ascending or descending run, and not on the denylist.
func checkPIN(pin string, denylist []string) error {
	if len(pin) != pinLength || !isDigits(pin) {
		return domain.ValidationError(map[string]string{"pin": pinDigitsOnly})
	}
	if isRun(pin, 0) || isRun(pin, 1) || isRun(pin, -1) || slices.Contains(denylist, pin) {
		return errWeakPIN
	}
	return nil
}

func isDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isRun reports whether each digit is the previous one plus step.
func isRun(pin string, step int) bool {
	for i := 1; i < len(pin); i++ {
		if int(pin[i])-int(pin[i-1]) != step {
			return false
		}
	}
	return true
}

// hashPIN returns an argon2id hash in the standard PHC string format
// ($argon2id$v=19$m=…,t=…,p=…$salt$hash), which carries its own parameters,
// so any argon2 library on the POS can verify it and the cost can change
// without invalidating older hashes.
func hashPIN(pin string, c PINConfig) (string, error) {
	salt := make([]byte, pinSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate pin salt: %w", err)
	}
	key := argon2.IDKey([]byte(pin), salt, c.Time, c.MemoryKiB, c.Threads, pinKeyBytes)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		c.MemoryKiB, c.Time, c.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}
