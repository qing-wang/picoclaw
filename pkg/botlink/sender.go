package botlink

import (
	"context"
	"errors"
	"time"

	alohaclawtools "github.com/sipeed/picoclaw/pkg/tools/alohaclaw"
)

// ErrBotNotConnected is returned immediately (i.e. without waiting for any
// timeout) by SendCommand/SendMessage when the target bot has no active
// BotLink WebSocket connection. Callers must use errors.Is to check for this,
// not string matching, so they can distinguish "target offline" from
// "target connected but timed out" (see
// doc/task-m3a-botlink-go-instructions.md §5).
var ErrBotNotConnected = errors.New("botlink: bot not connected")

// HubSender implements alohaclawtools.Sender on top of a Hub of live bot
// WebSocket connections. It is the BotLink counterpart of the MQTT
// alohaClawClient, sharing the same interface so fanreflex and
// AlohaClawTool-style callers can use either transport interchangeably.
type HubSender struct {
	hub *Hub
}

var _ alohaclawtools.Sender = (*HubSender)(nil)

// NewSender wraps hub in a Sender. Server.Sender() is the usual way to obtain
// one wired to a running server's hub.
func NewSender(hub *Hub) *HubSender {
	return &HubSender{hub: hub}
}

// SendCommand looks up targetID's live connection and sends text as a
// "command" message, waiting up to waitReply for the matching reply. If
// targetID has no active connection, it returns ErrBotNotConnected
// immediately rather than waiting out waitReply.
func (s *HubSender) SendCommand(ctx context.Context, targetID, text string, waitReply time.Duration) (string, error) {
	conn, ok := s.hub.get(targetID)
	if !ok {
		return "", ErrBotNotConnected
	}
	return conn.sendCommand(ctx, text, waitReply)
}

// SendMessage sends a one-way "message" to targetID, or returns
// ErrBotNotConnected immediately if it is not connected.
func (s *HubSender) SendMessage(targetID, text string) error {
	conn, ok := s.hub.get(targetID)
	if !ok {
		return ErrBotNotConnected
	}
	return conn.sendMessage(text)
}

// IsConnected reports whether at least one bot is currently connected.
func (s *HubSender) IsConnected() bool {
	return s.hub.isAnyConnected()
}

// IsTargetConnected reports whether targetID specifically has a live
// connection right now. This is what lets fanreflex tell "bot offline"
// (enter standby, no fail-safe) apart from "bot connected but not answering"
// (fail-safe, unchanged behaviour).
func (s *HubSender) IsTargetConnected(targetID string) bool {
	_, ok := s.hub.get(targetID)
	return ok
}
