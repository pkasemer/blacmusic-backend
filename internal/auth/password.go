package auth

import "golang.org/x/crypto/bcrypt"

const bcryptCost = 12

// HashPassword returns a bcrypt hash. bcrypt only reads the first 72 bytes, so
// callers must reject longer passwords.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

// CheckPassword reports whether pw matches the bcrypt hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}
