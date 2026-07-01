package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/database"
)

const roomsTestAdminToken = "rooms-test-admin-token"

var roomsTestTokenSecret = DeriveTokenSecret(roomsTestAdminToken)

func newTestRoomHandler(db *database.DB) *RoomHandler {
	cfg := &config.Config{AdminToken: roomsTestAdminToken}
	return NewRoomHandler(db, cfg, roomsTestTokenSecret)
}

func createJoinTokenForTest(t *testing.T, handler *RoomHandler, participantID, roomSlug string) string {
	t.Helper()

	token, err := handler.tokenManager.GenerateToken(participantID, roomSlug, "Test User", time.Hour)
	if err != nil {
		t.Fatalf("failed to create join token: %v", err)
	}
	return token
}

// TestRoomHandler_Create tests room creation
func TestRoomHandler_Create(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	tests := []struct {
		name           string
		body           map[string]interface{}
		expectedStatus int
		checkResponse  func(t *testing.T, body []byte)
	}{
		{
			name: "valid room creation",
			body: map[string]interface{}{
				"slug":               "test-room",
				"name":               "Test Room",
				"waitingRoomEnabled": true,
				"watermarkMode":      "text",
				"watermarkText":      "{{ name }}",
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				if err := json.Unmarshal(body, &room); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if room["slug"] != "test-room" {
					t.Errorf("expected slug 'test-room', got %v", room["slug"])
				}
				if room["name"] != "Test Room" {
					t.Errorf("expected name 'Test Room', got %v", room["name"])
				}
				if room["status"] != "pending" {
					t.Errorf("expected status 'pending', got %v", room["status"])
				}
			},
		},
		{
			name: "room with password",
			body: map[string]interface{}{
				"slug":          "password-room",
				"name":          "Password Room",
				"password":      "secret123",
				"watermarkMode": "none",
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["hasPassword"] != true {
					t.Errorf("expected hasPassword true, got %v", room["hasPassword"])
				}
			},
		},
		{
			name: "invalid slug - too short",
			body: map[string]interface{}{
				"slug":          "ab",
				"name":          "Test Room",
				"watermarkMode": "none",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "invalid slug - uppercase",
			body: map[string]interface{}{
				"slug":          "TestRoom",
				"name":          "Test Room",
				"watermarkMode": "none",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "empty name",
			body: map[string]interface{}{
				"slug":          "valid-slug",
				"name":          "",
				"watermarkMode": "none",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "watermark position and scale persist",
			body: map[string]interface{}{
				"slug":           "wm-pos-room",
				"name":           "Watermark Position Room",
				"watermarkMode":  "text",
				"watermarkText":  "{{ name }}",
				"watermarkPosX":  0.25,
				"watermarkPosY":  0.75,
				"watermarkScale": 2.0,
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["watermarkPosX"] != 0.25 {
					t.Errorf("expected watermarkPosX 0.25, got %v", room["watermarkPosX"])
				}
				if room["watermarkPosY"] != 0.75 {
					t.Errorf("expected watermarkPosY 0.75, got %v", room["watermarkPosY"])
				}
				if room["watermarkScale"] != 2.0 {
					t.Errorf("expected watermarkScale 2.0, got %v", room["watermarkScale"])
				}
			},
		},
		{
			name: "watermark position and scale clamped",
			body: map[string]interface{}{
				"slug":           "wm-clamp-room",
				"name":           "Watermark Clamp Room",
				"watermarkMode":  "text",
				"watermarkText":  "{{ name }}",
				"watermarkPosX":  1.5,
				"watermarkPosY":  -0.5,
				"watermarkScale": 10.0,
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["watermarkPosX"] != 1.0 {
					t.Errorf("expected watermarkPosX clamped to 1.0, got %v", room["watermarkPosX"])
				}
				if room["watermarkPosY"] != 0.0 {
					t.Errorf("expected watermarkPosY clamped to 0.0, got %v", room["watermarkPosY"])
				}
				if room["watermarkScale"] != 3.0 {
					t.Errorf("expected watermarkScale clamped to 3.0, got %v", room["watermarkScale"])
				}
			},
		},
		{
			name: "watermark scale defaults and position omitted",
			body: map[string]interface{}{
				"slug":          "wm-default-room",
				"name":          "Watermark Default Room",
				"watermarkMode": "text",
				"watermarkText": "{{ name }}",
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["watermarkScale"] != 1.0 {
					t.Errorf("expected default watermarkScale 1.0, got %v", room["watermarkScale"])
				}
				if _, ok := room["watermarkPosX"]; ok {
					t.Errorf("expected watermarkPosX omitted, got %v", room["watermarkPosX"])
				}
			},
		},
		{
			name: "valid participant limit",
			body: map[string]interface{}{
				"slug":            "limit-room",
				"name":            "Limit Room",
				"watermarkMode":   "none",
				"maxParticipants": 5,
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["maxParticipants"] != 5.0 {
					t.Errorf("expected maxParticipants 5, got %v", room["maxParticipants"])
				}
			},
		},
		{
			name: "participant limit too low",
			body: map[string]interface{}{
				"slug":            "limit-low-room",
				"name":            "Limit Low Room",
				"watermarkMode":   "none",
				"maxParticipants": 0,
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "participant limit too high",
			body: map[string]interface{}{
				"slug":            "limit-high-room",
				"name":            "Limit High Room",
				"watermarkMode":   "none",
				"maxParticipants": 101,
			},
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.body)
			req := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
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

// TestRoomHandler_Get tests getting a room by slug
func TestRoomHandler_Get(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create a test room first
	createBody := map[string]interface{}{
		"slug":          "get-test",
		"name":          "Get Test Room",
		"watermarkMode": "text",
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)

	if createRR.Code != http.StatusCreated {
		t.Fatalf("failed to create test room: %s", createRR.Body.String())
	}

	tests := []struct {
		name           string
		slug           string
		expectedStatus int
	}{
		{
			name:           "existing room",
			slug:           "get-test",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "non-existing room",
			slug:           "not-found",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/rooms/"+tt.slug, nil)
			req.SetPathValue("slug", tt.slug)

			rr := httptest.NewRecorder()
			handler.Get(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}
}

// TestRoomHandler_Update tests updating a room
func TestRoomHandler_Update(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create a test room
	createBody := map[string]interface{}{
		"slug":          "update-test",
		"name":          "Original Name",
		"watermarkMode": "none",
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)

	tests := []struct {
		name           string
		slug           string
		body           map[string]interface{}
		expectedStatus int
		checkResponse  func(t *testing.T, body []byte)
	}{
		{
			name: "update name",
			slug: "update-test",
			body: map[string]interface{}{
				"name": "Updated Name",
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["name"] != "Updated Name" {
					t.Errorf("expected name 'Updated Name', got %v", room["name"])
				}
			},
		},
		{
			name: "update multiple fields",
			slug: "update-test",
			body: map[string]interface{}{
				"name":               "Another Name",
				"waitingRoomEnabled": true,
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["name"] != "Another Name" {
					t.Errorf("expected name 'Another Name', got %v", room["name"])
				}
				if room["waitingRoomEnabled"] != true {
					t.Errorf("expected waitingRoomEnabled true, got %v", room["waitingRoomEnabled"])
				}
			},
		},
		{
			name: "update watermark position and scale",
			slug: "update-test",
			body: map[string]interface{}{
				"watermarkPosX":  0.5,
				"watermarkPosY":  0.25,
				"watermarkScale": 1.5,
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["watermarkPosX"] != 0.5 {
					t.Errorf("expected watermarkPosX 0.5, got %v", room["watermarkPosX"])
				}
				if room["watermarkPosY"] != 0.25 {
					t.Errorf("expected watermarkPosY 0.25, got %v", room["watermarkPosY"])
				}
				if room["watermarkScale"] != 1.5 {
					t.Errorf("expected watermarkScale 1.5, got %v", room["watermarkScale"])
				}
			},
		},
		{
			name: "update clamps watermark position and scale",
			slug: "update-test",
			body: map[string]interface{}{
				"watermarkPosX":  2.0,
				"watermarkPosY":  -1.0,
				"watermarkScale": 0.01,
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["watermarkPosX"] != 1.0 {
					t.Errorf("expected watermarkPosX clamped to 1.0, got %v", room["watermarkPosX"])
				}
				if room["watermarkPosY"] != 0.0 {
					t.Errorf("expected watermarkPosY clamped to 0.0, got %v", room["watermarkPosY"])
				}
				if room["watermarkScale"] != 0.25 {
					t.Errorf("expected watermarkScale clamped to 0.25, got %v", room["watermarkScale"])
				}
			},
		},
		{
			name: "clear watermark position",
			slug: "update-test",
			body: map[string]interface{}{
				"watermarkPosX": nil,
				"watermarkPosY": nil,
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if _, ok := room["watermarkPosX"]; ok {
					t.Errorf("expected watermarkPosX cleared, got %v", room["watermarkPosX"])
				}
				if _, ok := room["watermarkPosY"]; ok {
					t.Errorf("expected watermarkPosY cleared, got %v", room["watermarkPosY"])
				}
			},
		},
		{
			name: "update participant limit",
			slug: "update-test",
			body: map[string]interface{}{
				"maxParticipants": 42,
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if room["maxParticipants"] != 42.0 {
					t.Errorf("expected maxParticipants 42, got %v", room["maxParticipants"])
				}
			},
		},
		{
			name: "invalid participant limit rejected",
			slug: "update-test",
			body: map[string]interface{}{
				"maxParticipants": 200,
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "clear participant limit",
			slug: "update-test",
			body: map[string]interface{}{
				"maxParticipants": nil,
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var room map[string]interface{}
				json.Unmarshal(body, &room)
				if _, ok := room["maxParticipants"]; ok {
					t.Errorf("expected maxParticipants cleared, got %v", room["maxParticipants"])
				}
			},
		},
		{
			name: "update non-existing room",
			slug: "not-found",
			body: map[string]interface{}{
				"name": "Test",
			},
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.body)
			req := httptest.NewRequest("PATCH", "/api/rooms/"+tt.slug, bytes.NewReader(bodyBytes))
			req.SetPathValue("slug", tt.slug)
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			handler.Update(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, rr.Code, rr.Body.String())
			}

			if tt.checkResponse != nil && rr.Code == http.StatusOK {
				tt.checkResponse(t, rr.Body.Bytes())
			}
		})
	}
}

// TestRoomHandler_Delete tests deleting a room
func TestRoomHandler_Delete(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create a test room
	createBody := map[string]interface{}{
		"slug":          "delete-test",
		"name":          "Delete Me",
		"watermarkMode": "none",
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)

	tests := []struct {
		name           string
		slug           string
		expectedStatus int
	}{
		{
			name:           "delete existing room",
			slug:           "delete-test",
			expectedStatus: http.StatusNoContent,
		},
		{
			name:           "delete non-existing room",
			slug:           "not-found",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("DELETE", "/api/rooms/"+tt.slug, nil)
			req.SetPathValue("slug", tt.slug)

			rr := httptest.NewRecorder()
			handler.Delete(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}

	// Verify room is actually deleted
	getReq := httptest.NewRequest("GET", "/api/rooms/delete-test", nil)
	getReq.SetPathValue("slug", "delete-test")
	getRR := httptest.NewRecorder()
	handler.Get(getRR, getReq)

	if getRR.Code != http.StatusNotFound {
		t.Error("room should be deleted but still exists")
	}
}

// TestRoomHandler_Join tests joining a room
func TestRoomHandler_Join(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create rooms for testing
	rooms := []map[string]interface{}{
		{
			"slug":               "join-public",
			"name":               "Public Room",
			"watermarkMode":      "none",
			"waitingRoomEnabled": false,
		},
		{
			"slug":               "join-waiting",
			"name":               "Waiting Room",
			"watermarkMode":      "none",
			"waitingRoomEnabled": true,
		},
		{
			"slug":          "join-password",
			"name":          "Password Room",
			"password":      "secret12",
			"watermarkMode": "none",
		},
	}

	for _, room := range rooms {
		bodyBytes, _ := json.Marshal(room)
		req := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.Create(rr, req)
	}

	tests := []struct {
		name           string
		slug           string
		body           map[string]interface{}
		expectedStatus int
		checkResponse  func(t *testing.T, body []byte)
	}{
		{
			name: "join public room",
			slug: "join-public",
			body: map[string]interface{}{
				"name": "Test User",
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp map[string]interface{}
				json.Unmarshal(body, &resp)
				if resp["isAdmitted"] != true {
					t.Errorf("expected isAdmitted true for public room")
				}
				if resp["waitingRoom"] != false {
					t.Errorf("expected waitingRoom false for public room")
				}
			},
		},
		{
			name: "join waiting room",
			slug: "join-waiting",
			body: map[string]interface{}{
				"name": "Test User",
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp map[string]interface{}
				json.Unmarshal(body, &resp)
				if resp["isAdmitted"] != false {
					t.Errorf("expected isAdmitted false for waiting room")
				}
				if resp["waitingRoom"] != true {
					t.Errorf("expected waitingRoom true for waiting room")
				}
			},
		},
		{
			name: "join password room - correct password",
			slug: "join-password",
			body: map[string]interface{}{
				"name":     "Test User",
				"password": "secret12",
			},
			expectedStatus: http.StatusOK,
		},
		{
			name: "join password room - wrong password",
			slug: "join-password",
			body: map[string]interface{}{
				"name":     "Test User",
				"password": "wrong",
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "join password room - no password",
			slug: "join-password",
			body: map[string]interface{}{
				"name": "Test User",
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "join non-existing room",
			slug: "not-found",
			body: map[string]interface{}{
				"name": "Test User",
			},
			expectedStatus: http.StatusNotFound,
		},
		{
			name: "join with empty name",
			slug: "join-public",
			body: map[string]interface{}{
				"name": "",
			},
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.body)
			req := httptest.NewRequest("POST", "/api/rooms/"+tt.slug+"/join", bytes.NewReader(bodyBytes))
			req.SetPathValue("slug", tt.slug)
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			handler.Join(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, rr.Code, rr.Body.String())
			}

			if tt.checkResponse != nil && rr.Code == http.StatusOK {
				tt.checkResponse(t, rr.Body.Bytes())
			}
		})
	}
}

// TestRoomHandler_WaitingRoom tests waiting room management
func TestRoomHandler_WaitingRoom(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create a waiting room
	createBody := map[string]interface{}{
		"slug":               "waiting-test",
		"name":               "Waiting Test",
		"watermarkMode":      "none",
		"waitingRoomEnabled": true,
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)

	// Join the room
	joinBody := map[string]interface{}{
		"name": "Waiting User",
	}
	joinBytes, _ := json.Marshal(joinBody)
	joinReq := httptest.NewRequest("POST", "/api/rooms/waiting-test/join", bytes.NewReader(joinBytes))
	joinReq.SetPathValue("slug", "waiting-test")
	joinReq.Header.Set("Content-Type", "application/json")
	joinRR := httptest.NewRecorder()
	handler.Join(joinRR, joinReq)

	var joinResp map[string]interface{}
	json.Unmarshal(joinRR.Body.Bytes(), &joinResp)
	participantID := joinResp["participantId"].(string)

	// Test listing waiting room
	t.Run("list waiting participants", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/waiting-test/waiting", nil)
		req.SetPathValue("slug", "waiting-test")

		rr := httptest.NewRecorder()
		handler.ListWaiting(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}

		var participants []map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &participants)
		if len(participants) != 1 {
			t.Errorf("expected 1 participant, got %d", len(participants))
		}
	})

	// Test admitting participant
	t.Run("admit participant", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/rooms/waiting-test/admit/"+participantID, nil)
		req.SetPathValue("slug", "waiting-test")
		req.SetPathValue("id", participantID)

		rr := httptest.NewRecorder()
		handler.AdmitParticipant(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Errorf("expected status %d, got %d", http.StatusNoContent, rr.Code)
		}
	})

	// Verify waiting room is now empty
	t.Run("verify admitted", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/waiting-test/waiting", nil)
		req.SetPathValue("slug", "waiting-test")

		rr := httptest.NewRecorder()
		handler.ListWaiting(rr, req)

		var participants []map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &participants)
		if len(participants) != 0 {
			t.Errorf("expected 0 participants after admission, got %d", len(participants))
		}
	})
}

// TestRoomHandler_CheckParticipantStatus tests the participant status endpoint
func TestRoomHandler_CheckParticipantStatus(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create a waiting room
	createBody := map[string]interface{}{
		"slug":               "status-test",
		"name":               "Status Test",
		"watermarkMode":      "none",
		"waitingRoomEnabled": true,
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)

	// Join the room
	joinBody := map[string]interface{}{
		"name": "Status User",
	}
	joinBytes, _ := json.Marshal(joinBody)
	joinReq := httptest.NewRequest("POST", "/api/rooms/status-test/join", bytes.NewReader(joinBytes))
	joinReq.SetPathValue("slug", "status-test")
	joinReq.Header.Set("Content-Type", "application/json")
	joinRR := httptest.NewRecorder()
	handler.Join(joinRR, joinReq)

	var joinResp map[string]interface{}
	json.Unmarshal(joinRR.Body.Bytes(), &joinResp)
	participantID := joinResp["participantId"].(string)
	token := joinResp["token"].(string)

	t.Run("check status - not admitted", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/status-test/status/"+participantID, nil)
		req.SetPathValue("slug", "status-test")
		req.SetPathValue("id", participantID)
		req.Header.Set("X-Join-Token", token)

		rr := httptest.NewRecorder()
		handler.CheckParticipantStatus(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}

		var status map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &status)
		if status["isAdmitted"] != false {
			t.Error("expected isAdmitted false")
		}
	})

	// Admit the participant
	admitReq := httptest.NewRequest("POST", "/api/rooms/status-test/admit/"+participantID, nil)
	admitReq.SetPathValue("slug", "status-test")
	admitReq.SetPathValue("id", participantID)
	admitRR := httptest.NewRecorder()
	handler.AdmitParticipant(admitRR, admitReq)

	t.Run("check status - admitted", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/status-test/status/"+participantID, nil)
		req.SetPathValue("slug", "status-test")
		req.SetPathValue("id", participantID)
		req.Header.Set("X-Join-Token", token)

		rr := httptest.NewRecorder()
		handler.CheckParticipantStatus(rr, req)

		var status map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &status)
		if status["isAdmitted"] != true {
			t.Error("expected isAdmitted true after admission")
		}
	})

	t.Run("check status - invalid participant", func(t *testing.T) {
		invalidToken := createJoinTokenForTest(t, handler, "invalid-id", "status-test")
		req := httptest.NewRequest("GET", "/api/rooms/status-test/status/invalid-id", nil)
		req.SetPathValue("slug", "status-test")
		req.SetPathValue("id", "invalid-id")
		req.Header.Set("X-Join-Token", invalidToken)

		rr := httptest.NewRecorder()
		handler.CheckParticipantStatus(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("expected status %d, got %d", http.StatusNotFound, rr.Code)
		}
	})

	t.Run("check status - token in query param is rejected", func(t *testing.T) {
		// Credentials must not be accepted via query params (they get logged)
		req := httptest.NewRequest("GET", "/api/rooms/status-test/status/"+participantID, nil)
		req.SetPathValue("slug", "status-test")
		req.SetPathValue("id", participantID)
		req.URL.RawQuery = "token=" + url.QueryEscape(token)

		rr := httptest.NewRecorder()
		handler.CheckParticipantStatus(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d for query-param token, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("check status - missing token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/status-test/status/"+participantID, nil)
		req.SetPathValue("slug", "status-test")
		req.SetPathValue("id", participantID)

		rr := httptest.NewRecorder()
		handler.CheckParticipantStatus(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d for missing token, got %d", http.StatusUnauthorized, rr.Code)
		}
	})
}

