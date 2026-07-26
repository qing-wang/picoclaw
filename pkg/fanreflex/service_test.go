package fanreflex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// ─── Mock MQTT sender ─────────────────────────────────────────────────────────

// mockSender records commands sent and returns canned sensor data for GetAllSensorValues.
type mockSender struct {
	commandsSent []string
	sensorJSON   string // overrides default reply if non-empty
	disconnected bool   // when true, IsTargetConnected reports false (simulates BotLink offline)
	sensorErr    error  // when non-nil, SendCommand returns this error for GetAllSensorValues
}

func (m *mockSender) SendCommand(_ context.Context, _ string, text string, _ time.Duration) (string, error) {
	m.commandsSent = append(m.commandsSent, text)
	if text == "GetAllSensorValues" {
		if m.sensorErr != nil {
			return "", m.sensorErr
		}
		if m.sensorJSON != "" {
			return m.sensorJSON, nil
		}
		return `{"success":true,"data":[{"name":"VRM","value":45}]}`, nil
	}
	return `{"success":true}`, nil
}

func (m *mockSender) SendMessage(_ string, _ string) error { return nil }
func (m *mockSender) IsConnected() bool                    { return true }
func (m *mockSender) IsTargetConnected(_ string) bool       { return !m.disconnected }

func (m *mockSender) setFanSpeedCalls() int {
	n := 0
	for _, cmd := range m.commandsSent {
		if strings.HasPrefix(cmd, "SetFanSpeed") {
			n++
		}
	}
	return n
}

// ─── Tach validation ──────────────────────────────────────────────────────────

// TestTach_NoAlarmBeforeBaseline verifies that checkTachValidation produces no
// alarms when hasBaseline=false (service just started, no commands issued yet).
func TestTach_NoAlarmBeforeBaseline(t *testing.T) {
	svc := makeTachTestService(2)

	raw := map[string]float64{"fan1_tach": 1200}

	// Drive many ticks; without a baseline the inner window must never open.
	for i := 0; i < 10; i++ {
		alarms := svc.checkTachValidation(raw)
		if len(alarms) != 0 {
			t.Errorf("tick %d: expected no alarms before baseline, got %v", i, alarms)
		}
	}
	// failPolls must remain 0.
	if ts := svc.tachStates["fan1"]; ts.failPolls != 0 {
		t.Errorf("failPolls should be 0 before baseline, got %d", ts.failPolls)
	}
}

// TestTach_FirstCommandSkipsValidation verifies that after the first command
// (hasBaseline=true, pollsAfterNewCmd=polls) the validation window stays closed
// so no false alarm fires while we're still in "established baseline" state.
func TestTach_FirstCommandSkipsValidation(t *testing.T) {
	polls := 2
	svc := makeTachTestService(polls)

	// Simulate first command: hasBaseline=true, pollsAfterNewCmd=polls (window closed).
	ts := svc.tachStates["fan1"]
	ts.hasBaseline = true
	ts.lastCommandedPWM = 80
	ts.pollsAfterNewCmd = polls // >= polls → window check won't fire
	ts.prevTachValue = 1200

	// Tach is moving in the wrong direction — but validation window is closed.
	raw := map[string]float64{"fan1_tach": 500}
	for i := 0; i < 5; i++ {
		alarms := svc.checkTachValidation(raw)
		if len(alarms) != 0 {
			t.Errorf("tick %d: should not alarm between first and second command, got %v", i, alarms)
		}
	}
}

// TestTach_AlarmsAfterSecondCommand verifies that after a second command change
// the validation window opens and an alarm fires when the fan doesn't respond.
func TestTach_AlarmsAfterSecondCommand(t *testing.T) {
	polls := 2
	svc := makeTachTestService(polls)

	ts := svc.tachStates["fan1"]
	ts.hasBaseline = true
	ts.lastCommandedPWM = 80
	ts.commandedUp = true   // commanded to go up
	ts.pollsAfterNewCmd = 0 // second command resets the window
	ts.prevTachValue = 1200
	ts.failPolls = 0

	// Fan tach goes DOWN (wrong direction for commandedUp=true).
	for i := 0; i < polls; i++ {
		alarms := svc.checkTachValidation(map[string]float64{"fan1_tach": float64(1200 - 100*(i+1))})
		if i < polls-1 && len(alarms) != 0 {
			t.Errorf("tick %d: should not alarm before %d bad polls, got %v", i, polls, alarms)
		}
		if i == polls-1 && len(alarms) == 0 {
			t.Errorf("tick %d: should alarm after %d bad polls", i, polls)
		}
	}
}

