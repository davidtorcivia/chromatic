package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"chromatic/internal/database"
)

// TestStreamKeyHandler_Create tests stream key creation
func TestStreamKeyHandler_Create(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := NewStreamKeyHandler(db)

	tests := []struct {
		name           string
		body           map[string]interface{}
		expectedStatus int
		checkResponse  func(t *testing.T, body []byte)
	}{
		{
			name: "valid stream key creation",
			body: map[string]interface{}{
				"name": "Main Studio",
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var key map[string]interface{}
				if err := json.Unmarshal(body, &key); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if key["name"] != "Main Studio" {
					t.Errorf("expected name 'Main Studio', got %v", key["name"])
				}
				if key["keyToken"] == nil || key["keyToken"] == "" {
					t.Error("expected keyToken to be generated")
				}
				if key["id"] == nil || key["id"] == "" {
					t.Error("expected id to be generated")
				}
			},
		},
		{
			name: "empty name",
			body: map[string]interface{}{
				"name": "",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "missing body",
			body:           nil,
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bodyBytes []byte
			if tt.body != nil {
				bodyBytes, _ = json.Marshal(tt.body)
			}

			req := httptest.NewRequest("POST", "/api/stream-keys", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			handler.Create(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, rr.Code, rr.Body.String())
			}

			if tt.checkResponse != nil && rr.Code == tt.expectedStatus {
				tt.checkResponse(t, rr.Body.Bytes())
			}
		})
	}
}

// TestStreamKeyHandler_List tests listing stream keys
func TestStreamKeyHandler_List(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := NewStreamKeyHandler(db)

	// Create some test keys
	names := []string{"Key 1", "Key 2", "Key 3"}
	for _, name := range names {
		body := map[string]interface{}{"name": name}
		bodyBytes, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/stream-keys", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.Create(rr, req)

		if rr.Code != http.StatusCreated {
			t.Fatalf("failed to create test key: %s", rr.Body.String())
		}
	}

	t.Run("list all keys", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/stream-keys", nil)
		rr := httptest.NewRecorder()
		handler.List(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}

		var keys []map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &keys)
		if len(keys) != 3 {
			t.Errorf("expected 3 keys, got %d", len(keys))
		}
	})
}

// TestStreamKeyHandler_Delete tests deleting stream keys
func TestStreamKeyHandler_Delete(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := NewStreamKeyHandler(db)

	// Create a test key
	createBody := map[string]interface{}{"name": "Delete Me"}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/stream-keys", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)

	var createdKey map[string]interface{}
	json.Unmarshal(createRR.Body.Bytes(), &createdKey)
	keyID := createdKey["id"].(string)

	tests := []struct {
		name           string
		keyID          string
		expectedStatus int
	}{
		{
			name:           "delete existing key",
			keyID:          keyID,
			expectedStatus: http.StatusNoContent,
		},
		{
			name:           "delete non-existing key",
			keyID:          "not-found-id",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("DELETE", "/api/stream-keys/"+tt.keyID, nil)
			req.SetPathValue("id", tt.keyID)

			rr := httptest.NewRecorder()
			handler.Delete(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}

	// Verify key is actually deleted
	listReq := httptest.NewRequest("GET", "/api/stream-keys", nil)
	listRR := httptest.NewRecorder()
	handler.List(listRR, listReq)

	var keys []map[string]interface{}
	json.Unmarshal(listRR.Body.Bytes(), &keys)
	if len(keys) != 0 {
		t.Errorf("expected 0 keys after deletion, got %d", len(keys))
	}
}

// TestStreamKeyHandler_ValidateKey tests key validation
func TestStreamKeyHandler_ValidateKey(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := NewStreamKeyHandler(db)

	// Create a test key
	createBody := map[string]interface{}{"name": "Validate Me"}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/stream-keys", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)

	var createdKey map[string]interface{}
	json.Unmarshal(createRR.Body.Bytes(), &createdKey)
	keyToken := createdKey["keyToken"].(string)

	tests := []struct {
		name     string
		token    string
		expected bool
	}{
		{
			name:     "valid token",
			token:    keyToken,
			expected: true,
		},
		{
			name:     "invalid token",
			token:    "invalid-token",
			expected: false,
		},
		{
			name:     "empty token",
			token:    "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := handler.ValidateKey(context.Background(), tt.token)
			if err != nil && tt.expected {
				t.Errorf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected ValidateKey(%q) = %v, got %v", tt.token, tt.expected, result)
			}
		})
	}
}

// TestStreamKeyHandler_UniqueTokens tests that generated tokens are unique
func TestStreamKeyHandler_UniqueTokens(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := NewStreamKeyHandler(db)

	tokens := make(map[string]bool)

	// Create multiple keys and verify tokens are unique
	for i := 0; i < 10; i++ {
		body := map[string]interface{}{"name": "Key"}
		bodyBytes, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/stream-keys", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.Create(rr, req)

		var key map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &key)
		token := key["keyToken"].(string)

		if tokens[token] {
			t.Errorf("duplicate token generated: %s", token)
		}
		tokens[token] = true
	}
}
