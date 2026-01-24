package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// TokenPayload contains the data stored in a signed WebSocket token
type TokenPayload struct {
	ParticipantID string `json:"pid"`
	RoomSlug      string `json:"room"`
	Name          string `json:"name"`
	ExpiresAt     int64  `json:"exp"`
}

// TokenManager handles creation and validation of signed tokens
type TokenManager struct {
	secret []byte
}

// NewTokenManager creates a new TokenManager with the given secret
func NewTokenManager(secret string) *TokenManager {
	return &TokenManager{
		secret: []byte(secret),
	}
}

var (
	// ErrInvalidToken is returned when a token cannot be decoded
	ErrInvalidToken = errors.New("invalid token format")
	// ErrTokenExpired is returned when a token has expired
	ErrTokenExpired = errors.New("token expired")
	// ErrInvalidSignature is returned when the token signature doesn't match
	ErrInvalidSignature = errors.New("invalid token signature")
)

// GenerateToken creates a signed token for WebSocket authentication
func (tm *TokenManager) GenerateToken(participantID, roomSlug, name string, duration time.Duration) (string, error) {
	payload := TokenPayload{
		ParticipantID: participantID,
		RoomSlug:      roomSlug,
		Name:          name,
		ExpiresAt:     time.Now().Add(duration).Unix(),
	}

	// Encode payload to JSON
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	// Create HMAC signature
	mac := hmac.New(sha256.New, tm.secret)
	mac.Write(payloadBytes)
	signature := mac.Sum(nil)

	// Combine payload and signature (payload.signature format)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadBytes)
	signatureB64 := base64.RawURLEncoding.EncodeToString(signature)

	return payloadB64 + "." + signatureB64, nil
}

// ValidateToken verifies a token and returns its payload
func (tm *TokenManager) ValidateToken(token string) (*TokenPayload, error) {
	// Split token into payload and signature
	var payloadB64, signatureB64 string
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			payloadB64 = token[:i]
			signatureB64 = token[i+1:]
			break
		}
	}

	if payloadB64 == "" || signatureB64 == "" {
		return nil, ErrInvalidToken
	}

	// Decode payload
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, ErrInvalidToken
	}

	// Decode signature
	signature, err := base64.RawURLEncoding.DecodeString(signatureB64)
	if err != nil {
		return nil, ErrInvalidToken
	}

	// Verify HMAC signature
	mac := hmac.New(sha256.New, tm.secret)
	mac.Write(payloadBytes)
	expectedSignature := mac.Sum(nil)

	if !hmac.Equal(signature, expectedSignature) {
		return nil, ErrInvalidSignature
	}

	// Parse payload
	var payload TokenPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, ErrInvalidToken
	}

	// Check expiration
	if time.Now().Unix() > payload.ExpiresAt {
		return nil, ErrTokenExpired
	}

	return &payload, nil
}
