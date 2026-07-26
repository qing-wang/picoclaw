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
	w.Header().Set("Cache-Control", "no-store") // MINOR-6: prevent caching of masked config
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

	// Fresh disk read (bypass cache) — authoritative for security-boundary fields
	diskCfg, err := config.LoadConfig(s.opts.ConfigPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
		return
	}

	// Replace sentinel "********" → "[NOT_HERE]" so SecureString.UnmarshalJSON no-ops
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	// Reject any attempt to change security-boundary mgmt fields (CRITICAL-1)
	changed, err := mgmtSecurityFieldsChanged(raw, diskCfg.Mgmt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid mgmt section"})
		return
	}
	if changed {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": mgmtReadOnlyMsg})
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

	// Unconditionally restore security-boundary fields (defense in depth)
	restoreMgmtSecurityFields(&cfg.Mgmt, diskCfg.Mgmt)

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

	// Reject immediately if patch touches any security-boundary mgmt field (CRITICAL-1)
	if mgmtPatchHasProtectedKey(patch) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": mgmtReadOnlyMsg})
		return
	}

	// Fresh disk read (bypass cache) as authoritative base
	diskCfg, err := config.LoadConfig(s.opts.ConfigPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
		return
	}

	// Marshal base to map, apply merge patch, unmarshal result
	baseJSON, err := json.Marshal(diskCfg)
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

	// Unconditionally restore security-boundary fields (defense in depth)
	restoreMgmtSecurityFields(&newCfg.Mgmt, diskCfg.Mgmt)

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

// mgmtReadOnlyMsg is the error returned when a request tries to modify
// security-boundary mgmt fields via the config API.
const mgmtReadOnlyMsg = "mgmt.paired_clients, pair_interfaces, pair_subnets, and enabled are read-only via this API; use the pair endpoint (requires physical USB access)"

// mgmtPatchHasProtectedKey returns true if the PATCH map contains any of the
// security-boundary mgmt fields. Their presence in a PATCH implies intent to change.
func mgmtPatchHasProtectedKey(patch map[string]any) bool {
	mgmtRaw, ok := patch["mgmt"]
	if !ok {
		return false
	}
	mgmt, ok := mgmtRaw.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"paired_clients", "pair_interfaces", "pair_subnets", "enabled"} {
		if _, ok := mgmt[key]; ok {
			return true
		}
	}
	return false
}

// mgmtSecurityFieldsChanged returns true if the submitted PUT body contains any
// security-boundary mgmt field with a value that differs from the current disk value.
func mgmtSecurityFieldsChanged(raw map[string]any, diskMgmt config.MgmtConfig) (bool, error) {
	mgmtRaw, ok := raw["mgmt"]
	if !ok {
		return false, nil
	}
	mgmtMap, ok := mgmtRaw.(map[string]any)
	if !ok {
		return false, nil
	}

	if pc, ok := mgmtMap["paired_clients"]; ok {
		b, err := json.Marshal(pc)
		if err != nil {
			return true, err
		}
		var submitted []config.PairedClient
		if err := json.Unmarshal(b, &submitted); err != nil {
			return true, err
		}
		if !pairedClientsEqual(submitted, diskMgmt.PairedClients) {
			return true, nil
		}
	}

	if pi, ok := mgmtMap["pair_interfaces"]; ok {
		b, err := json.Marshal(pi)
		if err != nil {
			return true, err
		}
		var submitted []string
		if err := json.Unmarshal(b, &submitted); err != nil {
			return true, err
		}
		if !stringSliceEqual(submitted, diskMgmt.PairInterfaces) {
			return true, nil
		}
	}

	if ps, ok := mgmtMap["pair_subnets"]; ok {
		b, err := json.Marshal(ps)
		if err != nil {
			return true, err
		}
		var submitted []string
		if err := json.Unmarshal(b, &submitted); err != nil {
			return true, err
		}
		if !stringSliceEqual(submitted, diskMgmt.PairSubnets) {
			return true, nil
		}
	}

	if en, ok := mgmtMap["enabled"]; ok {
		if submittedEnabled, ok := en.(bool); ok {
			if submittedEnabled != diskMgmt.Enabled {
				return true, nil
			}
		}
	}

	return false, nil
}

// restoreMgmtSecurityFields overwrites the security-boundary fields in dst
// with authoritative disk values, making the config API blind to any submitted values.
func restoreMgmtSecurityFields(dst *config.MgmtConfig, disk config.MgmtConfig) {
	dst.PairedClients = disk.PairedClients
	dst.PairInterfaces = disk.PairInterfaces
	dst.PairSubnets = disk.PairSubnets
	dst.Enabled = disk.Enabled
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func pairedClientsEqual(a, b []config.PairedClient) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].TokenSHA256 != b[i].TokenSHA256 || a[i].Created != b[i].Created {
			return false
		}
	}
	return true
}

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
