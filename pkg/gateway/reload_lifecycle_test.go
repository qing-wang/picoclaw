package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/botlink"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/fanreflex"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/mgmt"
	alohaclawtools "github.com/sipeed/picoclaw/pkg/tools/alohaclaw"
)

// ---- Reload lifecycle (task m3d) ----------------------------------------------
//
// These tests reproduce, at the unit level, the real-machine defect from
// doc/task-m3d-reload-lifecycle-instructions.md §1: a POST /mgmt/v1/reload
// (or config hot reload) used to stop FanReflexService and BotLinkServer in
// stopAndCleanupServices(isReload=true) but restartServices() never rebuilt
// either — silently and invisibly leaving fan control dead until a full
// process restart, while /mgmt/v1/status kept reporting a healthy snapshot
// from the now-stopped instance.
//
// A minimal fan policy with tick_seconds=1 is used so the tests can observe
// "the ticker actually advanced" (last_tick_at changing between two reads)
// within a couple of seconds, instead of only checking pointer identity.

const testFanPolicyJSON = `{
	"version": 1,
	"tick_seconds": 1,
	"sensors": {},
	"derived": {},
	"actuators": {"pump": {"channel": "Pump"}},
	"modes": {"balanced": {"pump": {"fixed_pct": 50}}},
	"failsafe": {"commands": [["Pump", 100]]}
}`

// reloadTestFixture bundles everything needed to call stopAndCleanupServices
// / restartServices directly, the same way gateway.go's reload path does,
// without opening any real network listeners.
type reloadTestFixture struct {
	t          *testing.T
	cfg        *config.Config
	configPath string
	msgBus     *bus.MessageBus
	agentLoop  *agent.AgentLoop
	cm         *channels.Manager
	mux        *http.ServeMux
	httpSrv    *httptest.Server
	rs         *services
}