// TestRoomHandler_List tests listing rooms
func TestRoomHandler_List(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create some test rooms
	rooms := []map[string]interface{}{
		{"slug": "list-1", "name": "Room 1", "watermarkMode": "none"},
		{"slug": "list-2", "name": "Room 2", "watermarkMode": "none"},
		{"slug": "list-3", "name": "Room 3", "watermarkMode": "none"},
	}

	for _, room := range rooms {
		bodyBytes, _ := json.Marshal(room)
		req := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.Create(rr, req)
	}

	t.Run("list all rooms", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms", nil)
		rr := httptest.NewRecorder()
		handler.List(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}

		var result []map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &result)
		if len(result) != 3 {
			t.Errorf("expected 3 rooms, got %d", len(result))
		}
	})

	t.Run("list by status", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms?status=pending", nil)
		rr := httptest.NewRecorder()
		handler.List(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}

		var result []map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &result)
		for _, room := range result {
			if room["status"] != "pending" {
				t.Errorf("expected status 'pending', got %v", room["status"])
			}
		}
	})
}

// TestRoomHandler_PublicInfo tests the public room info endpoint
func TestRoomHandler_PublicInfo(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create a room with password
	createBody := map[string]interface{}{
		"slug":               "info-test",
		"name":               "Info Test Room",
		"password":           "secret12",
		"watermarkMode":      "text",
		"waitingRoomEnabled": true,
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)

	t.Run("get public info", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/info-test/info", nil)
		req.SetPathValue("slug", "info-test")

		rr := httptest.NewRecorder()
		handler.PublicInfo(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}

		var info map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &info)

		if info["name"] != "Info Test Room" {
			t.Errorf("expected name 'Info Test Room', got %v", info["name"])
		}
		if info["hasPassword"] != true {
			t.Errorf("expected hasPassword true, got %v", info["hasPassword"])
		}
		if info["waitingRoomEnabled"] != true {
			t.Errorf("expected waitingRoomEnabled true, got %v", info["waitingRoomEnabled"])
		}
	})

	t.Run("get public info - not found", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/not-found/info", nil)
		req.SetPathValue("slug", "not-found")

		rr := httptest.NewRecorder()
		handler.PublicInfo(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("expected status %d, got %d", http.StatusNotFound, rr.Code)
		}
	})
}

