package mgmt

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// ---- test helpers -----------------------------------------------------------

const minimalConfigJSON = `{"version":3,"gateway":{"port":9001},"mgmt":{"enabled":true,"pair_subnets":["127.0.0.1/32"]},"model_list":[]}`

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(minimalConfigJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Cfg: config.MgmtConfig{
			Enabled:     true,
			PairSubnets: []string{"127.0.0.1/32"},
		},
		ConfigPath:    cfgPath,
		HomePath:      homeDir,
		WorkspacePath: wsDir,
		Version:       "test-1.0",
	}
	return New(opts), dir
}

func postJSON(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.handlePair(w, req)
	return w
}

func pairClient(t *testing.T, srv *Server, clientName string) string {
	t.Helper()
	w := postJSON(t, srv, "/mgmt/v1/pair", fmt.Sprintf(`{"client_name":%q}`, clientName))
	if w.Code != http.StatusOK {
		t.Fatalf("pair failed: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	token := resp["token"]
	if token == "" {
		t.Fatal("pair returned empty token")
	}
	return token
}

func authedGet(t *testing.T, srv *Server, urlPath, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, urlPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.withAuth(srv.handleConfig)(w, req)
	return w
}

func authedPatch(t *testing.T, srv *Server, urlPath, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, urlPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.withAuth(srv.handleConfig)(w, req)
	return w
}

// ---- isPairAllowed ----------------------------------------------------------

func TestIsPairAllowed_Subnet(t *testing.T) {
	cfg := config.MgmtConfig{PairSubnets: []string{"127.0.0.1/32"}}

	allowed := []string{"127.0.0.1:12345", "127.0.0.1:9999"}
	for _, addr := range allowed {
		if !isPairAllowed(addr, cfg) {
			t.Errorf("expected %s to be allowed", addr)
		}
	}

	denied := []string{"10.0.0.1:12345", "192.168.1.1:9000"}
	for _, addr := range denied {
		if isPairAllowed(addr, cfg) {
			t.Errorf("expected %s to be denied", addr)
		}
	}
}

func TestIsPairAllowed_EmptyConfig(t *testing.T) {
	cfg := config.MgmtConfig{}
	if isPairAllowed("127.0.0.1:1234", cfg) {
		t.Error("empty config should deny all")
	}
}

func TestIsPairAllowed_CIDRRange(t *testing.T) {
	cfg := config.MgmtConfig{PairSubnets: []string{"192.168.1.0/24"}}
	if !isPairAllowed("192.168.1.100:1234", cfg) {
		t.Error("192.168.1.100 should be in 192.168.1.0/24")
	}
	if isPairAllowed("192.168.2.1:1234", cfg) {
		t.Error("192.168.2.1 should not be in 192.168.1.0/24")
	}
}

// ---- pairRateLimiter --------------------------------------------------------

func TestPairRateLimiter_BasicAllow(t *testing.T) {
	rl := newPairRateLimiter()
	for i := 0; i < pairRateMax; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Error("6th request in window should be denied")
	}
}

func TestPairRateLimiter_PerIP(t *testing.T) {
	rl := newPairRateLimiter()
	for i := 0; i < pairRateMax; i++ {
		rl.allow("1.2.3.4")
	}
	if !rl.allow("5.6.7.8") {
		t.Error("different IP should not be rate limited")
	}
}

func TestPairRateLimiter_WindowReset(t *testing.T) {
	rl := newPairRateLimiter()
	// Exhaust window
	for i := 0; i < pairRateMax; i++ {
		rl.allow("1.2.3.4")
	}
	// Manually expire the window
	rl.mu.Lock()
	rl.entries["1.2.3.4"].windowEnd = time.Now().Add(-time.Second)
	rl.mu.Unlock()
	if !rl.allow("1.2.3.4") {
		t.Error("request after window expiry should be allowed")
	}
}

// ---- authTracker ------------------------------------------------------------

func TestAuthTracker_LockoutAfterMaxFails(t *testing.T) {
	at := newAuthTracker()
	for i := 0; i < authMaxFails-1; i++ {
		at.recordFailure("1.2.3.4")
		if at.isLocked("1.2.3.4") {
			t.Fatalf("should not be locked after %d failures", i+1)
		}
	}
	at.recordFailure("1.2.3.4")
	if !at.isLocked("1.2.3.4") {
		t.Errorf("should be locked after %d failures", authMaxFails)
	}
}

func TestAuthTracker_SuccessResets(t *testing.T) {
	at := newAuthTracker()
	for i := 0; i < authMaxFails-1; i++ {
		at.recordFailure("1.2.3.4")
	}
	at.recordSuccess("1.2.3.4")
	at.recordFailure("1.2.3.4") // one more — should not lock
	if at.isLocked("1.2.3.4") {
		t.Error("should not be locked after reset")
	}
}

// ---- token round-trip -------------------------------------------------------

func TestTokenRoundTrip(t *testing.T) {
	srv, _ := newTestServer(t)

	token := pairClient(t, srv, "windows-mgmt")

	// Token should authenticate
	w := authedGet(t, srv, "/mgmt/v1/config", token)
	if w.Code != http.StatusOK {
		t.Errorf("valid token should authenticate, got %d %s", w.Code, w.Body.String())
	}
}

func TestTokenReject_WrongToken(t *testing.T) {
	srv, _ := newTestServer(t)
	pairClient(t, srv, "windows-mgmt")

	w := authedGet(t, srv, "/mgmt/v1/config", "00000000deadbeef00000000deadbeef00000000deadbeef00000000deadbeef")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token should get 401, got %d", w.Code)
	}
}

