// Package mgmt provides the device management API for PicoClaw.
// Endpoints are mounted on the shared gateway mux under /mgmt/v1.
package mgmt

import (
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// handlerMux is the minimal interface needed to register HTTP handlers.
// Both *http.ServeMux and channels.dynamicServeMux satisfy this interface.
type handlerMux interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// StatusProvider is a function that returns a fanreflex status snapshot.
// It is called from the /mgmt/v1/status endpoint.
type StatusProvider func() map[string]any

// Options holds constructor parameters for New.
type Options struct {
	Cfg           config.MgmtConfig
	ConfigPath    string
	HomePath      string
	WorkspacePath string
	Version       string
}

// Server is the management API server.
type Server struct {
	opts      Options
	startedAt time.Time

	mu             sync.RWMutex
	reloadFunc     func() error
	statusProvider StatusProvider
	channelsFn     func() []string

	pairRL  *pairRateLimiter
	authTrk *authTracker

	// config cache – avoids a disk read on every authenticated request
	cfgCacheMu sync.Mutex
	cfgCache   *config.Config
	cfgCacheAt time.Time

	// pairMu serialises pair operations to prevent TOCTOU on paired_clients
	pairMu sync.Mutex
}

const cfgCacheTTL = 5 * time.Second

// New creates a new management API Server.
func New(opts Options) *Server {
	return &Server{
		opts:      opts,
		startedAt: time.Now(),
		pairRL:    newPairRateLimiter(),
		authTrk:   newAuthTracker(),
	}
}

// SetReloadFunc sets the function used by POST /mgmt/v1/reload.
func (s *Server) SetReloadFunc(fn func() error) {
	s.mu.Lock()
	s.reloadFunc = fn
	s.mu.Unlock()
}

// SetStatusProvider sets the fanreflex snapshot provider for /mgmt/v1/status.
func (s *Server) SetStatusProvider(fn StatusProvider) {
	s.mu.Lock()
	s.statusProvider = fn
	s.mu.Unlock()
}

// SetEnabledChannelsFn sets the function that returns active channel names.
func (s *Server) SetEnabledChannelsFn(fn func() []string) {
	s.mu.Lock()
	s.channelsFn = fn
	s.mu.Unlock()
}

// RegisterOnMux mounts all /mgmt/v1 endpoints on mux.
func (s *Server) RegisterOnMux(mux handlerMux) {
	mux.HandleFunc("/mgmt/v1/info",   s.handleInfo)
	mux.HandleFunc("/mgmt/v1/pair",   s.handlePair)
	mux.HandleFunc("/mgmt/v1/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("/mgmt/v1/reload", s.withAuth(s.handleReload))
	mux.HandleFunc("/mgmt/v1/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/mgmt/v1/files/", s.withAuth(s.handleFiles))
}

// loadConfig returns a cached (up to cfgCacheTTL) or fresh config from disk.
func (s *Server) loadConfig() (*config.Config, error) {
	s.cfgCacheMu.Lock()
	defer s.cfgCacheMu.Unlock()
	if s.cfgCache != nil && time.Since(s.cfgCacheAt) < cfgCacheTTL {
		return s.cfgCache, nil
	}
	cfg, err := config.LoadConfig(s.opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	s.cfgCache = cfg
	s.cfgCacheAt = time.Now()
	return cfg, nil
}

// invalidateConfigCache forces the next loadConfig to read from disk.
func (s *Server) invalidateConfigCache() {
	s.cfgCacheMu.Lock()
	s.cfgCache = nil
	s.cfgCacheMu.Unlock()
}

// ---- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.WarnCF("mgmt", "json encode error", map[string]any{"error": err.Error()})
	}
}

func extractBearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) < len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}

// extractIP returns the host part of a "host:port" remote address.
func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
