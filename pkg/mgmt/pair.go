package mgmt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// getOrCreateDeviceID returns the persistent device UUID, creating it on first call.
// The ID is stored at <homePath>/device-id with mode 0600.
func getOrCreateDeviceID(homePath string) string {
	path := filepath.Join(homePath, "device-id")
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	id := newUUID()
	if err := fileutil.WriteFileAtomic(path, []byte(id+"\n"), 0o600); err != nil {
		logger.WarnCF("mgmt", "cannot persist device-id", map[string]any{"error": err.Error()})
	}
	return id
}

// newUUID generates a random UUID v4.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("mgmt: rand.Read: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// generateToken creates a 64-character hex token and its SHA-256 hex digest.
func generateToken() (token, sha256hex string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return
	}
	token = hex.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(token))
	sha256hex = hex.EncodeToString(sum[:])
	return
}

// isPairAllowed returns true if remoteAddr is permitted to call /pair.
// It checks pair_interfaces subnet membership and pair_subnets CIDR list.
func isPairAllowed(remoteAddr string, cfg config.MgmtConfig) bool {
	rawIP := extractIP(remoteAddr)
	ip := net.ParseIP(rawIP)
	if ip == nil {
		return false
	}

	// Check pair_interfaces
	for _, name := range cfg.PairInterfaces {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			switch a := addr.(type) {
			case *net.IPNet:
				if a.Contains(ip) {
					return true
				}
			case *net.IPAddr:
				if a.IP.Equal(ip) {
					return true
				}
			}
		}
	}

	// Check pair_subnets (CIDR)
	if len(cfg.PairSubnets) > 0 {
		addr, ok := netip.AddrFromSlice(ip.To16())
		if ok {
			addr = addr.Unmap()
			for _, cidr := range cfg.PairSubnets {
				prefix, err := netip.ParsePrefix(cidr)
				if err != nil {
					continue
				}
				if prefix.Contains(addr) {
					return true
				}
			}
		}
	}

	return false
}

// handlePair handles POST /mgmt/v1/pair.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	ip := extractIP(r.RemoteAddr)

	// Rate limit
	if !s.pairRL.allow(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}

	// Source check – 403 without explanation on failure (per spec)
	if !isPairAllowed(r.RemoteAddr, s.opts.Cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	// Parse body
	var body struct {
		ClientName string `json:"client_name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	clientName := strings.TrimSpace(body.ClientName)
	if clientName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client_name required"})
		return
	}

	// Generate token
	token, tokenHash, err := generateToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Serialise config update
	s.pairMu.Lock()
	defer s.pairMu.Unlock()

	cfg, err := config.LoadConfig(s.opts.ConfigPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
		return
	}

	// Upsert paired_clients entry (same client_name = rotate token)
	newClient := config.PairedClient{
		Name:        clientName,
		TokenSHA256: tokenHash,
		Created:     time.Now().UTC().Format(time.RFC3339),
	}
	replaced := false
	for i, c := range cfg.Mgmt.PairedClients {
		if c.Name == clientName {
			cfg.Mgmt.PairedClients[i] = newClient
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Mgmt.PairedClients = append(cfg.Mgmt.PairedClients, newClient)
	}

	if err := config.SaveConfig(s.opts.ConfigPath, cfg); err != nil {
		logger.ErrorCF("mgmt", "failed to save paired client", map[string]any{"error": err.Error()})
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.invalidateConfigCache()

	deviceName := cfg.Mgmt.DeviceName
	if deviceName == "" {
		if host, err := os.Hostname(); err == nil {
			deviceName = host
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"device_id":   getOrCreateDeviceID(s.opts.HomePath),
		"token":       token,
		"device_name": deviceName,
	})
}

// decodeJSON reads and decodes a JSON request body.
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return jsonDecoder(r).Decode(v)
}
