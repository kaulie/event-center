// Package verify implements inbound webhook authentication.
package verify

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"

	"github.com/kaulie/event-center/internal/model"
)

// Result describes how a request was authenticated.
type Result struct {
	// Method is the scheme that succeeded, e.g. "hmac_sha256".
	Method string
}

// HMACSHA256 checks an "sha256=<hex>" signature (GitHub style) over the raw
// body. It also accepts the legacy SHA-1 header when sha1Hex is provided.
func HMACSHA256(secret string, body []byte, signature, sha1Signature string) error {
	if secret == "" {
		return fmt.Errorf("source has no secret configured")
	}
	if signature == "" && sha1Signature == "" {
		return fmt.Errorf("missing signature header")
	}
	if signature != "" {
		expected := "sha256=" + sumHex(sha256.New, secret, body)
		if secureEqual(expected, signature) {
			return nil
		}
		return fmt.Errorf("sha256 signature mismatch")
	}
	expected := "sha1=" + sumHex(sha1.New, secret, body)
	if secureEqual(expected, sha1Signature) {
		return nil
	}
	return fmt.Errorf("sha1 signature mismatch")
}

// SignHMACSHA256 returns the "sha256=<hex>" signature for a body. It is used to
// sign outbound push deliveries and keeps producers and consumers symmetric.
func SignHMACSHA256(secret string, body []byte) string {
	return "sha256=" + sumHex(sha256.New, secret, body)
}

type hashFunc = func() hash.Hash

func sumHex(newHash hashFunc, secret string, body []byte) string {
	mac := hmac.New(newHash, []byte(secret))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Bearer checks an Authorization header against the configured token.
func Bearer(secret, authorization string) error {
	if secret == "" {
		return fmt.Errorf("source has no secret configured")
	}
	token := strings.TrimSpace(authorization)
	if len(token) > 7 && strings.EqualFold(token[:7], "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	if secureEqual(secret, token) {
		return nil
	}
	return fmt.Errorf("invalid bearer token")
}

// Mode dispatches to the verifier configured for a source.
func Mode(src *model.Source, body []byte, headers map[string]string) error {
	switch src.VerifyMode {
	case model.VerifyHMACSHA256:
		return HMACSHA256(src.Secret, body, headers["x-hub-signature-256"], headers["x-hub-signature"])
	case model.VerifyBearer:
		return Bearer(src.Secret, headers["authorization"])
	case model.VerifyNone, "":
		return nil
	default:
		return fmt.Errorf("unsupported verify mode %q", src.VerifyMode)
	}
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
