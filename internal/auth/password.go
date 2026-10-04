package auth

import (
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword bcrypts a password at the default cost.
func HashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(h), err
}

// VerifyPassword returns nil when plain matches the stored hash (legacy
// hashes are used unchanged).
func VerifyPassword(hash, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}

// dummyHash equalizes the time of a login for an unknown username with
// that of a known one.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("sms-gw-unknown-user"), bcrypt.DefaultCost)

func burnPasswordCheck(plain string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(plain)) }

// RandomPassword is a 32-character hex password (management resets).
func RandomPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
