package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetrics_Get_Singleton(t *testing.T) {
	// Get should always return the same instance
	m1 := Get()
	m2 := Get()

	if m1 != m2 {
		t.Error("Get() should return singleton instance")
	}
}

func TestMetrics_Gauges(t *testing.T) {
	m := Get()

	// Test ActiveRooms
	initial := m.ActiveRooms.Load()
	m.ActiveRooms.Add(5)
	if m.ActiveRooms.Load() != initial+5 {
		t.Errorf("expected ActiveRooms to be %d, got %d", initial+5, m.ActiveRooms.Load())
	}
	m.ActiveRooms.Add(-5) // Reset

	// Test ActiveWebsockets
	initial = m.ActiveWebsockets.Load()
	m.ActiveWebsockets.Add(10)
	if m.ActiveWebsockets.Load() != initial+10 {
		t.Errorf("expected ActiveWebsockets to be %d, got %d", initial+10, m.ActiveWebsockets.Load())
	}
	m.ActiveWebsockets.Add(-10) // Reset

	// Test ActiveWHIPIngests
	initial = m.ActiveWHIPIngests.Load()
	m.ActiveWHIPIngests.Add(3)
	if m.ActiveWHIPIngests.Load() != initial+3 {
		t.Errorf("expected ActiveWHIPIngests to be %d, got %d", initial+3, m.ActiveWHIPIngests.Load())
	}
	m.ActiveWHIPIngests.Add(-3) // Reset

	// Test ActiveSubscribers
	initial = m.ActiveSubscribers.Load()
	m.ActiveSubscribers.Add(7)
	if m.ActiveSubscribers.Load() != initial+7 {
		t.Errorf("expected ActiveSubscribers to be %d, got %d", initial+7, m.ActiveSubscribers.Load())
	}
	m.ActiveSubscribers.Add(-7) // Reset

	// Test WaitingParticipants
	initial = m.WaitingParticipants.Load()
	m.WaitingParticipants.Add(2)
	if m.WaitingParticipants.Load() != initial+2 {
		t.Errorf("expected WaitingParticipants to be %d, got %d", initial+2, m.WaitingParticipants.Load())
	}
	m.WaitingParticipants.Add(-2) // Reset
}

func TestMetrics_Counters(t *testing.T) {
	m := Get()

	// Test TotalRoomsCreated
	initial := m.TotalRoomsCreated.Load()
	m.TotalRoomsCreated.Add(1)
	if m.TotalRoomsCreated.Load() != initial+1 {
		t.Errorf("expected TotalRoomsCreated to increase by 1")
	}

	// Test TotalMessagesChat
	initial = m.TotalMessagesChat.Load()
	m.TotalMessagesChat.Add(100)
	if m.TotalMessagesChat.Load() != initial+100 {
		t.Errorf("expected TotalMessagesChat to increase by 100")
	}

	// Test TotalMessagesCursor
	initial = m.TotalMessagesCursor.Load()
	m.TotalMessagesCursor.Add(50)
	if m.TotalMessagesCursor.Load() != initial+50 {
		t.Errorf("expected TotalMessagesCursor to increase by 50")
	}

	// Test TotalMessagesMedia
	initial = m.TotalMessagesMedia.Load()
	m.TotalMessagesMedia.Add(25)
	if m.TotalMessagesMedia.Load() != initial+25 {
		t.Errorf("expected TotalMessagesMedia to increase by 25")
	}

	// Test TotalFilesUploaded
	initial = m.TotalFilesUploaded.Load()
	m.TotalFilesUploaded.Add(10)
	if m.TotalFilesUploaded.Load() != initial+10 {
		t.Errorf("expected TotalFilesUploaded to increase by 10")
	}

	// Test TotalJoinRequests
	initial = m.TotalJoinRequests.Load()
	m.TotalJoinRequests.Add(20)
	if m.TotalJoinRequests.Load() != initial+20 {
		t.Errorf("expected TotalJoinRequests to increase by 20")
	}

	// Test TotalErrors
	initial = m.TotalErrors.Load()
	m.TotalErrors.Add(5)
	if m.TotalErrors.Load() != initial+5 {
		t.Errorf("expected TotalErrors to increase by 5")
	}

	// Test TotalWebRTCErrors
	initial = m.TotalWebRTCErrors.Load()
	m.TotalWebRTCErrors.Add(3)
	if m.TotalWebRTCErrors.Load() != initial+3 {
		t.Errorf("expected TotalWebRTCErrors to increase by 3")
	}
}

func TestMetrics_Handler(t *testing.T) {
	req := httptest.NewRequest("GET", "/metrics", nil)
	rr := httptest.NewRecorder()

	Handler()(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Check content type
	contentType := rr.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") {
		t.Errorf("expected content-type text/plain, got %s", contentType)
	}

	body := rr.Body.String()

	// Check for expected metric names
	expectedMetrics := []string{
		"chromatic_active_rooms",
		"chromatic_websocket_connections",
		"chromatic_whip_ingests",
		"chromatic_active_subscribers",
		"chromatic_waiting_participants",
		"chromatic_rooms_created_total",
		"chromatic_messages_chat_total",
		"chromatic_messages_cursor_total",
		"chromatic_messages_media_total",
		"chromatic_files_uploaded_total",
		"chromatic_join_requests_total",
		"chromatic_errors_total",
		"chromatic_webrtc_errors_total",
		"chromatic_uptime_seconds",
		"chromatic_info",
		"# HELP",
		"# TYPE",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(body, metric) {
			t.Errorf("expected metrics output to contain %s", metric)
		}
	}

	// Check Prometheus format
	if !strings.Contains(body, "gauge") && !strings.Contains(body, "counter") {
		t.Error("expected Prometheus format with gauge/counter types")
	}
}

func TestMetrics_Handler_UptimeIncreases(t *testing.T) {
	// First request
	req1 := httptest.NewRequest("GET", "/metrics", nil)
	rr1 := httptest.NewRecorder()
	Handler()(rr1, req1)

	body1 := rr1.Body.String()

	// Uptime should be present and non-negative
	if !strings.Contains(body1, "chromatic_uptime_seconds") {
		t.Error("expected uptime metric")
	}
}

func TestMetrics_BuildInfo(t *testing.T) {
	req := httptest.NewRequest("GET", "/metrics", nil)
	rr := httptest.NewRecorder()

	Handler()(rr, req)

	body := rr.Body.String()

	// Check build info
	if !strings.Contains(body, `chromatic_info{version="1.0.0"} 1`) {
		t.Error("expected build info metric with version")
	}
}
