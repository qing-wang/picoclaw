package botlink

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// wireMessage is the BotLink JSON wire message.
//
// This is a cross-language contract shared with the C# BotLink client (see
// doc/task-m3a-botlink-go-instructions.md §3) — field names, JSON shape, and
// per-direction semantics must not change without updating both sides.
//
//	PicoClaw -> bot: type "command" (text + request_id), "message" (text,
//	                 one-way), "ping"
//	bot -> PicoClaw: type "reply" (request_id + payload = {success,result,data}),
//	                 "pong"
type wireMessage struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Text      string          `json:"text,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

const (
	msgTypeCommand = "command"
	msgTypeMessage = "message"
	msgTypePing    = "ping"
	msgTypeReply   = "reply"
	msgTypePong    = "pong"
)

// botConn is one active WebSocket connection to a single bot. It owns the
// pendingReqs <-> reply-channel bookkeeping (mirroring the MQTT transport's
// alohaClawClient) and serializes writes, since gorilla/websocket forbids
// concurrent writers on one connection.
type botConn struct {
	botID string
	ws    *websocket.Conn

	writeMu sync.Mutex
	closed  atomic.Bool
	done    chan struct{} // closed exactly once, when the connection is torn down

	pendingMu sync.Mutex
	pending   map[string]chan string // request_id -> reply payload (JSON text)
}

func newBotConn(botID string, ws *websocket.Conn) *botConn {
	return &botConn{
		botID:   botID,
		ws:      ws,
		done:    make(chan struct{}),
		pending: make(map[string]chan string),
	}
}

// writeJSON serializes and sends v, holding writeMu for the duration since
// gorilla/websocket does not allow concurrent writes on one *Conn.
func (c *botConn) writeJSON(v any) error {
	if c.closed.Load() {
		return fmt.Errorf("botlink: connection to %q is closed", c.botID)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteJSON(v)
}

// close tears the connection down exactly once: it unblocks any in-flight
// sendCommand waiters (via done) and closes the underlying socket.
func (c *botConn) close() {
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
		_ = c.ws.Close()
	}
}

// sendCommand sends a "command" message and, when waitReply > 0, waits for
// the matching "reply" (correlated by request_id, same pattern as the MQTT
// transport's pendingReqs map). The returned string is the reply payload
// re-serialized as JSON text — this intentionally matches the MQTT
// transport's SendCommand return format so fanreflex's shared botReply JSON
// parsing works unchanged regardless of transport.
func (c *botConn) sendCommand(ctx context.Context, text string, waitReply time.Duration) (string, error) {
	reqID := uuid.New().String()

	var replyCh chan string
	if waitReply > 0 {
		replyCh = make(chan string, 1)
		c.pendingMu.Lock()
		c.pending[reqID] = replyCh
		c.pendingMu.Unlock()
		defer func() {
			c.pendingMu.Lock()
			delete(c.pending, reqID)
			c.pendingMu.Unlock()
		}()
	}

	msg := wireMessage{Type: msgTypeCommand, RequestID: reqID, Text: text}
	if err := c.writeJSON(msg); err != nil {
		return "", fmt.Errorf("botlink: send command to %q: %w", c.botID, err)
	}

	if waitReply <= 0 {
		return "", nil
	}

	timer := time.NewTimer(waitReply)
	defer timer.Stop()

	select {
	case reply := <-replyCh:
		return reply, nil
	case <-timer.C:
		return "", fmt.Errorf("botlink: no reply from %q after %v", c.botID, waitReply)
	case <-c.done:
		return "", fmt.Errorf("botlink: connection to %q closed while waiting for reply", c.botID)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// sendMessage sends a one-way "message" (no reply expected).
func (c *botConn) sendMessage(text string) error {
	msg := wireMessage{Type: msgTypeMessage, RequestID: uuid.New().String(), Text: text}
	if err := c.writeJSON(msg); err != nil {
		return fmt.Errorf("botlink: send message to %q: %w", c.botID, err)
	}
	return nil
}

// handleInbound dispatches one message received from the bot.
func (c *botConn) handleInbound(msg wireMessage) {
	switch msg.Type {
	case msgTypeReply:
		if msg.RequestID == "" {
			return
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[msg.RequestID]
		c.pendingMu.Unlock()
		if !ok {
			return
		}
		text := string(msg.Payload)
		select {
		case ch <- text:
		default:
		}
	case msgTypePong:
		// Application-level pong; no action needed. Keepalive relies on
		// WebSocket control-frame pings (handled by the server's read loop /
		// pong handler), not this application-level message.
	}
}