func TestTokenReject_NoToken(t *testing.T) {
	srv, _ := newTestServer(t)
	pairClient(t, srv, "windows-mgmt")

	req := httptest.NewRequest(http.MethodGet, "/mgmt/v1/config", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.withAuth(srv.handleConfig)(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing token should get 401, got %d", w.Code)
	}
}

// ---- re-pair rotates token --------------------------------------------------

func TestRePair_OldTokenRejected(t *testing.T) {
	srv, _ := newTestServer(t)

	oldToken := pairClient(t, srv, "windows-mgmt")
	newToken := pairClient(t, srv, "windows-mgmt") // same client_name

	if oldToken == newToken {
		t.Fatal("re-pair should generate a different token")
	}

	// Old token should be rejected
	w := authedGet(t, srv, "/mgmt/v1/config", oldToken)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("old token should get 401 after re-pair, got %d", w.Code)
	}

	// New token should work
	w = authedGet(t, srv, "/mgmt/v1/config", newToken)
	if w.Code != http.StatusOK {
		t.Errorf("new token should authenticate, got %d", w.Code)
	}
}

// ---- pair source restriction ------------------------------------------------

func TestPair_ForbiddenSource(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/mgmt/v1/pair", strings.NewReader(`{"client_name":"evil"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:12345" // not in 127.0.0.1/32
	w := httptest.NewRecorder()
	srv.handlePair(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-whitelisted source should get 403, got %d", w.Code)
	}
}

