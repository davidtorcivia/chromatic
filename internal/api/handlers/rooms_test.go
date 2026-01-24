package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"chromatic/internal/database"
)

// TestRoomHandler_Create tests room creation
func TestRoomHandler_Create(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := NewRoomHandler(db)

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

	handler := NewRoomHandler(db)

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

	handler := NewRoomHandler(db)

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

	handler := NewRoomHandler(db)

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

	handler := NewRoomHandler(db)

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
			"password":      "secret",
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
				"password": "secret",
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

	handler := NewRoomHandler(db)

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

	handler := NewRoomHandler(db)

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

	t.Run("check status - not admitted", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/status-test/status/"+participantID, nil)
		req.SetPathValue("slug", "status-test")
		req.SetPathValue("id", participantID)

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

		rr := httptest.NewRecorder()
		handler.CheckParticipantStatus(rr, req)

		var status map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &status)
		if status["isAdmitted"] != true {
			t.Error("expected isAdmitted true after admission")
		}
	})

	t.Run("check status - invalid participant", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/rooms/status-test/status/invalid-id", nil)
		req.SetPathValue("slug", "status-test")
		req.SetPathValue("id", "invalid-id")

		rr := httptest.NewRecorder()
		handler.CheckParticipantStatus(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("expected status %d, got %d", http.StatusNotFound, rr.Code)
		}
	})
}

// TestRoomHandler_List tests listing rooms
func TestRoomHandler_List(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := NewRoomHandler(db)

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

	handler := NewRoomHandler(db)

	// Create a room with password
	createBody := map[string]interface{}{
		"slug":               "info-test",
		"name":               "Info Test Room",
		"password":           "secret",
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
