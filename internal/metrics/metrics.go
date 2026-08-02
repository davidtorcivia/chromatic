// Package metrics exposes the Prometheus counters and gauges Chromatic
// reports (rooms, participants, ingest state, forwarder drops).
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

	// Renegotiation health. These exist because the 2026-08-02 outage was
	// invisible to monitoring: a room-wide reconnect loop ran for 27 minutes
	// while every gauge above looked normal (participants were connected —
	// repeatedly). Each of these counts an event that is survivable once and
	// pathological in a run, so the signal is the RATE, not the total.
	//
	// TotalRenegotiationsWedged: a subscriber could not apply a server
	// renegotiation offer. Recoverable now, but a sustained rate means
	// something is systematically rejecting our SDP.
	TotalRenegotiationsWedged atomic.Int64
	// TotalProgramAudioMonoFallbacks: the browser refused the stereo-tuned
	// answer and we answered untuned to keep the connection. Program audio is
	// mono until the next renegotiation, so any nonzero value is worth knowing.
	TotalProgramAudioMonoFallbacks atomic.Int64
	// TotalSubscriberResubscribes: a client asked for a brand-new subscriber.
	// A few are normal over a long session; a steady stream is the loop.
	TotalSubscriberResubscribes atomic.Int64
	// TotalStaleAnswersIgnored: an answer arrived for a negotiation that had
	// already settled. Benign individually; a spike means offer/answer races.
	TotalStaleAnswersIgnored atomic.Int64

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

		// Renegotiation health — alert on rate of change, not absolute value.
		writeCounter("chromatic_renegotiations_wedged_total",
			"Subscriber renegotiation offers the client could not apply",
			m.TotalRenegotiationsWedged.Load())
		writeCounter("chromatic_program_audio_mono_fallbacks_total",
			"Times a browser refused the stereo-tuned answer and program audio fell back to mono",
			m.TotalProgramAudioMonoFallbacks.Load())
		writeCounter("chromatic_subscriber_resubscribes_total",
			"Client-requested rebuilds of a subscriber connection",
			m.TotalSubscriberResubscribes.Load())
		writeCounter("chromatic_stale_answers_ignored_total",
			"Answers discarded because their negotiation had already settled",
			m.TotalStaleAnswersIgnored.Load())

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
