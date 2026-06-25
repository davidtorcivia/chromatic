package websocket

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// OnDisconnect is a callback type for client disconnect events
type OnDisconnect func(client *Client)

// ReadPump pumps messages from the WebSocket connection to the hub
func (c *Client) ReadPump(handler func(*Client, Message)) {
	c.ReadPumpWithDisconnect(handler, nil)
}

// ReadPumpWithDisconnect pumps messages and calls onDisconnect when client leaves
func (c *Client) ReadPumpWithDisconnect(handler func(*Client, Message), onDisconnect OnDisconnect) {
	defer func() {
		// Call disconnect handler before unregistering
		if onDisconnect != nil {
			onDisconnect(c)
		}
		c.Hub.Unregister(c)
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxSignalingMessageSize)
	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Invalid message format from %s: %v", c.ID, err)
			continue
		}

		handler(c, msg)
	}
}

// WritePump pumps messages from the hub to the WebSocket connection
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		if c.isDone() {
			// Client is shutting down; do not drain stale queued messages before
			// letting a replacement connection take over.
			c.writeCloseFrame()
			return
		}

		select {
		case <-c.Done:
			c.writeCloseFrame()
			return

		case message := <-c.Send:
			if c.isDone() {
				c.writeCloseFrame()
				return
			}
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			if c.isDone() {
				c.writeCloseFrame()
				return
			}
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) isDone() bool {
	select {
	case <-c.Done:
		return true
	default:
		return false
	}
}

func (c *Client) writeCloseFrame() {
	// Send channel is intentionally never closed to avoid send-on-closed-channel
	// panics in concurrent producers.
	c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
}

// SendJSON sends a JSON message to this client
func (c *Client) SendJSON(msgType string, payload interface{}) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	msg := Message{
		Type:    msgType,
		Payload: payloadBytes,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// Single select avoids the TOCTOU race of check-then-send.
	// Send channel is never closed, so this cannot panic.
	select {
	case <-c.Done:
		// Client disconnected, don't send
	case c.Send <- msgBytes:
	default:
		// Direct JSON sends are used for critical state/signaling. Dropping
		// them silently can leave the browser stuck with stale WebRTC state,
		// so close the connection and let reconnect rebuild a consistent path.
		log.Printf("Send buffer full for client %s; closing connection", c.ID)
		c.closeOnce.Do(func() {
			close(c.Done)
		})
		if c.Conn != nil {
			c.Conn.Close()
		}
	}

	return nil
}
