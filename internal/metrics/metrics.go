package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics holds all application metrics
type Metrics struct {
	// Gauges (current values)
	ActiveRooms         atomic.Int64
	ActiveWebsockets    atomic.Int64
	ActiveWHIPIngests   atomic.Int64
	ActiveSubscribers   atomic.Int64
	WaitingParticipants atomic.Int64

	// Counters (cumulative)
	TotalRoomsCreated   atomic.Int64
	TotalMessagesChat   atomic.Int64
	TotalMessagesCursor atomic.Int64
	TotalMessagesMedia  atomic.Int64
	TotalFilesUploaded  atomic.Int64
	TotalJoinRequests   atomic.Int64
	TotalErrors         atomic.Int64
	TotalWebRTCErrors   atomic.Int64

	// Uptime tracking
	startTime time.Time
}

// Global metrics instance
var globalMetrics *Metrics
var once sync.Once

// Get returns the global metrics instance
func Get() *Metrics {
	once.Do(func() {
		globalMetrics = &Metrics{
			startTime: time.Now(),
		}
	})
	return globalMetrics
}

// Handler returns an HTTP handler that serves Prometheus-format metrics
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := Get()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		// Helper to write metric
		writeGauge := func(name, help string, value int64) {
			fmt.Fprintf(w, "# HELP %s %s\n", name, help)
			fmt.Fprintf(w, "# TYPE %s gauge\n", name)
			fmt.Fprintf(w, "%s %d\n\n", name, value)
		}

		writeCounter := func(name, help string, value int64) {
			fmt.Fprintf(w, "# HELP %s %s\n", name, help)
			fmt.Fprintf(w, "# TYPE %s counter\n", name)
			fmt.Fprintf(w, "%s %d\n\n", name, value)
		}

		// Gauges
		writeGauge("chromatic_active_rooms", "Number of currently active rooms", m.ActiveRooms.Load())
		writeGauge("chromatic_websocket_connections", "Number of active WebSocket connections", m.ActiveWebsockets.Load())
		writeGauge("chromatic_whip_ingests", "Number of active WHIP ingest connections", m.ActiveWHIPIngests.Load())
		writeGauge("chromatic_active_subscribers", "Number of active WebRTC subscribers", m.ActiveSubscribers.Load())
		writeGauge("chromatic_waiting_participants", "Number of participants in waiting rooms", m.WaitingParticipants.Load())

		// Counters
		writeCounter("chromatic_rooms_created_total", "Total number of rooms created", m.TotalRoomsCreated.Load())
		writeCounter("chromatic_messages_chat_total", "Total number of chat messages sent", m.TotalMessagesChat.Load())
		writeCounter("chromatic_messages_cursor_total", "Total number of cursor updates sent", m.TotalMessagesCursor.Load())
		writeCounter("chromatic_messages_media_total", "Total number of media toggle messages", m.TotalMessagesMedia.Load())
		writeCounter("chromatic_files_uploaded_total", "Total number of files uploaded", m.TotalFilesUploaded.Load())
		writeCounter("chromatic_join_requests_total", "Total number of room join requests", m.TotalJoinRequests.Load())
		writeCounter("chromatic_errors_total", "Total number of errors", m.TotalErrors.Load())
		writeCounter("chromatic_webrtc_errors_total", "Total number of WebRTC errors", m.TotalWebRTCErrors.Load())

		// Uptime
		uptime := time.Since(m.startTime).Seconds()
		fmt.Fprintf(w, "# HELP chromatic_uptime_seconds Time since server started\n")
		fmt.Fprintf(w, "# TYPE chromatic_uptime_seconds gauge\n")
		fmt.Fprintf(w, "chromatic_uptime_seconds %.0f\n\n", uptime)

		// Build info
		fmt.Fprintf(w, "# HELP chromatic_info Build information\n")
		fmt.Fprintf(w, "# TYPE chromatic_info gauge\n")
		fmt.Fprintf(w, "chromatic_info{version=\"1.0.0\"} 1\n")
	}
}
