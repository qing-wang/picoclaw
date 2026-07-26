package mgmt

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sipeed/picoclaw/pkg/config"
)

// handleConfig dispatches GET /PUT /PATCH /mgmt/v1/config.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetConfig(w, r)
	case http.MethodPut:
		s.handlePutConfig(w, r)
	case http.MethodPatch:
		s.handlePatchConfig(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleGetConfig returns the full config with secrets masked as "********".
func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.loadConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
		return
	}

	masked, err := maskConfigForOutput(cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, masked)
}

// handlePutConfig fully replaces the config.
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read error"})
		return
	}

	// Replace sentinel "********" → "[NOT_HERE]" so SecureString.UnmarshalJSON no-ops
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	replaceSentinelInMap(raw)
	normalised, err := json.Marshal(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	var cfg config.Config
	if err := json.Unmarshal(normalised, &cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid config: %v", err)})
		return
	}

	// Restore secrets from disk for any field that was sentinelled
	if err := cfg.SecurityCopyFrom(s.opts.ConfigPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "secret restore failed"})
		return
	}

	if errs := validateMgmtConfig(&cfg); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": errs})
		return
	}

	if err := config.SaveConfig(s.opts.ConfigPath, &cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed"})
		return
	}
	s.invalidateConfigCache()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePatchConfig applies an RFC 7396 merge-patch to the existing config.
func (s *Server) handlePatchConfig(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	patchBody, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read error"})
		return
	}

	var patch map[string]any
	if err := json.Unmarshal(patchBody, &patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	// Load base config
	cfg, err := s.loadConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
		return
	}

	// Marshal base to map, apply merge patch, unmarshal result
	baseJSON, err := json.Marshal(cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	var base map[string]any
	if err := json.Unmarshal(baseJSON, &base); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	mergePatch(base, patch)

	// Replace sentinel before unmarshal
	replaceSentinelInMap(base)
	merged, err := json.Marshal(base)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	var newCfg config.Config
	if err := json.Unmarshal(merged, &newCfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid config: %v", err)})
		return
	}

	// Restore secrets from disk
	if err := newCfg.SecurityCopyFrom(s.opts.ConfigPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "secret restore failed"})
		return
	}

	if errs := validateMgmtConfig(&newCfg); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": errs})
		return
	}

	if err := config.SaveConfig(s.opts.ConfigPath, &newCfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed"})
		return
	}
	s.invalidateConfigCache()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- helpers ----------------------------------------------------------------

const secretMask = "********"
const notHereSentinel = "[NOT_HERE]"

// maskConfigForOutput marshals cfg and replaces all "[NOT_HERE]" values with "********".
func maskConfigForOutput(cfg *config.Config) (map[string]any, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	replaceNotHereInMap(m)
	return m, nil
}

// replaceNotHereInMap replaces all "[NOT_HERE]" string values with "********".
func replaceNotHereInMap(v any) {
	switch vt := v.(type) {
	case map[string]any:
		for k, val := range vt {
			if s, ok := val.(string); ok && s == notHereSentinel {
				vt[k] = secretMask
			} else {
				replaceNotHereInMap(val)
			}
		}
	case []any:
		for _, item := range vt {
			replaceNotHereInMap(item)
		}
	}
}

// replaceSentinelInMap replaces "********" string values with "[NOT_HERE]" so
// SecureString.UnmarshalJSON treats them as no-ops (preserves existing value).
func replaceSentinelInMap(v any) {
	switch vt := v.(type) {
	case map[string]any:
		for k, val := range vt {
			if s, ok := val.(string); ok && s == secretMask {
				vt[k] = notHereSentinel
			} else {
				replaceSentinelInMap(val)
			}
		}
	case []any:
		for _, item := range vt {
			replaceSentinelInMap(item)
		}
	}
}

// mergePatch applies a JSON Merge Patch (RFC 7396) to base in place.
func mergePatch(base, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(base, k)
			continue
		}
		if patchMap, ok := v.(map[string]any); ok {
			if baseMap, ok := base[k].(map[string]any); ok {
				mergePatch(baseMap, patchMap)
				continue
			}
		}
		base[k] = v
	}
}

// validateMgmtConfig runs the subset of config validation that the mgmt API
// needs to enforce on writes.
func validateMgmtConfig(cfg *config.Config) []string {
	var errs []string
	if err := cfg.ValidateModelList(); err != nil {
		errs = append(errs, err.Error())
	}
	if cfg.Gateway.Port != 0 && (cfg.Gateway.Port < 1 || cfg.Gateway.Port > 65535) {
		errs = append(errs, fmt.Sprintf("gateway.port %d is out of valid range (1-65535)", cfg.Gateway.Port))
	}
	return errs
}

// jsonDecoder returns a strict JSON decoder for an HTTP request body.
func jsonDecoder(r *http.Request) *json.Decoder {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20))
}