func TestPair_RateLimit(t *testing.T) {
	srv, _ := newTestServer(t)
	// Exhaust the rate limit
	for i := 0; i < pairRateMax; i++ {
		postJSON(t, srv, "/mgmt/v1/pair", `{"client_name":"test"}`)
	}
	w := postJSON(t, srv, "/mgmt/v1/pair", `{"client_name":"test"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("exceeded rate limit should get 429, got %d", w.Code)
	}
}

// ---- config masking ---------------------------------------------------------

func TestMaskConfigForOutput(t *testing.T) {
	cfg := &config.Config{}
	// Set a SecureString via json round-trip
	raw := `{"gateway":{"port":9001}}`
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatal(err)
	}

	out, err := maskConfigForOutput(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// [NOT_HERE] values should become ********
	checkNoNotHere(t, out)
}

func checkNoNotHere(t *testing.T, v any) {
	t.Helper()
	switch vt := v.(type) {
	case map[string]any:
		for _, val := range vt {
			if s, ok := val.(string); ok && s == notHereSentinel {
				t.Errorf("found raw [NOT_HERE] in masked output")
			}
			checkNoNotHere(t, val)
		}
	case []any:
		for _, item := range vt {
			checkNoNotHere(t, item)
		}
	}
}

func TestMaskConfig_SecretAppearsAsMask(t *testing.T) {
	// Build a map that simulates a marshalled config with a [NOT_HERE] value
	m := map[string]any{
		"some_secret": notHereSentinel,
		"nested": map[string]any{
			"api_key": notHereSentinel,
		},
	}
	replaceNotHereInMap(m)
	if m["some_secret"] != secretMask {
		t.Errorf("expected %q, got %q", secretMask, m["some_secret"])
	}
	nested := m["nested"].(map[string]any)
	if nested["api_key"] != secretMask {
		t.Errorf("expected %q, got %q", secretMask, nested["api_key"])
	}
}

// ---- sentinel handling ------------------------------------------------------

func TestReplaceSentinelInMap(t *testing.T) {
	m := map[string]any{
		"key":    secretMask,
		"normal": "hello",
		"nested": map[string]any{
			"s": secretMask,
		},
		"list": []any{secretMask, "other"},
	}
	replaceSentinelInMap(m)
	if m["key"] != notHereSentinel {
		t.Errorf("expected [NOT_HERE], got %v", m["key"])
	}
	if m["normal"] != "hello" {
		t.Errorf("normal values must not change")
	}
	nested := m["nested"].(map[string]any)
	if nested["s"] != notHereSentinel {
		t.Errorf("nested sentinel not replaced")
	}
}

// ---- merge patch ------------------------------------------------------------

func TestMergePatch_ScalarOverride(t *testing.T) {
	base := map[string]any{"a": "old", "b": "keep"}
	patch := map[string]any{"a": "new"}
	mergePatch(base, patch)
	if base["a"] != "new" {
		t.Error("scalar override failed")
	}
	if base["b"] != "keep" {
		t.Error("unpatched key must not change")
	}
}

func TestMergePatch_DeleteNull(t *testing.T) {
	base := map[string]any{"a": "x", "b": "y"}
	patch := map[string]any{"a": nil}
	mergePatch(base, patch)
	if _, ok := base["a"]; ok {
		t.Error("null patch value should delete key")
	}
}

func TestMergePatch_NestedMerge(t *testing.T) {
	base := map[string]any{
		"gateway": map[string]any{"port": float64(9001), "host": "localhost"},
	}
	patch := map[string]any{
		"gateway": map[string]any{"port": float64(9002)},
	}
	mergePatch(base, patch)
	gw := base["gateway"].(map[string]any)
	if gw["port"] != float64(9002) {
		t.Error("nested port should be updated")
	}
	if gw["host"] != "localhost" {
		t.Error("nested host should be preserved")
	}
}

// ---- files whitelist --------------------------------------------------------

func TestValidateFilePath_Allowed(t *testing.T) {
	allowed := []string{
		"fan-policy.json",
		"fan-mode.txt",
		"skills/my-skill.json",
		"skills/sub/file.json",
	}
	for _, p := range allowed {
		if err := validateFilePath(p); err != nil {
			t.Errorf("path %q should be allowed: %v", p, err)
		}
	}
}

func TestValidateFilePath_Denied(t *testing.T) {
	denied := []string{
		"config.json",
		"../config.json",
		"../../etc/passwd",
		".security.yml",
		"skills/../config.json",
		"",
	}
	for _, p := range denied {
		if p == "" {
			continue // empty is handled before validateFilePath
		}
		if err := validateFilePath(p); err == nil {
			t.Errorf("path %q should be denied", p)
		}
	}
}

// ---- /info no sensitive fields ----------------------------------------------

func TestInfoNoSensitiveFields(t *testing.T) {
	srv, _ := newTestServer(t)

	// Pair a client first so paired_clients is non-empty
	pairClient(t, srv, "test-client")

	req := httptest.NewRequest(http.MethodGet, "/mgmt/v1/info", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.handleInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /info failed: %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// Must have identity fields
	if resp["device_id"] == nil || resp["device_id"] == "" {
		t.Error("device_id missing")
	}
	if resp["api_version"] != float64(1) {
		t.Errorf("api_version should be integer 1, got %v", resp["api_version"])
	}

	// Must NOT expose paired_clients list or any token hash
	if _, ok := resp["paired_clients"]; ok {
		t.Error("info must not expose paired_clients")
	}
	if _, ok := resp["token"]; ok {
		t.Error("info must not expose token")
	}
	if _, ok := resp["token_sha256"]; ok {
		t.Error("info must not expose token_sha256")
	}

	// paired should be a bool, not the list
	if _, ok := resp["paired"].(bool); !ok {
		t.Error("paired field should be a bool")
	}
}

// ---- generateToken ----------------------------------------------------------

func TestGenerateToken_Uniqueness(t *testing.T) {
	tokens := make(map[string]bool)
	for i := 0; i < 20; i++ {
		tok, hash, err := generateToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 64 {
			t.Errorf("token should be 64 hex chars, got %d", len(tok))
		}
		if len(hash) != 64 {
			t.Errorf("token hash should be 64 hex chars, got %d", len(hash))
		}
		if tokens[tok] {
			t.Errorf("duplicate token generated at iteration %d", i)
		}
		tokens[tok] = true
	}
}

// ---- authTracker lockout blocks requests ------------------------------------

func TestAuthLockoutBlocksRequests(t *testing.T) {
	srv, _ := newTestServer(t)
	pairClient(t, srv, "test-client")

	ip := "127.0.0.1"
	// Trigger lockout by recording enough failures directly
	for i := 0; i < authMaxFails; i++ {
		srv.authTrk.recordFailure(ip)
	}

	req := httptest.NewRequest(http.MethodGet, "/mgmt/v1/config", nil)
	req.Header.Set("Authorization", "Bearer validtoken")
	req.RemoteAddr = ip + ":12345"
	w := httptest.NewRecorder()
	srv.withAuth(srv.handleConfig)(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("locked IP should get 429, got %d", w.Code)
	}
}

// ---- CRITICAL-1: config API cannot modify security-boundary mgmt fields -----

func TestPatchConfig_RejectPairedClients(t *testing.T) {
	srv, dir := newTestServer(t)
	token := pairClient(t, srv, "test-client")

	w := authedPatch(t, srv, "/mgmt/v1/config", token, `{"mgmt":{"paired_clients":[]}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("patching paired_clients should get 400, got %d %s", w.Code, w.Body.String())
	}

	// Verify disk unchanged — paired_clients still has 1 entry
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Mgmt.PairedClients) != 1 {
		t.Errorf("disk should still have 1 paired client, got %d", len(cfg.Mgmt.PairedClients))
	}
}

