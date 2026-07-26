package botlink

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	// WSPath is the BotLink WebSocket endpoint, mounted on the shared gateway
	// mux (see doc/task-m3a-botlink-go-instructions.md §3).
	WSPath = "/botlink/ws"

	defaultPingInterval = 30 * time.Second // server -> bot WebSocket ping frame cadence
	defaultReadTimeout  = 60 * time.Second // no read (incl. pong) within this window => dead connection
)

// handlerMux is the minimal interface needed to register HTTP handlers.
// Both *http.ServeMux and the gateway's dynamicServeMux satisfy this
// interface (mirrors pkg/mgmt's identically-named, identically-shaped type).
type handlerMux interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Server is the BotLink WebSocket transport: it hosts the /botlink/ws
// endpoint that bots (e.g. CTFanBot) connect to, authenticates them against
// the configured bot list, and maintains a Hub of live connections. Sender()
// exposes that hub through the alohaclaw.Sender interface for fanreflex (or
// any other caller) to send commands over.
//
// Server is designed to survive a gateway config reload without being
// recreated: RegisterOnMux mounts /botlink/ws on the shared gateway
// http.ServeMux exactly once, at process startup — a ServeMux panics if the
// same pattern is registered twice, so reload cannot rebuild-and-re-register
// a new Server. Instead, reload calls UpdateConfig to swap in the new bot
// authorization table and enabled flag on this same, already-registered
// instance (see doc/task-m3d-reload-lifecycle-instructions.md §2).
type Server struct {
	mu      sync.RWMutex
	hub     *Hub
	bots    map[string]config.BotLinkBot // bot_id -> config; replaced wholesale by UpdateConfig
	enabled bool                         // checked by handleWebSocket; toggled by UpdateConfig

	upgrader     websocket.Upgrader
	pingInterval time.Duration
	readTimeout  time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a BotLink server for the given set of paired bots. It does not
// start listening until RegisterOnMux is called with the shared gateway mux.
// cfg.Enabled only gates whether handleWebSocket accepts connections (503
// when false) — RegisterOnMux must still be called unconditionally so that a
// later reload can flip Enabled on via UpdateConfig without needing to
// register the handler for the first time (which would panic if some other
// path already registered it, or simply be too late if nothing did).
func New(cfg config.BotLinkConfig) *Server {
	bots := make(map[string]config.BotLinkBot, len(cfg.Bots))
	for _, b := range cfg.Bots {
		bots[b.BotID] = b
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		hub:     newHub(),
		bots:    bots,
		enabled: cfg.Enabled,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// BotLink is authenticated by bearer token + bot ID, not browser
			// same-origin policy; bots are non-browser LAN clients.
			CheckOrigin: func(*http.Request) bool { return true },
		},
		pingInterval: defaultPingInterval,
		readTimeout:  defaultReadTimeout,
		ctx:          ctx,
		cancel:       cancel,
	}
}

// RegisterOnMux mounts /botlink/ws on mux (mirrors pkg/mgmt and
// pkg/channels/pico's RegisterOnMux / mux-registration pattern).
//
// Call this exactly once per process lifetime, regardless of whether BotLink
// is currently enabled — http.ServeMux panics on duplicate registration of
// the same pattern, so calling this again on config reload (e.g. from a
// rebuilt Server) would crash the gateway. Enable/disable after startup is a
// runtime state check inside handleWebSocket (503 when disabled), handled by
// UpdateConfig, not a registration decision.
func (s *Server) RegisterOnMux(mux handlerMux) {
	mux.HandleFunc(WSPath, s.handleWebSocket)
}

// UpdateConfig replaces this server's bot authorization table and enabled
// flag with cfg's, without touching the mux registration (see RegisterOnMux).
// Any bot present in the old table but absent from cfg.Bots has its live
// connection closed immediately — its authorization was just revoked, so it
// must not remain connected. Bots that remain in cfg.Bots keep their
// existing connection untouched (reload must not disturb bots that are still
// authorized, even if unrelated config changed).
func (s *Server) UpdateConfig(cfg config.BotLinkConfig) {
	newBots := make(map[string]config.BotLinkBot, len(cfg.Bots))
	for _, b := range cfg.Bots {
		newBots[b.BotID] = b
	}

	s.mu.Lock()
	oldBots := s.bots
	s.bots = newBots
	s.enabled = cfg.Enabled
	s.mu.Unlock()

	for botID := range oldBots {
		if _, stillPaired := newBots[botID]; !stillPaired {
			s.hub.closeBot(botID)
		}
	}
}