// ─── Persistent sensor failure retries fail-safe ──────────────────────────────

// TestHandleSensorFailure_RetryWhenActive verifies that when failSafeActive is
// already true and consecutive missing ticks continue, handleSensorFailure
// keeps calling sendFailSafe (which retries commands and re-notifies rate-limited).
func TestHandleSensorFailure_RetryWhenActive(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.jsonl")

	svc := &Service{
		cfg: config.FanReflexConfig{Shadow: true},
		policy: &Policy{
			TickSeconds: 15,
			FailSafe: FailSafeDef{
				Commands: []FailSafeCommand{{Channel: "Fan1", PWM: 100}},
			},
			Actuators: map[string]ActuatorDef{
				"fan1": {Channel: "Fan1"},
			},
		},
		logPath:            logPath,
		failSafeActive:     true,
		failSafeReason:     "test",
		consecutiveMissing: 2, // next call pushes it to 3
	}
	defer svc.closeLog()

	svc.handleSensorFailure("test failure")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log not written after retry: %v", err)
	}
	if !strings.Contains(string(data), "FAIL_SAFE") {
		t.Errorf("expected FAIL_SAFE in log, got: %s", data)
	}
}

// TestHandleSensorFailure_TriggerOnThirdMissing verifies fail-safe is activated
// on the third consecutive missing tick (not before).
func TestHandleSensorFailure_TriggerOnThirdMissing(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.jsonl")

	svc := &Service{
		cfg: config.FanReflexConfig{Shadow: true},
		policy: &Policy{
			TickSeconds: 15,
			FailSafe: FailSafeDef{
				Commands: []FailSafeCommand{{Channel: "Fan1", PWM: 100}},
			},
			Actuators: map[string]ActuatorDef{"fan1": {Channel: "Fan1"}},
		},
		logPath: logPath,
	}
	defer svc.closeLog()

	svc.handleSensorFailure("miss 1") // consecutiveMissing=1, no failsafe yet
	if _, err := os.ReadFile(logPath); err == nil {
		t.Error("log should not exist after 1 missing tick")
	}
	svc.handleSensorFailure("miss 2") // consecutiveMissing=2, still no failsafe
	if _, err := os.ReadFile(logPath); err == nil {
		t.Error("log should not exist after 2 missing ticks")
	}
	svc.handleSensorFailure("miss 3") // consecutiveMissing=3 → trigger
	if !svc.failSafeActive {
		t.Error("failSafeActive should be true after 3 missing ticks")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log should exist after 3 missing ticks: %v", err)
	}
	if !strings.Contains(string(data), "FAIL_SAFE") {
		t.Errorf("expected FAIL_SAFE in log, got: %s", data)
	}
}

// ─── Decision log format ──────────────────────────────────────────────────────

