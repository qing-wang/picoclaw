// Package botlink implements the BotLink LAN transport: a same-subnet
// WebSocket alternative to the AlohaClaw MQTT transport for talking to bots
// such as CTFanBot. See doc/task-m3a-botlink-go-instructions.md §3 for the
// wire protocol, which is a cross-language contract shared with a C# client
// and must not be changed unilaterally.
package botlink

import "sync"

// Hub tracks the single active *botConn per bot_id. It is the shared state
// between the WebSocket server (which populates it as bots connect) and
// HubSender (which looks targets up in it to route commands).
type Hub struct {
	mu    sync.RWMutex
	conns map[string]*botConn // bot_id -> conn
}

func newHub() *Hub {
	return &Hub{conns: make(map[string]*botConn)}
}

// get returns the current connection for botID, if any.
func (h *Hub) get(botID string) (*botConn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.conns[botID]
	return c, ok
}

// replace installs newConn as the current connection for botID, returning any
// previously-registered connection so the caller can close it. Per the wire
// protocol (§3): "同一 bot_id 重複連線 → 新連線取代舊連線".
func (h *Hub) replace(botID string, newConn *botConn) *botConn {
	h.mu.Lock()
	old := h.conns[botID]
	h.conns[botID] = newConn
	h.mu.Unlock()
	return old
}

// removeIfCurrent deletes botID's entry only if it still points at conn. This
// guards against a stale connection's cleanup racing with — and wrongly
// evicting — a newer connection that has already replaced it in the hub.
func (h *Hub) removeIfCurrent(botID string, conn *botConn) {
	h.mu.Lock()
	if h.conns[botID] == conn {
		delete(h.conns, botID)
	}
	h.mu.Unlock()
}

// isAnyConnected reports whether at least one bot is currently connected.
func (h *Hub) isAnyConnected() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns) > 0
}

// closeBot closes and evicts botID's current connection, if any. This is
// used by Server.UpdateConfig to drop a bot's live connection the moment its
// authorization is revoked (removed from config on reload) — an unpaired bot
// must not remain connected just because it was already connected before the
// reload.
func (h *Hub) closeBot(botID string) {
	h.mu.Lock()
	c, ok := h.conns[botID]
	if ok {
		delete(h.conns, botID)
	}
	h.mu.Unlock()

	if ok {
		c.close()
	}
}

// closeAll closes every active connection, e.g. on server shutdown.
func (h *Hub) closeAll() {
	h.mu.Lock()
	all := make([]*botConn, 0, len(h.conns))
	for _, c := range h.conns {
		all = append(all, c)
	}
	clear(h.conns)
	h.mu.Unlock()

	for _, c := range all {
		c.close()
	}
}