// Sender returns a Sender implementation backed by this server's hub.
func (s *Server) Sender() *HubSender {
	return NewSender(s.hub)
}

// Stop closes all active connections and waits for their goroutines to exit.
func (s *Server) Stop() {
	s.cancel()
	s.hub.closeAll()
	s.wg.Wait()
}

// authenticate validates the request's Authorization/X-Bot-Id headers per
// §3: SHA-256(token) compared in constant time against the configured bot's
// token_sha256 (mirrors pkg/mgmt/auth.go's withAuth).
func (s *Server) authenticate(r *http.Request) (config.BotLinkBot, bool) {
	botID := r.Header.Get("X-Bot-Id")
	token := extractBearerToken(r.Header.Get("Authorization"))
	if botID == "" || token == "" {
		return config.BotLinkBot{}, false
	}

	s.mu.RLock()
	bot, ok := s.bots[botID]
	s.mu.RUnlock()
	if !ok || bot.TokenSHA256 == "" {
		return config.BotLinkBot{}, false
	}

	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(tokenHash), []byte(bot.TokenSHA256)) != 1 {
		return config.BotLinkBot{}, false
	}

	return bot, true
}

func extractBearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) < len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}

// handleWebSocket authenticates and upgrades one BotLink connection. On
// success, an existing connection for the same bot_id is closed and replaced
// (§3: "同一 bot_id 重複連線 → 新連線取代舊連線").
//
// This handler is registered on the mux unconditionally (see RegisterOnMux)
// so that enabling BotLink via a config reload never needs a second
// registration; when currently disabled it rejects with 503 instead of
// upgrading.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	enabled := s.enabled
	s.mu.RUnlock()
	if !enabled {
		http.Error(w, "botlink disabled", http.StatusServiceUnavailable)
		return
	}

	bot, ok := s.authenticate(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.ErrorCF("botlink", "WebSocket upgrade failed", map[string]any{
			"error": err.Error(), "bot_id": bot.BotID,
		})
		return
	}

	bc := newBotConn(bot.BotID, conn)
	if old := s.hub.replace(bot.BotID, bc); old != nil {
		logger.InfoCF("botlink", "New connection replacing existing one for bot", map[string]any{
			"bot_id": bot.BotID,
		})
		old.close()
	}

	logger.InfoCF("botlink", "Bot connected", map[string]any{"bot_id": bot.BotID})

	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.readLoop(bc)
	}()
	go func() {
		defer s.wg.Done()
		s.pingLoop(bc)
	}()
}

// readLoop reads inbound frames for one connection until it errors/closes,
// then tears the connection down and evicts it from the hub (if it is still
// the current one — see Hub.removeIfCurrent).
func (s *Server) readLoop(bc *botConn) {
	defer func() {
		bc.close()
		s.hub.removeIfCurrent(bc.botID, bc)
		logger.InfoCF("botlink", "Bot disconnected", map[string]any{"bot_id": bc.botID})
	}()

	_ = bc.ws.SetReadDeadline(time.Now().Add(s.readTimeout))
	bc.ws.SetPongHandler(func(string) error {
		_ = bc.ws.SetReadDeadline(time.Now().Add(s.readTimeout))
		return nil
	})

	for {
		_, raw, err := bc.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = bc.ws.SetReadDeadline(time.Now().Add(s.readTimeout))

		var msg wireMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		bc.handleInbound(msg)
	}
}

// pingLoop sends periodic WebSocket ping control frames (not the application
// -level {"type":"ping"} message — §3 reserves that for future use) to keep
// the connection alive and let SetReadDeadline/pong-handler detect death.
func (s *Server) pingLoop(bc *botConn) {
	ticker := time.NewTicker(s.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-bc.done:
			return
		case <-ticker.C:
			bc.writeMu.Lock()
			err := bc.ws.WriteMessage(websocket.PingMessage, nil)
			bc.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