// TestDecisionLog_FullTickResultFields verifies that the decision log for a
// curve actuator includes all filter-pipeline fields required for shadow verification.
func TestDecisionLog_FullTickResultFields(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.jsonl")

	am := boardBalancedActuator()
	fixed := 100
	policy := &Policy{
		Version:     1,
		TickSeconds: 15,
		Sensors:     map[string]string{"vrm": "VRM", "pump_tach": "Pump Tach"},
		Derived:     map[string]DerivedDef{},
		Actuators: map[string]ActuatorDef{
			"board": {Channel: "Board Fan"},
			"pump":  {Channel: "Pump Tach"},
		},
		Modes: map[string]map[string]ActuatorMode{
			"balanced": {
				"board": am,
				"pump":  {FixedPct: &fixed},
			},
		},
	}

	svc := &Service{
		cfg:          config.FanReflexConfig{Shadow: true},
		policy:       policy,
		logPath:      logPath,
		filterStates: map[string]*ActuatorFilterState{"board": {}, "pump": {}},
		tachStates:   map[string]*tachState{},
		lastSentPWM:  map[string]*int{},
	}
	defer svc.closeLog()

	// Build a minimal log entry by calling writeLog directly with a curve entry.
	r := TickActuator(svc.filterStates["board"], am,
		map[string]float64{"vrm": 55}, 0, 15)
	entry := tickLog{
		TS:   "2026-07-13T00:00:00Z",
		Mode: "balanced",
		Raw:  map[string]float64{"vrm": 55},
		Actuators: map[string]any{
			"board": &logActuatorCurve{
				AvgInput:        r.AvgInput,
				PrimaryCurveOut: r.PrimaryCurveOut,
				HystCurveOut:    r.HystCurveOut,
				DwellOut:        r.DwellOut,
				BackstopPWM:     r.BackstopPWM,
				BackstopFired:   r.BackstopFired,
				FinalPWM:        r.FinalPWM,
				WouldSend:       true,
			},
			"pump": &logActuatorFixed{FixedPct: 100, WouldSend: true},
		},
	}
	svc.writeLog(entry)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log not written: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &raw); err != nil {
		t.Fatalf("log not valid JSON: %v\nlog: %s", err, data)
	}

	actuators, ok := raw["actuators"].(map[string]any)
	if !ok {
		t.Fatalf("actuators not a map: %v", raw["actuators"])
	}

	board, ok := actuators["board"].(map[string]any)
	if !ok {
		t.Fatalf("board actuator entry missing or not a map")
	}

	for _, field := range []string{"avg_input", "primary_curve_out", "hyst_curve_out", "dwell_out", "backstop_pwm", "backstop_fired", "final_pwm"} {
		if _, ok := board[field]; !ok {
			t.Errorf("decision log missing field %q in board actuator entry", field)
		}
	}

	pump, ok := actuators["pump"].(map[string]any)
	if !ok {
		t.Fatalf("pump actuator entry missing or not a map")
	}
	if _, ok := pump["fixed_pct"]; !ok {
		t.Errorf("decision log missing 'fixed_pct' in pump actuator entry")
	}
}

// ─── readMode dynamic validation ─────────────────────────────────────────────