func TestPatchConfig_RejectPairSubnets(t *testing.T) {
	srv, _ := newTestServer(t)
	token := pairClient(t, srv, "test-client")

	w := authedPatch(t, srv, "/mgmt/v1/config", token, `{"mgmt":{"pair_subnets":["0.0.0.0/0"]}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("patching pair_subnets should get 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestPatchConfig_RejectMgmtEnabled(t *testing.T) {
	srv, _ := newTestServer(t)
	token := pairClient(t, srv, "test-client")

	w := authedPatch(t, srv, "/mgmt/v1/config", token, `{"mgmt":{"enabled":false}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("patching mgmt.enabled should get 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestPatchConfig_AllowDeviceName(t *testing.T) {
	srv, _ := newTestServer(t)
	token := pairClient(t, srv, "test-client")

	w := authedPatch(t, srv, "/mgmt/v1/config", token, `{"mgmt":{"device_name":"renamed-device"}}`)
	if w.Code != http.StatusOK {
		t.Errorf("patching device_name should succeed, got %d %s", w.Code, w.Body.String())
	}
}

// ---- MEDIUM-2: rate limiter map size is bounded -----------------------------

func TestRateLimiterMaxEntries(t *testing.T) {
	rl := newPairRateLimiter()
	// Insert more unique IPs than the hard cap
	for i := 0; i < rateLimiterMaxEntries+200; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		rl.allow(ip)
	}
	rl.mu.Lock()
	size := len(rl.entries)
	rl.mu.Unlock()
	if size > rateLimiterMaxEntries {
		t.Errorf("rate limiter map size %d exceeds cap %d", size, rateLimiterMaxEntries)
	}
}
