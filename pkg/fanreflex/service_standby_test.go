package fanreflex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/state"
)

// ─── standby (BotLink offline vs fail-safe) semantics ─────────────────────────
//
// These tests cover §4.4 of doc/task-m3a-botlink-go-instructions.md: an
// offline target bot must enter "standby" (no fail-safe, no repeat
// notifications, decision log tagged "state":"standby"), and reconnecting
// must resume normal control with a forced re-send (LastSentPWM cleared)
// since the actual fan state during the offline window is unknown. A
// connected-but-failing bot must still trigger the existing fail-safe path
// unchanged (regression guard).

// makeStandbyTestService builds a minimal Service with one fixed-speed "pump"
// actuator, a real bus.MessageBus + state.Manager (so notify() actually
// publishes, letting tests assert on notification counts), and a mockSender.
func makeStandbyTestService(t *testing.T) (*Service, *mockSender, *bus.MessageBus) {
	t.Helper()
	dir := t.TempDir()
	modeFile := filepath.Join(dir, "fan-mode.txt")
	if err := os.WriteFile(modeFile, []byte("balanced"), 0o644); err != nil {
		t.Fatal(err)
	}

	mb := bus.NewMessageBus()
	t.Cleanup(mb.Close)

	sm := state.NewManager(dir)
	// "telegram" is not an internal channel (see pkg/constants.IsInternalChannel),
	// so notify() will actually publish to mb — required for these tests to
	// observe notification behaviour.
	if err := sm.SetLastChannel("telegram:999"); err != nil {
		t.Fatalf("SetLastChannel: %v", err)
	}

	mock := &mockSender{sensorJSON: `{"success":true,"data":[{"name":"VRM","value":45}]}`}
	fixed := 100
	policy := &Policy{
		Version:     1,
		TickSeconds: 15,
		Sensors:     map[string]string{"vrm": "VRM"},
		Derived:     map[string]DerivedDef{},
		Actuators:   map[string]ActuatorDef{"pump": {Channel: "Pump Channel"}},
		Modes: map[string]map[string]ActuatorMode{
			"balanced": {"pump": {FixedPct: &fixed}},
		},
		FailSafe: FailSafeDef{
			Commands: []FailSafeCommand{{Channel: "Pump Channel", PWM: 100}},
		},
	}

	svc := &Service{
		cfg:          config.FanReflexConfig{ModePath: modeFile, TargetBotID: "bot1"},
		policy:       policy,
		logPath:      filepath.Join(dir, "test.jsonl"),
		client:       mock,
		msgBus:       mb,
		stateMan:     sm,
		filterStates: map[string]*ActuatorFilterState{"pump": {}},
		tachStates:   map[string]*tachState{},
		lastSentPWM:  map[string]*int{},
		startedAt:    time.Now(),
	}
	t.Cleanup(func() { svc.closeLog() })
	return svc, mock, mb
}

// drainOutbound counts OutboundMessages published within quiet before giving up.
func drainOutbound(t *testing.T, mb *bus.MessageBus, quiet time.Duration) int {
	t.Helper()
	ch := mb.OutboundChan()
	count := 0
	for {
		select {
		case <-ch:
			count++
		case <-time.After(quiet):
			return count
		}
	}
}

func readLogLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("log not written: %v", err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("log line not valid JSON: %v\nline: %s", err, line)
		}
		rows = append(rows, row)
	}
	return rows
}