// TestRoomHandler_AdminJoin tests joining a room with an admin token in the request body
func TestRoomHandler_AdminJoin(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	// Create a room with both a waiting room and a password — admins bypass both
	createBody := map[string]interface{}{
		"slug":               "admin-join",
		"name":               "Admin Join Room",
		"password":           "secret12",
		"watermarkMode":      "none",
		"waitingRoomEnabled": true,
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)
	if createRR.Code != http.StatusCreated {
		t.Fatalf("failed to create test room: %s", createRR.Body.String())
	}

	doJoin := func(t *testing.T, body map[string]interface{}) *httptest.ResponseRecorder {
		t.Helper()
		bodyBytes, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/rooms/admin-join/join", bytes.NewReader(bodyBytes))
		req.SetPathValue("slug", "admin-join")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.Join(rr, req)
		return rr
	}

	t.Run("valid admin token bypasses waiting room and password", func(t *testing.T) {
		rr := doJoin(t, map[string]interface{}{
			"name":       "Admin User",
			"adminToken": roomsTestAdminToken,
		})

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
		}

		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["role"] != "admin" {
			t.Errorf("expected role 'admin', got %v", resp["role"])
		}
		if resp["isAdmitted"] != true {
			t.Error("expected admin to be admitted immediately")
		}
		if resp["waitingRoom"] != false {
			t.Error("expected admin to bypass the waiting room")
		}

		// Verify the participant role was persisted
		var role string
		var isAdmitted bool
		err := db.QueryRow(`SELECT role, is_admitted FROM participants WHERE id = ?`, resp["participantId"]).Scan(&role, &isAdmitted)
		if err != nil {
			t.Fatalf("failed to look up participant: %v", err)
		}
		if role != "admin" || !isAdmitted {
			t.Errorf("expected persisted role=admin, is_admitted=true; got role=%s, is_admitted=%v", role, isAdmitted)
		}
	})

	t.Run("invalid admin token is rejected", func(t *testing.T) {
		rr := doJoin(t, map[string]interface{}{
			"name":       "Fake Admin",
			"adminToken": "wrong-token",
		})

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("viewer join still returns viewer role", func(t *testing.T) {
		rr := doJoin(t, map[string]interface{}{
			"name":     "Regular User",
			"password": "secret12",
		})

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
		}

		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["role"] != "viewer" {
			t.Errorf("expected role 'viewer', got %v", resp["role"])
		}
	})

	t.Run("admin token rejected when none configured", func(t *testing.T) {
		// Handler with no admin token configured must never grant admin
		noAdminHandler := NewRoomHandler(db, &config.Config{}, roomsTestTokenSecret)
		bodyBytes, _ := json.Marshal(map[string]interface{}{
			"name":       "Sneaky User",
			"adminToken": "",
		})
		req := httptest.NewRequest("POST", "/api/rooms/admin-join/join", bytes.NewReader(bodyBytes))
		req.SetPathValue("slug", "admin-join")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		noAdminHandler.Join(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})
}

// capTestHub is a stub hub for capacity tests: the participant cap counts
// LIVE connections (hub clients), not historical participant rows.
type capTestHub struct{ count int }

func (s *capTestHub) BroadcastJSON(string, string, interface{}, string) error { return nil }
func (s *capTestHub) SendToJSON(string, string, string, interface{}) error    { return nil }
func (s *capTestHub) BroadcastToAdminsJSON(string, string, interface{}) error { return nil }
func (s *capTestHub) RoomClientCount(string) int                              { return s.count }
func (s *capTestHub) CloseRoom(string)                                        {}

// TestRoomHandler_ParticipantCap tests the per-room participant limit
func TestRoomHandler_ParticipantCap(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	cfg := &config.Config{
		AdminToken:             roomsTestAdminToken,
		MaxParticipantsPerRoom: 2,
	}
	handler := NewRoomHandler(db, cfg, roomsTestTokenSecret)
	hub := &capTestHub{}
	handler.SetHub(hub)

	// Create a room
	createBody := map[string]interface{}{
		"slug":          "cap-test",
		"name":          "Cap Test Room",
		"watermarkMode": "none",
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)
	if createRR.Code != http.StatusCreated {
		t.Fatalf("failed to create test room: %s", createRR.Body.String())
	}

	join := func(t *testing.T, name string) *httptest.ResponseRecorder {
		t.Helper()
		bodyBytes, _ := json.Marshal(map[string]interface{}{"name": name})
		req := httptest.NewRequest("POST", "/api/rooms/cap-test/join", bytes.NewReader(bodyBytes))
		req.SetPathValue("slug", "cap-test")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.Join(rr, req)
		return rr
	}

	// First two joins succeed (0 then 1 live connection)
	for i, name := range []string{"User One", "User Two"} {
		hub.count = i
		rr := join(t, name)
		if rr.Code != http.StatusOK {
			t.Fatalf("join %d: expected status %d, got %d: %s", i+1, http.StatusOK, rr.Code, rr.Body.String())
		}
	}

	// With the room at capacity (2 live connections), the next join is rejected
	hub.count = 2
	rr := join(t, "User Three")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status %d when room is full, got %d", http.StatusServiceUnavailable, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Room is full") {
		t.Errorf("expected 'Room is full' in response, got %q", rr.Body.String())
	}

	// A leave/rejoin churn must NOT consume capacity: even after many
	// historical participant rows, joins succeed while live count is low.
	if _, err := db.Exec("UPDATE participants SET joined_at = ?", time.Now().Add(-joinReservationWindow*2)); err != nil {
		t.Fatalf("failed to age participant rows: %v", err)
	}
	hub.count = 1
	if rr := join(t, "Returning User"); rr.Code != http.StatusOK {
		t.Errorf("expected rejoin to succeed with free capacity, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRoomHandler_PerRoomParticipantLimit verifies that a room's
// max_participants overrides the global MaxParticipantsPerRoom cap.
func TestRoomHandler_PerRoomParticipantLimit(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	cfg := &config.Config{
		AdminToken:             roomsTestAdminToken,
		MaxParticipantsPerRoom: 20, // global default is higher than the room limit
	}
	handler := NewRoomHandler(db, cfg, roomsTestTokenSecret)
	hub := &capTestHub{}
	handler.SetHub(hub)

	// Create a room with a per-room limit of 1
	createBody := map[string]interface{}{
		"slug":            "room-limit-test",
		"name":            "Room Limit Test",
		"watermarkMode":   "none",
		"maxParticipants": 1,
	}
	bodyBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)
	if createRR.Code != http.StatusCreated {
		t.Fatalf("failed to create test room: %s", createRR.Body.String())
	}

	join := func(t *testing.T, name string) *httptest.ResponseRecorder {
		t.Helper()
		bodyBytes, _ := json.Marshal(map[string]interface{}{"name": name})
		req := httptest.NewRequest("POST", "/api/rooms/room-limit-test/join", bytes.NewReader(bodyBytes))
		req.SetPathValue("slug", "room-limit-test")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.Join(rr, req)
		return rr
	}

	// First join succeeds
	if rr := join(t, "User One"); rr.Code != http.StatusOK {
		t.Fatalf("expected first join to succeed, got %d: %s", rr.Code, rr.Body.String())
	}

	// Second join is rejected despite the global cap of 20
	hub.count = 1
	rr := join(t, "User Two")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status %d when room is full, got %d", http.StatusServiceUnavailable, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Room is full") {
		t.Errorf("expected 'Room is full' in response, got %q", rr.Body.String())
	}

	// Raising the per-room limit lets more participants in
	updateBody, _ := json.Marshal(map[string]interface{}{"maxParticipants": 2})
	updateReq := httptest.NewRequest("PATCH", "/api/rooms/room-limit-test", bytes.NewReader(updateBody))
	updateReq.SetPathValue("slug", "room-limit-test")
	updateReq.Header.Set("Content-Type", "application/json")
	updateRR := httptest.NewRecorder()
	handler.Update(updateRR, updateReq)
	if updateRR.Code != http.StatusOK {
		t.Fatalf("failed to update participant limit: %s", updateRR.Body.String())
	}

	if rr := join(t, "User Two"); rr.Code != http.StatusOK {
		t.Errorf("expected join to succeed after raising limit, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestDeriveTokenSecret tests the join-token secret derivation
func TestDeriveTokenSecret(t *testing.T) {
	adminToken := "super-secret-admin-token"
	secret := DeriveTokenSecret(adminToken)

	if len(secret) != 32 {
		t.Fatalf("expected 32-byte derived secret, got %d bytes", len(secret))
	}

	// The derived secret must never equal the raw admin token
	if string(secret) == adminToken {
		t.Error("derived secret must differ from the admin token")
	}

	// Derivation must be deterministic
	if string(DeriveTokenSecret(adminToken)) != string(secret) {
		t.Error("derivation must be deterministic")
	}

	// Different admin tokens must derive different secrets
	if string(DeriveTokenSecret("other-token")) == string(secret) {
		t.Error("different admin tokens must derive different secrets")
	}

	// A token signed with the derived secret must not validate against a
	// manager keyed with the raw admin token (and vice versa)
	derivedTM := NewTokenManager(secret)
	rawTM := NewTokenManager([]byte(adminToken))

	token, err := derivedTM.GenerateToken("pid-1", "room-1", "Name", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	if _, err := derivedTM.ValidateToken(token); err != nil {
		t.Errorf("token should validate with derived-secret manager: %v", err)
	}
	if _, err := rawTM.ValidateToken(token); err == nil {
		t.Error("token signed with derived secret must not validate with raw admin token secret")
	}
}
