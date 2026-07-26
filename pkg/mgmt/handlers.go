package mgmt

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

// handleInfo serves GET /mgmt/v1/info — no authentication required.
// Returns device identity metadata. Does NOT expose tokens or paired client details.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	cfg, _ := s.loadConfig() // best-effort; use zero values on error

	deviceName := ""
	paired := false
	if cfg != nil {
		deviceName = cfg.Mgmt.DeviceName
		paired = len(cfg.Mgmt.PairedClients) > 0
	}
	if deviceName == "" {
		if host, err := os.Hostname(); err == nil {
			deviceName = host
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":   getOrCreateDeviceID(s.opts.HomePath),
		"device_name": deviceName,
		"version":     s.opts.Version,
		"api_version": "v1",
		"platform":    fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		"paired":      paired,
	})
}

// handleReload serves POST /mgmt/v1/reload — triggers a config reload.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	s.mu.RLock()
	fn := s.reloadFunc
	s.mu.RUnlock()

	if fn == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reload not available"})
		return
	}

	go func() {
		if err := fn(); err != nil {
			_ = err // error is logged inside the reload func
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "reload triggered"})
}

// handleStatus serves GET /mgmt/v1/status.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	s.mu.RLock()
	channelsFn := s.channelsFn
	statusFn := s.statusProvider
	s.mu.RUnlock()

	var channels []string
	if channelsFn != nil {
		channels = channelsFn()
	}

	var fanreflexStatus map[string]any
	if statusFn != nil {
		fanreflexStatus = statusFn()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds": int(time.Since(s.startedAt).Seconds()),
		"version":        s.opts.Version,
		"channels":       channels,
		"fanreflex":      fanreflexStatus,
		"config_path":    s.opts.ConfigPath,
	})
}

// handleFiles serves GET and PUT under /mgmt/v1/files/.
// Only whitelisted paths are accessible.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	// Strip /mgmt/v1/files/ prefix to get the relative path
	relPath := strings.TrimPrefix(r.URL.Path, "/mgmt/v1/files/")
	if relPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path required"})
		return
	}

	if err := validateFilePath(relPath); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	absPath, err := safeJoinWorkspace(s.opts.WorkspacePath, relPath)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.serveFileDownload(w, r, absPath)
	case http.MethodPut:
		s.serveFileUpload(w, r, absPath)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) serveFileDownload(w http.ResponseWriter, _ *http.Request, absPath string) {
	f, err := os.Open(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read error"})
		}
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (s *Server) serveFileUpload(w http.ResponseWriter, r *http.Request, absPath string) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB max
	defer r.Body.Close()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read error"})
		return
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "mkdir failed"})
		return
	}

	if err := atomicWrite(absPath, data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- path security helpers --------------------------------------------------

// allowedPaths lists the whitelisted relative paths and prefixes.
// Prefix entries end with "/".
var allowedPaths = []string{
	"fan-policy.json",
	"fan-mode.txt",
	"skills/", // prefix — any file under skills/ is allowed
}

// validateFilePath checks that relPath matches the whitelist and contains no
// path traversal sequences.
func validateFilePath(relPath string) error {
	// Reject any ".." segment before joining
	cleaned := path.Clean(relPath)
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("path traversal not allowed")
	}

	for _, allowed := range allowedPaths {
		if strings.HasSuffix(allowed, "/") {
			if strings.HasPrefix(cleaned, allowed) || cleaned == strings.TrimSuffix(allowed, "/") {
				return nil
			}
		} else {
			if cleaned == allowed {
				return nil
			}
		}
	}
	return fmt.Errorf("path not in allowed list")
}

// safeJoinWorkspace joins workspace and relPath and verifies the result stays
// under workspace using EvalSymlinks to catch symlink escapes.
func safeJoinWorkspace(workspace, relPath string) (string, error) {
	joined := filepath.Join(workspace, filepath.FromSlash(relPath))

	// For upload paths (file may not exist yet), check the parent directory
	target := joined
	if _, err := os.Lstat(joined); os.IsNotExist(err) {
		target = filepath.Dir(joined)
		if _, err2 := os.Lstat(target); os.IsNotExist(err2) {
			// Parent doesn't exist yet; that's OK — we'll create it. Just check
			// the workspace root is real.
			target = workspace
		}
	}

	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		// If target doesn't exist, just check joined has workspace as prefix
		absJoined, err2 := filepath.Abs(joined)
		if err2 != nil {
			return "", fmt.Errorf("path resolution failed")
		}
		absWS, err3 := filepath.Abs(workspace)
		if err3 != nil {
			return "", fmt.Errorf("path resolution failed")
		}
		if !strings.HasPrefix(absJoined, absWS+string(filepath.Separator)) && absJoined != absWS {
			return "", fmt.Errorf("path escapes workspace")
		}
		return joined, nil
	}

	realWS, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		realWS = workspace
	}

	if !strings.HasPrefix(realTarget, realWS+string(filepath.Separator)) && realTarget != realWS {
		return "", fmt.Errorf("path escapes workspace")
	}
	return joined, nil
}

// atomicWrite is a var so tests can swap it for plain os.WriteFile.
var atomicWrite = func(filePath string, data []byte, perm os.FileMode) error {
	return fileutil.WriteFileAtomic(filePath, data, perm)
}
