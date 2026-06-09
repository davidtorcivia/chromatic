package websocket

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"chromatic/internal/metrics"
)

func init() {
	// Ensure metrics is initialized for tests
	_ = metrics.Get()
}

// mockConn implements a minimal interface for testing without real WebSocket
type mockConn struct {
	closed bool
	mu     sync.Mutex
}

func (m *mockConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func newTestClient(id, name, roomSlug string, hub *Hub) *Client {
	return &Client{
		ID:       id,
		Name:     name,
		RoomSlug: roomSlug,
		Hub:      hub,
		Send:     make(chan []byte, 256),
		Done:     make(chan struct{}),
		Conn:     nil, // Will use mock in tests that need it
	}
}

// stopHub gracefully stops the hub without calling Shutdown (which needs real connections)
func stopHub(hub *Hub) {
	close(hub.done)
}

func TestHub_RegisterClient(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client := newTestClient("client-1", "Alice", "test-room", hub)

	hub.Register(client)

	// Allow time for registration
	time.Sleep(10 * time.Millisecond)

	if !hub.RoomExists("test-room") {
		t.Error("room should exist after client registration")
	}

	if hub.ClientCount("test-room") != 1 {
		t.Errorf("expected 1 client, got %d", hub.ClientCount("test-room"))
	}
}

func TestHub_UnregisterClient(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client := newTestClient("client-1", "Alice", "test-room", hub)

	hub.Register(client)
	time.Sleep(10 * time.Millisecond)

	hub.Unregister(client)
	time.Sleep(10 * time.Millisecond)

	if hub.RoomExists("test-room") {
		t.Error("room should be removed when empty")
	}
}

func TestHub_MultipleClients(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client1 := newTestClient("client-1", "Alice", "test-room", hub)
	client2 := newTestClient("client-2", "Bob", "test-room", hub)
	client3 := newTestClient("client-3", "Charlie", "other-room", hub)

	hub.Register(client1)
	hub.Register(client2)
	hub.Register(client3)

	time.Sleep(10 * time.Millisecond)

	if hub.ClientCount("test-room") != 2 {
		t.Errorf("expected 2 clients in test-room, got %d", hub.ClientCount("test-room"))
	}

	if hub.ClientCount("other-room") != 1 {
		t.Errorf("expected 1 client in other-room, got %d", hub.ClientCount("other-room"))
	}

	// Unregister one client
	hub.Unregister(client1)
	time.Sleep(10 * time.Millisecond)

	if hub.ClientCount("test-room") != 1 {
		t.Errorf("expected 1 client in test-room after unregister, got %d", hub.ClientCount("test-room"))
	}
}

func TestHub_Broadcast(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client1 := newTestClient("client-1", "Alice", "test-room", hub)
	client2 := newTestClient("client-2", "Bob", "test-room", hub)

	hub.Register(client1)
	hub.Register(client2)
	time.Sleep(10 * time.Millisecond)

	// Broadcast a message
	message := []byte(`{"type":"test","payload":"hello"}`)
	hub.Broadcast("test-room", message, "")

	// Both clients should receive the message
	select {
	case msg := <-client1.Send:
		if string(msg) != string(message) {
			t.Errorf("client1 received wrong message: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("client1 did not receive message")
	}

	select {
	case msg := <-client2.Send:
		if string(msg) != string(message) {
			t.Errorf("client2 received wrong message: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("client2 did not receive message")
	}
}

func TestHub_BroadcastWithExclude(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client1 := newTestClient("client-1", "Alice", "test-room", hub)
	client2 := newTestClient("client-2", "Bob", "test-room", hub)

	hub.Register(client1)
	hub.Register(client2)
	time.Sleep(10 * time.Millisecond)

	// Broadcast a message excluding client1
	message := []byte(`{"type":"test","payload":"hello"}`)
	hub.Broadcast("test-room", message, "client-1")

	// Only client2 should receive the message
	select {
	case msg := <-client2.Send:
		if string(msg) != string(message) {
			t.Errorf("client2 received wrong message: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("client2 did not receive message")
	}

	// client1 should NOT receive the message
	select {
	case <-client1.Send:
		t.Error("client1 should not have received the message (was excluded)")
	case <-time.After(50 * time.Millisecond):
		// Expected - client1 was excluded
	}
}

func TestHub_BroadcastJSON(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client := newTestClient("client-1", "Alice", "test-room", hub)
	hub.Register(client)
	time.Sleep(10 * time.Millisecond)

	payload := map[string]string{"message": "hello"}
	err := hub.BroadcastJSON("test-room", "chat:message", payload, "")
	if err != nil {
		t.Fatalf("BroadcastJSON failed: %v", err)
	}

	select {
	case msg := <-client.Send:
		var received Message
		if err := json.Unmarshal(msg, &received); err != nil {
			t.Fatalf("failed to unmarshal message: %v", err)
		}
		if received.Type != "chat:message" {
			t.Errorf("expected type 'chat:message', got '%s'", received.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("client did not receive message")
	}
}

func TestHub_SendTo(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client1 := newTestClient("client-1", "Alice", "test-room", hub)
	client2 := newTestClient("client-2", "Bob", "test-room", hub)

	hub.Register(client1)
	hub.Register(client2)
	time.Sleep(10 * time.Millisecond)

	// Send to specific client
	message := []byte(`{"type":"private","payload":"hello"}`)
	hub.SendTo("test-room", "client-1", message)

	// Only client1 should receive
	select {
	case msg := <-client1.Send:
		if string(msg) != string(message) {
			t.Errorf("client1 received wrong message: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("client1 did not receive message")
	}

	// client2 should NOT receive
	select {
	case <-client2.Send:
		t.Error("client2 should not have received the private message")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestHub_GetRoomClients(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client1 := newTestClient("client-1", "Alice", "test-room", hub)
	client2 := newTestClient("client-2", "Bob", "test-room", hub)

	hub.Register(client1)
	hub.Register(client2)
	time.Sleep(10 * time.Millisecond)

	clients := hub.GetRoomClients("test-room")
	if len(clients) != 2 {
		t.Errorf("expected 2 clients, got %d", len(clients))
	}

	// Check for nonexistent room
	clients = hub.GetRoomClients("nonexistent")
	if len(clients) != 0 {
		t.Errorf("expected 0 clients for nonexistent room, got %d", len(clients))
	}
}

func TestHub_GetClient(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	client := newTestClient("client-1", "Alice", "test-room", hub)
	hub.Register(client)
	time.Sleep(10 * time.Millisecond)

	found := hub.GetClient("test-room", "client-1")
	if found == nil {
		t.Error("expected to find client")
	}
	if found.Name != "Alice" {
		t.Errorf("expected name 'Alice', got '%s'", found.Name)
	}

	// Check for nonexistent client
	notFound := hub.GetClient("test-room", "nonexistent")
	if notFound != nil {
		t.Error("expected nil for nonexistent client")
	}
}

func TestHub_RoomExists(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	if hub.RoomExists("test-room") {
		t.Error("room should not exist before any clients join")
	}

	client := newTestClient("client-1", "Alice", "test-room", hub)
	hub.Register(client)
	time.Sleep(10 * time.Millisecond)

	if !hub.RoomExists("test-room") {
		t.Error("room should exist after client joins")
	}
}

func TestHub_ClientCount(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	if hub.ClientCount("test-room") != 0 {
		t.Error("empty room should have 0 clients")
	}

	client1 := newTestClient("client-1", "Alice", "test-room", hub)
	client2 := newTestClient("client-2", "Bob", "test-room", hub)

	hub.Register(client1)
	time.Sleep(10 * time.Millisecond)

	if hub.ClientCount("test-room") != 1 {
		t.Errorf("expected 1 client, got %d", hub.ClientCount("test-room"))
	}

	hub.Register(client2)
	time.Sleep(10 * time.Millisecond)

	if hub.ClientCount("test-room") != 2 {
		t.Errorf("expected 2 clients, got %d", hub.ClientCount("test-room"))
	}
}

func TestHub_ConcurrentAccess(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer stopHub(hub)

	var wg sync.WaitGroup
	numClients := 50

	// Concurrently register many clients
	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := newTestClient(
				string(rune('a'+id)),
				"Client",
				"test-room",
				hub,
			)
			hub.Register(client)
		}(i)
	}

	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	// Count should eventually be numClients
	count := hub.ClientCount("test-room")
	if count != numClients {
		t.Errorf("expected %d clients, got %d", numClients, count)
	}
}

func TestHub_BroadcastAfterShutdown(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	client := newTestClient("client-1", "Alice", "test-room", hub)
	hub.Register(client)

	hub.Shutdown()

	// Broadcast after shutdown must be a no-op: it must not block, panic,
	// or deliver messages.
	done := make(chan struct{})
	go func() {
		hub.Broadcast("test-room", []byte("late message"), "")
		if err := hub.BroadcastJSON("test-room", "test", map[string]string{"k": "v"}, ""); err != nil {
			t.Errorf("BroadcastJSON after shutdown returned error: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Broadcast blocked after shutdown")
	}

	select {
	case msg := <-client.Send:
		t.Errorf("client received message after shutdown: %s", msg)
	default:
		// Expected — nothing delivered
	}
}

func TestClient_MediaStateConcurrency(t *testing.T) {
	client := newTestClient("client-1", "Alice", "test-room", nil)
	client.SetVideoEnabled(true)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client.SetAudioEnabled(i%2 == 0)
			_ = client.AudioEnabled()
			client.SetVideoEnabled(i%2 == 1)
			_ = client.VideoEnabled()
		}(i)
	}
	wg.Wait()
}