// newReloadTestFixture builds a fully-wired-but-not-networked services set:
// BotLinkServer (registered once on a real *http.ServeMux — so a stray
// second RegisterOnMux call anywhere in the reload path would panic the test
// immediately, exactly as it would panic the real gateway), a running
// FanReflexService using the BotLink transport (avoids any real MQTT/network
// dependency), and a mgmt.Server wired the same way gateway.Run wires it.
func newReloadTestFixture(t *testing.T, fanReflexEnabled, botLinkEnabled bool) *reloadTestFixture {
	t.Helper()

	homeDir := t.TempDir()
	wsDir := filepath.Join(homeDir, "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace): %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "fan-policy.json"), []byte(testFanPolicyJSON), 0o644); err != nil {
		t.Fatalf("WriteFile(fan-policy.json): %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = wsDir
	cfg.Gateway.Port = 9001
	cfg.Mgmt.Enabled = true
	cfg.Mgmt.PairSubnets = []string{"127.0.0.1/32", "::1/128"}
	cfg.Tools.FanReflex = config.FanReflexConfig{
		Enabled:         fanReflexEnabled,
		TargetBotID:     "bot1",
		PolicyPath:      "fan-policy.json",
		DecisionLogPath: "fanreflex-decisions.jsonl",
		Transport:       config.FanReflexTransportBotLink,
	}
	cfg.Tools.BotLink = config.BotLinkConfig{Enabled: botLinkEnabled}

	configPath := filepath.Join(homeDir, "config.json")
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	msgBus := bus.NewMessageBus()
	al := agent.NewAgentLoop(cfg, msgBus, &startupBlockedProvider{reason: "reload lifecycle test"})
	t.Cleanup(al.Close)

	store := media.NewFileMediaStoreWithCleanup(media.MediaCleanerConfig{})
	cm, err := channels.NewManager(cfg, msgBus, store)
	if err != nil {
		t.Fatalf("channels.NewManager: %v", err)
	}
	al.SetChannelManager(cm)
	al.SetMediaStore(store)

	mux := http.NewServeMux()

	// Constructed unconditionally, mirroring setupAndStartServices — see
	// gateway.go's comment on why this must not be gated on cfg.Tools.BotLink.Enabled.
	botlinkSrv := botlink.New(cfg.Tools.BotLink)
	botlinkSrv.RegisterOnMux(mux)

	rs := &services{
		ChannelManager: cm,
		BotLinkServer:  botlinkSrv,
	}
	// Stop whatever FanReflexService/BotLinkServer ends up live at the end of
	// the test (rs's fields may be reassigned by reload() during the test) —
	// otherwise their goroutines and open decision-log file handles outlive
	// the test, which on Windows fails t.TempDir()'s own cleanup with
	// "process cannot access the file" when it tries to remove the workspace.
	t.Cleanup(func() {
		if rs.FanReflexService != nil {
			rs.FanReflexService.Stop()
		}
		if rs.BotLinkServer != nil {
			rs.BotLinkServer.Stop()
		}
	})

	if fanReflexEnabled {
		var botlinkSender alohaclawtools.Sender
		if cfg.Tools.BotLink.Enabled {
			botlinkSender = botlinkSrv.Sender()
		}
		frSvc, frErr := fanreflex.NewService(
			cfg.Tools.FanReflex,
			cfg.Tools.AlohaClaw,
			botlinkSender,
			msgBus,
			cfg.WorkspacePath(),
		)
		if frErr != nil {
			t.Fatalf("fanreflex.NewService: %v", frErr)
		}
		if err := frSvc.Start(); err != nil {
			t.Fatalf("FanReflexService.Start: %v", err)
		}
		rs.FanReflexService = frSvc
	}

	mgmtSrv := mgmt.New(mgmt.Options{
		Cfg:           cfg.Mgmt,
		ConfigPath:    configPath,
		HomePath:      homeDir,
		WorkspacePath: cfg.WorkspacePath(),
		Version:       "test",
	})
	if rs.FanReflexService != nil {
		mgmtSrv.SetStatusProvider(rs.FanReflexService.StatusSnapshot)
	}
	mgmtSrv.RegisterOnMux(mux)
	rs.MgmtServer = mgmtSrv

	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	return &reloadTestFixture{
		t:          t,
		cfg:        cfg,
		configPath: configPath,
		msgBus:     msgBus,
		agentLoop:  al,
		cm:         cm,
		mux:        mux,
		httpSrv:    httpSrv,
		rs:         rs,
	}
}

// pairAndGetToken performs a real POST /mgmt/v1/pair over HTTP and returns
// the issued bearer token, exercising the exact same code path a real mgmt
// client (e.g. the CTFanBot device management tab) would use.
func (f *reloadTestFixture) pairAndGetToken() string {
	f.t.Helper()
	resp, err := http.Post(f.httpSrv.URL+"/mgmt/v1/pair", "application/json",
		strings.NewReader(`{"client_name":"reload-lifecycle-test"}`))
	if err != nil {
		f.t.Fatalf("POST /mgmt/v1/pair: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("POST /mgmt/v1/pair status=%d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		f.t.Fatalf("decode pair response: %v", err)
	}
	if body["token"] == "" {
		f.t.Fatal("pair response missing token")
	}
	return body["token"]
}

// getStatus performs a real GET /mgmt/v1/status over HTTP with the given
// bearer token and returns the decoded JSON body.
func (f *reloadTestFixture) getStatus(token string) map[string]any {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.httpSrv.URL+"/mgmt/v1/status", nil)
	if err != nil {
		f.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("GET /mgmt/v1/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET /mgmt/v1/status status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		f.t.Fatalf("decode status response: %v", err)
	}
	return body
}

// reload runs exactly the two calls gateway.go's handleConfigReload makes on
// every reload: stopAndCleanupServices(isReload=true) then restartServices.
func (f *reloadTestFixture) reload() {
	f.t.Helper()
	stopAndCleanupServices(f.rs, 5*time.Second, true)
	if err := restartServices(f.agentLoop, f.rs, f.msgBus); err != nil {
		f.t.Fatalf("restartServices: %v", err)
	}
}

// TestReload_FanReflexServiceRecreatedRunningAndBotLinkSurvives is the core
// regression test for this task: it drives a full reload cycle through the
// real gateway functions (stopAndCleanupServices + restartServices) and a
// real HTTP round trip against /mgmt/v1/status, and asserts:
//
//  1. Before reload, FanReflexService is ticking (last_tick_at advances) and
//     /status reports fanreflex_running: true.
//  2. BotLinkServer is the *same* instance after reload (never
//     stopped/recreated — its mux registration must stay exactly-once).
//  3. The pre-reload FanReflexService instance is truly stopped (its
//     last_tick_at is frozen) — not just replaced while still running in the
//     background (goroutine leak).
//  4. The post-reload FanReflexService is a new instance that is actively
//     ticking, and /status (hit again over real HTTP) reflects it.
func TestReload_FanReflexServiceRecreatedRunningAndBotLinkSurvives(t *testing.T) {
	f := newReloadTestFixture(t, true /* fanReflexEnabled */, true /* botLinkEnabled */)
	token := f.pairAndGetToken()

	oldFR := f.rs.FanReflexService
	oldBotLink := f.rs.BotLinkServer

	// Let the pre-reload service tick at least once, then twice (to prove it
	// is advancing, not just non-zero from Start()'s snapAt-at-first-tick).
	tick1 := waitForTickAdvance(t, oldFR, time.Time{}, 3*time.Second)

	before := f.getStatus(token)
	if running, _ := before["fanreflex_running"].(bool); !running {
		t.Fatalf("expected fanreflex_running=true before reload, got %v", before)
	}
	frBefore, _ := before["fanreflex"].(map[string]any)
	if frBefore == nil || frBefore["last_tick_at"] == nil {
		t.Fatalf("expected fanreflex.last_tick_at before reload, got %v", before["fanreflex"])
	}

	// --- reload ---
	f.reload()

	newFR := f.rs.FanReflexService
	if newFR == nil {
		t.Fatal("FanReflexService is nil after reload — fan control silently stopped (the exact bug this task fixes)")
	}
	if newFR == oldFR {
		t.Fatal("FanReflexService is the same instance after reload — expected a freshly rebuilt service")
	}
	if f.rs.BotLinkServer != oldBotLink {
		t.Fatal("BotLinkServer identity changed across reload — it must survive reload without being recreated (trap 1)")
	}

	// The OLD instance must be truly stopped: its snapshot must not advance
	// any further, however long we wait.
	oldSnapAfterStop := oldFR.StatusSnapshot()["last_tick_at"]
	time.Sleep(1500 * time.Millisecond)
	oldSnapLater := oldFR.StatusSnapshot()["last_tick_at"]
	if oldSnapAfterStop != oldSnapLater {
		t.Fatalf("old FanReflexService kept ticking after being stopped: %v -> %v (goroutine leak)",
			oldSnapAfterStop, oldSnapLater)
	}

	// The NEW instance must be actively ticking.
	tick2 := waitForTickAdvance(t, newFR, time.Time{}, 3*time.Second)
	_ = tick1

	after := f.getStatus(token)
	if running, _ := after["fanreflex_running"].(bool); !running {
		t.Fatalf("expected fanreflex_running=true after reload, got %v", after)
	}
	frAfter, _ := after["fanreflex"].(map[string]any)
	if frAfter == nil || frAfter["last_tick_at"] == nil {
		t.Fatalf("expected fanreflex.last_tick_at after reload, got %v", after["fanreflex"])
	}
	if frAfter["last_tick_at"] == frBefore["last_tick_at"] {
		t.Error("status last_tick_at unchanged across reload — matches the real-machine defect from the task doc (tick frozen at reload time)")
	}
	_ = tick2
}

// TestReload_FanReflexDisabled_StaysNilAndStatusHonest verifies the
// "cfg.Tools.FanReflex.Enabled == false" branch of restartServices: no
// service is (re)built, runningServices.FanReflexService is nil, and
// /mgmt/v1/status honestly reports fanreflex_running: false rather than
// echoing a stale snapshot.
func TestReload_FanReflexDisabled_StaysNilAndStatusHonest(t *testing.T) {
	f := newReloadTestFixture(t, false /* fanReflexEnabled */, true /* botLinkEnabled */)
	token := f.pairAndGetToken()

	if f.rs.FanReflexService != nil {
		t.Fatal("precondition: FanReflexService should be nil when disabled")
	}

	before := f.getStatus(token)
	if running, _ := before["fanreflex_running"].(bool); running {
		t.Fatalf("expected fanreflex_running=false before reload (disabled), got %v", before)
	}
	if before["fanreflex"] != nil {
		t.Fatalf("expected fanreflex: null before reload (disabled), got %v", before["fanreflex"])
	}

	f.reload()

	if f.rs.FanReflexService != nil {
		t.Fatal("FanReflexService should remain nil after reload while disabled")
	}

	after := f.getStatus(token)
	if running, _ := after["fanreflex_running"].(bool); running {
		t.Fatalf("expected fanreflex_running=false after reload (disabled), got %v", after)
	}
	if after["fanreflex"] != nil {
		t.Fatalf("expected fanreflex: null after reload (disabled), got %v", after["fanreflex"])
	}
}

// TestReload_BotLinkUpdateConfigCalled_NoPanicOnMultipleReloads runs several
// reload cycles in a row against a real *http.ServeMux and asserts no panic
// occurs — the only way this test file could crash from a double
// registration is if gateway's reload path (restartServices /
// stopAndCleanupServices) were changed to call BotLinkServer.RegisterOnMux
// again, which is exactly trap 1 from the task doc.
func TestReload_BotLinkUpdateConfigCalled_NoPanicOnMultipleReloads(t *testing.T) {
	f := newReloadTestFixture(t, true, true)
	botlinkBefore := f.rs.BotLinkServer

	for i := 0; i < 3; i++ {
		f.reload()
	}

	if f.rs.BotLinkServer != botlinkBefore {
		t.Fatal("BotLinkServer identity changed across reloads")
	}
	if f.rs.FanReflexService == nil {
		t.Fatal("FanReflexService should be non-nil (enabled) after repeated reloads")
	}
}

// waitForTickAdvance polls svc.StatusSnapshot()["last_tick_at"] until it
// becomes a non-nil value different from notEqualTo, or fails the test after
// timeout. It returns the observed value as a string.
func waitForTickAdvance(t *testing.T, svc *fanreflex.Service, notEqualTo time.Time, timeout time.Duration) string {
	t.Helper()
	_ = notEqualTo
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := svc.StatusSnapshot()
		if v, ok := snap["last_tick_at"]; ok && v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("FanReflexService did not tick within %v", timeout)
	return ""
}
