// Package auth handles BitDX Prop Firm's own login system — entirely
// separate from the exchange's Dex-Backend auth (PROP_FIRM_PLAN.md
// section 2: "own auth, separate JWT/session from Dex-Backend").
package auth

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// HashPassword and CheckPassword wrap bcrypt for pf_users.password_hash.
func HashPassword(plaintext string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	return string(hash), err
}

func CheckPassword(hash, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}

// GenerateCredentials produces a fresh username ("PF-XXXXXX") and a random
// password for a newly-provisioned account (PROP_FIRM_PLAN.md section 3).
// The plaintext password is returned exactly once — the caller must hand
// it back in the provisioning response and never log or store it, only
// its bcrypt hash.
func GenerateCredentials() (username, plaintextPassword string, err error) {
	usernameSuffix, err := randomDigits(6)
	if err != nil {
		return "", "", err
	}
	password, err := randomToken(12)
	if err != nil {
		return "", "", err
	}
	return "PF-" + usernameSuffix, password, nil
}

func randomDigits(n int) (string, error) {
	digits := "0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = digits[int(b)%len(digits)]
	}
	return string(out), nil
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	// base32 avoids ambiguous characters (0/O, 1/I) that plague copy-paste
	// of a one-time-revealed credential.
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)[:n], nil
}

// --- JWT session tokens for logged-in prop-firm traders ---

type Claims struct {
	UserID   string `json:"sub"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
}

func NewTokenIssuer(secret string, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: []byte(secret), ttl: ttl}
}

func (i *TokenIssuer) Issue(userID, username string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(i.ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(i.secret)
}

func (i *TokenIssuer) Verify(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return i.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}
