package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// VerifyInternalSecret checks the X-Internal-Secret header on a
// server-to-server call (POST /internal/provision) against the shared
// secret configured via PROPFIRM_INTERNAL_SECRET — the transport
// mechanism PROP_FIRM_PLAN.md section 4/13 left open, resolved here as
// synchronous HTTP + a shared secret, the simplest option, swappable
// later without changing the provisioning contract itself.
//
// Constant-time comparison (hmac.Equal) avoids a timing side-channel on
// the secret.
func VerifyInternalSecret(r *http.Request, expected string) bool {
	if expected == "" {
		return false // refuse to run wide open if misconfigured
	}
	got := r.Header.Get("X-Internal-Secret")
	return hmac.Equal([]byte(sha256sum(got)), []byte(sha256sum(expected)))
}

func sha256sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