// TestStandby_NoFailSafe_NoRepeatNotify_LogsStandby verifies that while the
// target bot is disconnected: no SetFanSpeed commands are sent, fail-safe is
// never triggered, exactly one notification fires across multiple offline
// ticks (not one per tick), and every tick's decision log line is tagged
// "state":"standby".
func TestStandby_NoFailSafe_NoRepeatNotify_LogsStandby(t *testing.T) {
	svc, mock, mb := makeStandbyTestService(t)
	mock.disconnected = true

	for i := 0; i < 3; i++ {
		svc.runTick()
	}

	if svc.failSafeActive {
		t.Error("failSafeActive should remain false while the target bot is offline (standby, not fault)")
	}
	if mock.setFanSpeedCalls() != 0 {
		t.Errorf("no SetFanSpeed commands should be sent while offline, got %d", mock.setFanSpeedCalls())
	}

	notifyCount := drainOutbound(t, mb, 300*time.Millisecond)
	if notifyCount != 1 {
		t.Errorf("expected exactly 1 standby notification across 3 offline ticks, got %d", notifyCount)
	}

	rows := readLogLines(t, svc.logPath)
	if len(rows) != 3 {
		t.Fatalf("expected 3 log lines, got %d", len(rows))
	}
	for i, row := range rows {
		if row["state"] != "standby" {
			t.Errorf("line %d: expected state=standby, got %v", i, row["state"])
		}
	}
}

// TestStandby_Reconnect_ResumesAndForcesResend verifies that after a
// disconnect/reconnect cycle: control resumes, a recovery notification
// fires, standbyActive clears, and — because LastSentPWM was cleared on
// reconnect — the actuator is re-sent even though its target value (100)
// did not change from before the outage.
func TestStandby_Reconnect_ResumesAndForcesResend(t *testing.T) {
	svc, mock, mb := makeStandbyTestService(t)

	// Tick 1: connected, normal initial send.
	svc.runTick()
	if got := mock.setFanSpeedCalls(); got != 1 {
		t.Fatalf("tick 1: want 1 SetFanSpeed (initial), got %d", got)
	}
	if svc.lastSentPWM["pump"] == nil {
		t.Fatal("lastSentPWM[pump] should be set after the first send")
	}

	// Go offline for two ticks.
	mock.disconnected = true
	svc.runTick()
	svc.runTick()
	if got := mock.setFanSpeedCalls(); got != 1 {
		t.Errorf("no new SetFanSpeed should be sent while offline, got %d", got)
	}
	drainOutbound(t, mb, 200*time.Millisecond) // discard the standby notification

	// Reconnect: target PWM (100) is unchanged from before the outage, but
	// the tick must still resend because LastSentPWM was cleared.
	mock.disconnected = false
	svc.runTick()

	if got := mock.setFanSpeedCalls(); got != 2 {
		t.Errorf("reconnect tick: expected a forced resend (2 total SetFanSpeed calls), got %d", got)
	}
	if svc.standbyActive {
		t.Error("standbyActive should be false after reconnecting")
	}

	notifyCount := drainOutbound(t, mb, 200*time.Millisecond)
	if notifyCount != 1 {
		t.Errorf("expected exactly 1 recovery notification, got %d", notifyCount)
	}

	rows := readLogLines(t, svc.logPath)
	if len(rows) != 4 {
		t.Fatalf("expected 4 log lines (1 normal + 2 standby + 1 recovery), got %d", len(rows))
	}
	if rows[3]["state"] != nil && rows[3]["state"] != "" {
		t.Errorf("recovery tick log should not be tagged standby, got state=%v", rows[3]["state"])
	}
}

// TestStandby_ConnectedButReadFails_FailSafeStillTriggers is a regression
// guard: when the target bot IS connected but sensor reads fail, the
// existing fail-safe ladder must still trigger exactly as before — only a
// disconnected target enters standby instead of fail-safe.
func TestStandby_ConnectedButReadFails_FailSafeStillTriggers(t *testing.T) {
	svc, mock, _ := makeStandbyTestService(t)
	mock.sensorErr = fmt.Errorf("simulated sensor read failure")

	for i := 0; i < 3; i++ {
		svc.runTick()
	}

	if !svc.failSafeActive {
		t.Fatal("expected fail-safe to trigger after 3 consecutive failed reads while the bot is connected")
	}
	if svc.standbyActive {
		t.Error("standbyActive must not be set when the bot is connected (even if reads are failing)")
	}
}
