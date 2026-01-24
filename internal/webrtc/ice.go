package webrtc

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

// generateTURNCredentials generates time-limited TURN credentials using HMAC-SHA1
// This implements the coturn ephemeral credentials mechanism
func generateTURNCredentials(secret, realm string) (username string, credential string) {
	// Credential expires in 24 hours
	expiry := time.Now().Add(24 * time.Hour).Unix()

	// Username format: "timestamp:realm"
	username = fmt.Sprintf("%d:%s", expiry, realm)

	// HMAC-SHA1 of username with shared secret
	h := hmac.New(sha1.New, []byte(secret))
	h.Write([]byte(username))
	credential = base64.StdEncoding.EncodeToString(h.Sum(nil))

	return username, credential
}