// TestReadMode_CustomModeAccepted verifies that a mode name present in
// policy.Modes but absent from the old hardcoded list (e.g. "night") is
// accepted by readMode() and returned as-is.
func TestReadMode_CustomModeAccepted(t *testing.T) {
	dir := t.TempDir()
	modeFile := filepath.Join(dir, "fan-mode.txt")
	if err := os.WriteFile(modeFile, []byte("night\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := &Service{
		cfg: config.FanReflexConfig{ModePath: modeFile},
		policy: &Policy{
			Modes: map[string]map[string]ActuatorMode{
				"balanced": {},
				"night":    {},
			},
		},
	}

	got := svc.readMode()
	if got != "night" {
		t.Errorf("expected \"night\" (present in policy.Modes), got %q", got)
	}
}

// TestReadMode_UnknownModeFallsBackToBalanced verifies that a mode name not
// present in policy.Modes (regardless of casing) falls back to "balanced".
func TestReadMode_UnknownModeFallsBackToBalanced(t *testing.T) {
	dir := t.TempDir()
	modeFile := filepath.Join(dir, "fan-mode.txt")

	svc := &Service{
		cfg: config.FanReflexConfig{ModePath: modeFile},
		policy: &Policy{
			Modes: map[string]map[string]ActuatorMode{
				"balanced": {},
				"night":    {},
			},
		},
	}

	for _, content := range []string{"turbo", "PERFORMANCE", "xxx", ""} {
		if err := os.WriteFile(modeFile, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got := svc.readMode()
		if got != "balanced" {
			t.Errorf("content %q not in policy.Modes: expected fallback \"balanced\", got %q", content, got)
		}
	}
}

// ─── helper ───────────────────────────────────────────────────────────────────

// makeTachTestService builds a minimal Service with one tach-validated actuator
// "fan1" whose tach sensor logical name is "fan1_tach" (channel = sensor full name).
func makeTachTestService(polls int) *Service {
	return &Service{
		cfg: config.FanReflexConfig{},
		policy: &Policy{
			TickSeconds: 15,
			Actuators: map[string]ActuatorDef{
				"fan1": {Channel: "Fan1 Channel", TachValidated: true},
			},
			Sensors: map[string]string{
				"fan1_tach": "Fan1 Channel", // matches actuator channel → tachSensorName returns "fan1_tach"
			},
			Faults: FaultsDef{UnresponsiveFan: &UnresponsiveFanFault{Polls: polls}},
		},
		tachStates: map[string]*tachState{
			"fan1": {},
		},
	}
}

// ─── Periodic re-assert ───────────────────────────────────────────────────────

// makeReassertService returns a Service with a single fixed-speed "pump" actuator
// and a mock MQTT sender suitable for re-assert integration tests.
func makeReassertService(t *testing.T) (*Service, *mockSender) {
	t.Helper()
	dir := t.TempDir()
	modeFile := filepath.Join(dir, "fan-mode.txt")
	if err := os.WriteFile(modeFile, []byte("balanced"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockSender{
		sensorJSON: `{"success":true,"data":[{"name":"VRM","value":45}]}`,
	}
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
	}

	svc := &Service{
		cfg:          config.FanReflexConfig{ModePath: modeFile, TargetBotID: "bot1"},
		policy:       policy,
		logPath:      filepath.Join(dir, "test.jsonl"),
		client:       mock,
		filterStates: map[string]*ActuatorFilterState{"pump": {}},
		tachStates:   map[string]*tachState{},
		lastSentPWM:  map[string]*int{},
		startedAt:    time.Now(),
	}
	t.Cleanup(func() { svc.closeLog() })
	return svc, mock
}

// TestReassert_FixedSendsOnInterval verifies that a fixed-speed actuator whose
// value has not changed is still re-sent on tick 20 (and every 20th tick after),
// but suppressed on ticks 2–19.
func TestReassert_FixedSendsOnInterval(t *testing.T) {
	svc, mock := makeReassertService(t)

	// Tick 1: initial send (lastSentPWM == nil).
	svc.runTick()
	if mock.setFanSpeedCalls() != 1 {
		t.Fatalf("tick 1: want 1 SetFanSpeed (initial), got %d", mock.setFanSpeedCalls())
	}

	// Ticks 2–19: value unchanged, not a re-assert tick → suppressed.
	for i := 2; i <= 19; i++ {
		svc.runTick()
	}
	if mock.setFanSpeedCalls() != 1 {
		t.Errorf("ticks 2-19: want still 1 SetFanSpeed (suppressed), got %d", mock.setFanSpeedCalls())
	}

	// Tick 20: re-assert tick → must send even though value is unchanged.
	svc.runTick()
	if mock.setFanSpeedCalls() != 2 {
		t.Errorf("tick 20: want 2 SetFanSpeed (re-assert), got %d", mock.setFanSpeedCalls())
	}

	// Ticks 21–39: suppressed again.
	for i := 21; i <= 39; i++ {
		svc.runTick()
	}
	if mock.setFanSpeedCalls() != 2 {
		t.Errorf("ticks 21-39: want still 2 SetFanSpeed, got %d", mock.setFanSpeedCalls())
	}

	// Tick 40: second re-assert.
	svc.runTick()
	if mock.setFanSpeedCalls() != 3 {
		t.Errorf("tick 40: want 3 SetFanSpeed (second re-assert), got %d", mock.setFanSpeedCalls())
	}
}

// TestReassert_LogMarkedReassert verifies that the decision log for a re-assert
// tick has "reassert":true on the actuator entry, while normal ticks do not.
func TestReassert_LogMarkedReassert(t *testing.T) {
	svc, _ := makeReassertService(t)

	// Run 20 ticks so tick 20 is the re-assert tick.
	for i := 0; i < 20; i++ {
		svc.runTick()
	}

	data, err := os.ReadFile(svc.logPath)
	if err != nil {
		t.Fatalf("log not written: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 20 {
		t.Fatalf("expected 20 log lines, got %d", len(lines))
	}

	// Parse the 20th line (index 19 = tick 20).
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[19]), &row); err != nil {
		t.Fatalf("tick-20 log line not valid JSON: %v\nline: %s", err, lines[19])
	}
	acts, _ := row["actuators"].(map[string]any)
	pump, _ := acts["pump"].(map[string]any)
	if pump["reassert"] != true {
		t.Errorf("tick 20 log: expected pump.reassert=true, got %v", pump)
	}

	// Also confirm tick 1 (index 0) does NOT have reassert.
	var row1 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row1); err != nil {
		t.Fatalf("tick-1 log line not valid JSON: %v", err)
	}
	acts1, _ := row1["actuators"].(map[string]any)
	pump1, _ := acts1["pump"].(map[string]any)
	if pump1["reassert"] == true {
		t.Errorf("tick 1 log: pump.reassert should not be true on initial send")
	}
}

// ─── Send order ───────────────────────────────────────────────────────────────

// TestSendOrder_PumpFirstRadiatorLast verifies that the command send loop emits
// SetFanSpeed for pump before board and radiator, and radiator last, regardless
// of Go map iteration order.
func TestSendOrder_PumpFirstRadiatorLast(t *testing.T) {
	dir := t.TempDir()
	modeFile := filepath.Join(dir, "fan-mode.txt")
	if err := os.WriteFile(modeFile, []byte("balanced"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockSender{
		sensorJSON: `{"success":true,"data":[{"name":"VRM","value":45}]}`,
	}
	fixed := 100
	policy := &Policy{
		Version:     1,
		TickSeconds: 15,
		Sensors:     map[string]string{"vrm": "VRM"},
		Derived:     map[string]DerivedDef{},
		Actuators: map[string]ActuatorDef{
			"pump":     {Channel: "Pump Channel"},
			"board":    {Channel: "Board Channel"},
			"radiator": {Channel: "Radiator Channel"},
		},
		Modes: map[string]map[string]ActuatorMode{
			"balanced": {
				"pump":     {FixedPct: &fixed},
				"board":    {FixedPct: &fixed},
				"radiator": {FixedPct: &fixed},
			},
		},
	}

	svc := &Service{
		cfg:     config.FanReflexConfig{ModePath: modeFile, TargetBotID: "bot1"},
		policy:  policy,
		logPath: filepath.Join(dir, "test.jsonl"),
		client:  mock,
		filterStates: map[string]*ActuatorFilterState{
			"pump": {}, "board": {}, "radiator": {},
		},
		tachStates:  map[string]*tachState{},
		lastSentPWM: map[string]*int{},
		startedAt:   time.Now(),
	}
	t.Cleanup(func() { svc.closeLog() })

	svc.runTick()

	// Collect SetFanSpeed commands in the order they were issued.
	var fanCmds []string
	for _, cmd := range mock.commandsSent {
		if strings.HasPrefix(cmd, "SetFanSpeed") {
			fanCmds = append(fanCmds, cmd)
		}
	}
	if len(fanCmds) != 3 {
		t.Fatalf("expected 3 SetFanSpeed commands, got %d: %v", len(fanCmds), fanCmds)
	}

	pumpIdx, radiatorIdx := -1, -1
	for i, cmd := range fanCmds {
		if strings.Contains(cmd, "Pump Channel") {
			pumpIdx = i
		}
		if strings.Contains(cmd, "Radiator Channel") {
			radiatorIdx = i
		}
	}
	if pumpIdx != 0 {
		t.Errorf("pump must be first (index 0), got index %d; order: %v", pumpIdx, fanCmds)
	}
	if radiatorIdx != 2 {
		t.Errorf("radiator must be last (index 2), got index %d; order: %v", radiatorIdx, fanCmds)
	}
}

// TestReassert_NoTachWindow verifies that calling updateTachState with the same
// PWM as lastCommandedPWM (the re-assert scenario) is a no-op — the validation
// window is not reopened and pollsAfterNewCmd is unchanged.
func TestReassert_NoTachWindow(t *testing.T) {
	polls := 2
	svc := makeTachTestService(polls)
	ts := svc.tachStates["fan1"]

	// Simulate post-first-command state: baseline established, window closed.
	ts.hasBaseline = true
	ts.lastCommandedPWM = 80
	ts.pollsAfterNewCmd = polls // window closed (>= polls)
	ts.prevTachValue = 1200

	// A re-assert uses the same PWM: updateTachState must not alter the window.
	svc.updateTachState("fan1", 80, map[string]float64{"fan1_tach": 900})

	if ts.pollsAfterNewCmd != polls {
		t.Errorf("re-assert: pollsAfterNewCmd should stay %d, got %d", polls, ts.pollsAfterNewCmd)
	}
	if ts.lastCommandedPWM != 80 {
		t.Errorf("re-assert: lastCommandedPWM should stay 80, got %d", ts.lastCommandedPWM)
	}
	if ts.prevTachValue != 1200 {
		t.Errorf("re-assert: prevTachValue should stay 1200, got %v", ts.prevTachValue)
	}
}

