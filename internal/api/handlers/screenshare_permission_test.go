package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	gorillaws "github.com/gorilla/websocket"
)

func sendWS(t *testing.T, conn *gorillaws.Conn, msgType string, payload interface{}) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg, _ := json.Marshal(wsTestMessage{Type: msgType, Payload: raw})
	if err := conn.WriteMessage(gorillaws.TextMessage, msg); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestScreenSharePermission_PersistsAcrossRequests verifies BUG 3 behavior:
// the first request prompts the admin; once approved, the approval is
// persisted and later requests are auto-approved without prompting; revoke
// clears it and the next request prompts again.
func TestScreenSharePermission_PersistsAcrossRequests(t *testing.T) {
	env, cleanup := newRejoinTestEnv(t)
	defer cleanup()

	// Admin participant
	if _, err := env.db.Exec(`INSERT INTO participants (id, room_id, name, role, color, is_admitted) VALUES ('admin1', 'room1', 'Admin', 'admin', '#3a86ff', 1)`); err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	tm := NewTokenManager([]byte("test-secret-for-rejoin"))
	adminToken, err := tm.GenerateToken("admin1", env.slug, "Admin", time.Hour)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	adminConn := dialWith(t, env, adminToken, "Admin")
	defer adminConn.Close()
	waitForMessages(t, adminConn, 5*time.Second, "room:state")

	viewerConn := env.dial() // part1 (viewer)
	defer viewerConn.Close()
	waitForMessages(t, viewerConn, 5*time.Second, "room:state")

	// 1. First request: admin must receive screenshare:pending
	sendWS(t, viewerConn, "screenshare:request", map[string]interface{}{})
	pending := waitForMessages(t, adminConn, 5*time.Second, "screenshare:pending")
	var pendingData struct {
		ParticipantID string `json:"participantId"`
	}
	json.Unmarshal(pending["screenshare:pending"], &pendingData)
	if pendingData.ParticipantID != "part1" {
		t.Fatalf("pending for wrong participant: %s", pendingData.ParticipantID)
	}

	// 2. Admin approves: viewer gets approved, flag persisted, state broadcast
	sendWS(t, adminConn, "admin:screenshare-approve", map[string]interface{}{"participantId": "part1"})
	waitForMessages(t, viewerConn, 5*time.Second, "screenshare:approved")

	updated := waitForMessages(t, adminConn, 5*time.Second, "participant:updated")
	if !strings.Contains(string(updated["participant:updated"]), `"canScreenshare":true`) {
		t.Fatalf("participant:updated should carry canScreenshare=true: %s", updated["participant:updated"])
	}

	var allowed bool
	if err := env.db.QueryRow(`SELECT can_screenshare FROM participants WHERE id = 'part1'`).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("approval not persisted (allowed=%v, err=%v)", allowed, err)
	}

	// 3. Second request: auto-approved, admin NOT prompted
	sendWS(t, viewerConn, "screenshare:request", map[string]interface{}{})
	waitForMessages(t, viewerConn, 5*time.Second, "screenshare:approved")

	// Admin should receive no screenshare:pending now. Drain briefly.
	adminConn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	for {
		_, raw, err := adminConn.ReadMessage()
		if err != nil {
			break // deadline: no pending arrived
		}
		var msg wsTestMessage
		if json.Unmarshal(raw, &msg) == nil && msg.Type == "screenshare:pending" {
			t.Fatal("admin was re-prompted despite persistent approval")
		}
	}

	// Re-dial admin (the deadline above poisoned the gorilla conn for reads).
	adminConn2 := dialWith(t, env, adminToken, "Admin")
	defer adminConn2.Close()
	waitForMessages(t, adminConn2, 5*time.Second, "room:state")

	// 4. Revoke: flag cleared, viewer notified
	sendWS(t, adminConn2, "admin:screenshare-revoke", map[string]interface{}{"participantId": "part1"})
	waitForMessages(t, viewerConn, 5*time.Second, "screenshare:denied")
	if err := env.db.QueryRow(`SELECT can_screenshare FROM participants WHERE id = 'part1'`).Scan(&allowed); err != nil || allowed {
		t.Fatalf("approval not revoked (allowed=%v, err=%v)", allowed, err)
	}

	// 5. Next request prompts the admin again
	sendWS(t, viewerConn, "screenshare:request", map[string]interface{}{})
	waitForMessages(t, adminConn2, 5*time.Second, "screenshare:pending")
}

// TestRoomState_IncludesCanScreenshare verifies room:state carries the
// persisted flag so the admin UI can render per-row state.
func TestRoomState_IncludesCanScreenshare(t *testing.T) {
	env, cleanup := newRejoinTestEnv(t)
	defer cleanup()

	if _, err := env.db.Exec(`UPDATE participants SET can_screenshare = TRUE WHERE id = 'part1'`); err != nil {
		t.Fatalf("update: %v", err)
	}

	conn := env.dial()
	defer conn.Close()
	msgs := waitForMessages(t, conn, 5*time.Second, "room:state")

	var state struct {
		Participants []struct {
			ID             string `json:"id"`
			CanScreenshare bool   `json:"canScreenshare"`
		} `json:"participants"`
	}
	if err := json.Unmarshal(msgs["room:state"], &state); err != nil {
		t.Fatalf("parse room:state: %v", err)
	}
	found := false
	for _, p := range state.Participants {
		if p.ID == "part1" {
			found = true
			if !p.CanScreenshare {
				t.Fatal("room:state should report canScreenshare=true for part1")
			}
		}
	}
	if !found {
		t.Fatal("part1 missing from room:state participants")
	}
}

// TestLargeSignalingMessageDoesNotDisconnect guards the BUG 2 root cause:
// SDP renegotiation offers regularly exceed 8 KB; the server's read limit
// must accept them instead of killing the connection.
func TestLargeSignalingMessageDoesNotDisconnect(t *testing.T) {
	env, cleanup := newRejoinTestEnv(t)
	defer cleanup()

	conn := env.dial()
	defer conn.Close()
	waitForMessages(t, conn, 5*time.Second, "room:state")

	// A 64 KB signaling payload (larger than any realistic SDP). The SDP is
	// junk, so the server logs an error — but it must NOT drop the socket.
	bigSDP := strings.Repeat("a=candidate junk line padding\r\n", 64*1024/30)
	sendWS(t, conn, "signal:answer", map[string]interface{}{"sdp": bigSDP})

	// The connection must still be alive: a chat message should round-trip.
	sendWS(t, conn, "chat:send", map[string]interface{}{"content": "still alive"})
	msgs := waitForMessages(t, conn, 5*time.Second, "chat:message")
	if !strings.Contains(string(msgs["chat:message"]), "still alive") {
		t.Fatalf("unexpected chat payload: %s", msgs["chat:message"])
	}
}
