package fanreflex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/constants"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/state"
	alohaclawtools "github.com/sipeed/picoclaw/pkg/tools/alohaclaw"
)

const (
	logMaxBytes        = 10 * 1024 * 1024 // 10 MB
	notifyInterval     = 10 * time.Minute
	reassertEveryTicks = 20 // re-send all actuators every N ticks even if value unchanged
)

// sensorDatum is one reading from GetAllSensorValues.
type sensorDatum struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// botReply is the JSON structure returned by CTFanBot commands.
type botReply struct {
	Success bool          `json:"success"`
	Result  string        `json:"result"`
	Data    []sensorDatum `json:"data"`
}

// tickLog is one JSONL decision-log line written every tick.
type tickLog struct {
	TS        string             `json:"ts"`
	Mode      string             `json:"mode"`
	Raw       map[string]float64 `json:"raw"`
	Derived   map[string]float64 `json:"derived,omitempty"`
	Actuators map[string]any     `json:"actuators"`
	Alarms    []string           `json:"alarms,omitempty"`
	Fault     *string            `json:"fault,omitempty"`
}

// logActuatorFixed is the decision-log entry for a fixed-speed actuator (e.g. pump).
type logActuatorFixed struct {
	FixedPct  int  `json:"fixed_pct"`
	Sent      bool `json:"sent,omitempty"`
	WouldSend bool `json:"would_send,omitempty"`
	Reassert  bool `json:"reassert,omitempty"`
}

// logActuatorCurve is the decision-log entry for a curve-based actuator.
// All filter-pipeline fields are included to enable per-tick shadow-mode verification.
type logActuatorCurve struct {
	AvgInput        float64 `json:"avg_input"`
	PrimaryCurveOut int     `json:"primary_curve_out"`
	HystCurveOut    int     `json:"hyst_curve_out"`
	DwellOut        int     `json:"dwell_out"`
	BackstopPWM     int     `json:"backstop_pwm"`
	BackstopFired   bool    `json:"backstop_fired"`
	FinalPWM        int     `json:"final_pwm"`
	Sent            bool    `json:"sent,omitempty"`
	WouldSend       bool    `json:"would_send,omitempty"`
	Reassert        bool    `json:"reassert,omitempty"`
}

// tachState tracks unresponsive-fan detection for one tach-validated actuator.
type tachState struct {
	hasBaseline      bool    // false until first command is issued
	lastCommandedPWM int
	pollsAfterNewCmd int     // polls since last direction change
	prevTachValue    float64
	commandedUp      bool
	failPolls        int  // consecutive polls that didn't move in commanded direction
	suspect          bool
}

// rateLimiter throttles notifications of the same alarm type.
type rateLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (r *rateLimiter) canNotify(alarm string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = make(map[string]time.Time)
	}
	if t, ok := r.last[alarm]; ok && time.Since(t) < notifyInterval {
		return false
	}
	r.last[alarm] = time.Now()
	return true
}

// Service is the deterministic fan-reflex control service.
type Service struct {
	cfg      config.FanReflexConfig
	policy   *Policy
	workspace string
	msgBus   *bus.MessageBus
	stateMan *state.Manager
	client   alohaclawtools.Sender

	filterStates map[string]*ActuatorFilterState
	tachStates   map[string]*tachState
	lastSentPWM  map[string]*int // actuator name → last sent PWM (nil = never)

	// fault tracking
	consecutiveMissing int
	consecutiveOK      int
	failSafeActive     bool
	failSafeReason     string
	flowLowSecs        float64

	rateLimit rateLimiter

	// decision log
	logPath string
	logFile *os.File
	logSize int64

	// control
	stopChan  chan struct{}
	wg        sync.WaitGroup
	startedAt time.Time
	tickCount int // increments each runTick; drives periodic re-assert
	mu        sync.Mutex

	// snapshot fields – updated by runLoop goroutine, read by StatusSnapshot
	snapMu     sync.Mutex
	snapAt     time.Time
	snapMode   string
	snapFail   bool
	snapReason string
}

// NewService creates a new Service.  It loads and validates the policy and
// establishes (or joins) the shared MQTT connection.  Returns an error if the
// policy is invalid or the MQTT connection cannot be established.
func NewService(
	cfg config.FanReflexConfig,
	alohaCfg config.AlohaClawConfig,
	msgBus *bus.MessageBus,
	workspace string,
) (*Service, error) {
	policyPath := cfg.PolicyPath
	if !filepath.IsAbs(policyPath) {
		policyPath = filepath.Join(workspace, policyPath)
	}
	policy, err := LoadAndValidatePolicy(policyPath)
	if err != nil {
		return nil, fmt.Errorf("fanreflex: policy invalid: %w", err)
	}

	port := alohaCfg.Port
	if port <= 0 {
		port = 8883
	}
	client, err := alohaclawtools.GetOrCreateClient(
		alohaCfg.BrokerIP, port, alohaCfg.BotID, alohaCfg.BotPassword,
	)
	if err != nil {
		return nil, fmt.Errorf("fanreflex: MQTT connect: %w", err)
	}

	logPath := cfg.DecisionLogPath
	if !filepath.IsAbs(logPath) {
		logPath = filepath.Join(workspace, logPath)
	}

	svc := &Service{
		cfg:          cfg,
		policy:       policy,
		workspace:    workspace,
		msgBus:       msgBus,
		stateMan:     state.NewManager(workspace),
		client:       client,
		filterStates: make(map[string]*ActuatorFilterState),
		tachStates:   make(map[string]*tachState),
		lastSentPWM:  make(map[string]*int),
		logPath:      logPath,
	}

	for name, adef := range policy.Actuators {
		svc.filterStates[name] = &ActuatorFilterState{}
		if adef.TachValidated {
			svc.tachStates[name] = &tachState{}
		}
	}

	return svc, nil
}

// Start launches the ticker loop.
func (s *Service) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopChan != nil {
		return nil
	}
	s.startedAt = time.Now()
	s.stopChan = make(chan struct{})
	s.wg.Add(1)
	go s.runLoop(s.stopChan)

	shadow := ""
	if s.cfg.Shadow {
		shadow = " (shadow mode)"
	}
	logger.InfoCF("fanreflex", "Service started"+shadow, map[string]any{
		"target_bot": s.cfg.TargetBotID,
		"tick_s":     s.policy.TickSeconds,
	})
	return nil
}

// Stop shuts down the ticker loop and waits for any in-flight tick to finish
// before closing the decision log.
func (s *Service) Stop() {
	s.mu.Lock()
	ch := s.stopChan
	s.stopChan = nil
	s.mu.Unlock()

	if ch != nil {
		close(ch)
	}
	s.wg.Wait() // ensure runLoop has exited before touching the log file
	s.closeLog()
}

// StatusSnapshot returns a read-safe snapshot of current service state for the mgmt API.
func (s *Service) StatusSnapshot() map[string]any {
	s.snapMu.Lock()
	at := s.snapAt
	mode := s.snapMode
	fail := s.snapFail
	reason := s.snapReason
	s.snapMu.Unlock()

	result := map[string]any{
		"enabled":          true,
		"shadow":           s.cfg.Shadow,
		"target_bot_id":    s.cfg.TargetBotID,
		"mode":             mode,
		"fail_safe_active": fail,
	}
	if at.IsZero() {
		result["last_tick_at"] = nil
	} else {
		result["last_tick_at"] = at.UTC().Format(time.RFC3339)
	}
	if fail && reason != "" {
		result["fail_safe_reason"] = reason
	}
	return result
}

func (s *Service) runLoop(stop chan struct{}) {
	defer s.wg.Done()
	tickDur := time.Duration(s.policy.TickSeconds) * time.Second
	ticker := time.NewTicker(tickDur)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.runTick()
		}
	}
}

// nowSecs returns elapsed seconds since service start (for the filter engine).
func (s *Service) nowSecs() float64 {
	return time.Since(s.startedAt).Seconds()
}

func (s *Service) runTick() {
	tickStart := time.Now()
	s.snapMu.Lock()
	s.snapAt = tickStart
	s.snapMu.Unlock()

	s.tickCount++
	isReassertTick := s.tickCount%reassertEveryTicks == 0

	mode := s.readMode()
	modeMap, ok := s.policy.Modes[mode]
	if !ok {
		modeMap = s.policy.Modes["balanced"]
		mode = "balanced"
	}
	s.snapMu.Lock()
	s.snapMode = mode
	s.snapMu.Unlock()

	// Read sensors.
	rawSensors, readErr := s.readAllSensors()
	if readErr != nil {
		logger.WarnCF("fanreflex", "sensor read failed", map[string]any{"error": readErr.Error()})
		s.handleSensorFailure(readErr.Error())
		return
	}

	// Check required sensors.
	for _, rs := range s.policy.RequiredSensors {
		if _, ok := rawSensors[rs]; !ok {
			msg := fmt.Sprintf("required sensor %q missing from reply", rs)
			logger.WarnCF("fanreflex", "required sensor missing", map[string]any{"sensor": rs})
			s.handleSensorFailure(msg)
			return
		}
	}

	// Sensors OK: reset consecutive-missing counter, advance OK counter.
	s.consecutiveMissing = 0
	if s.failSafeActive {
		s.consecutiveOK++
		if s.consecutiveOK >= 3 {
			s.failSafeActive = false
			s.failSafeReason = ""
			s.consecutiveOK = 0
			s.snapMu.Lock()
			s.snapFail = false
			s.snapReason = ""
			s.snapMu.Unlock()
			s.notify("fanreflex_recovery", "[FanReflex] 感應器恢復正常，回到正常控制迴圈")
		}
		// Stay in fail-safe commands until 3 consecutive OK ticks.
		s.sendFailSafe(mode, rawSensors)
		return
	}
	s.consecutiveOK = 0

	// Compute derived values.
	allValues := ComputeDerived(s.policy.Derived, rawSensors)

	// Derived-only entries for the log (exclude raw keys).
	derivedOnly := make(map[string]float64, len(allValues)-len(rawSensors))
	for k, v := range allValues {
		if _, isRaw := rawSensors[k]; !isRaw {
			derivedOnly[k] = v
		}
	}

	now := s.nowSecs()
	tickSecs := float64(s.policy.TickSeconds)

	// Pass 1: compute all actuator outputs.
	results := make(map[string]TickResult)
	fixedTargets := make(map[string]int)

	for actuatorName, am := range modeMap {
		if am.IsFixed() {
			fixedTargets[actuatorName] = *am.FixedPct
		} else {
			fs := s.filterStates[actuatorName]
			results[actuatorName] = TickActuator(fs, am, allValues, now, tickSecs)
		}
	}

	// Fault checks (before sending commands).
	fault, faultReason := s.checkFaults(rawSensors, fixedTargets, results)
	if fault {
		s.triggerFailSafe(faultReason, mode, rawSensors)
		return
	}

	// Build log entry.
	entry := tickLog{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Mode:      mode,
		Raw:       rawSensors,
		Derived:   derivedOnly,
		Actuators: make(map[string]any),
	}

	// Tach validation is only meaningful when commands are actually issued.
	// Skip entirely in shadow mode to prevent false alarms.
	if !s.cfg.Shadow {
		entry.Alarms = s.checkTachValidation(rawSensors)
	}

	// Pass 2: send / record commands and populate actuator log entries.
	// Order is fixed (pump → board → radiator → others) so radiator is always the
	// last writer on CTFanBot's bulk-write controller.
	for _, actuatorName := range actuatorSendOrder(s.policy.Actuators) {
		adef := s.policy.Actuators[actuatorName]
		am := modeMap[actuatorName]

		if am.IsFixed() {
			targetPWM := fixedTargets[actuatorName]
			la := &logActuatorFixed{FixedPct: targetPWM}

			last := s.lastSentPWM[actuatorName]
			valueChanged := last == nil || *last != targetPWM
			isReassert := isReassertTick && last != nil && *last == targetPWM
			if valueChanged || isReassert {
				la.Reassert = isReassert
				if s.cfg.Shadow {
					la.WouldSend = true
					pwm := targetPWM
					s.lastSentPWM[actuatorName] = &pwm
				} else {
					if err := s.sendFanSpeed(adef.Channel, targetPWM); err != nil {
						logger.WarnCF("fanreflex", "SetFanSpeed failed",
							map[string]any{"channel": adef.Channel, "pwm": targetPWM, "error": err.Error()})
					} else {
						la.Sent = true
						pwm := targetPWM
						s.lastSentPWM[actuatorName] = &pwm
						if !isReassert {
							s.updateTachState(actuatorName, targetPWM, rawSensors)
						}
					}
				}
			}
			entry.Actuators[actuatorName] = la

		} else {
			r := results[actuatorName]
			la := &logActuatorCurve{
				AvgInput:        r.AvgInput,
				PrimaryCurveOut: r.PrimaryCurveOut,
				HystCurveOut:    r.HystCurveOut,
				DwellOut:        r.DwellOut,
				BackstopPWM:     r.BackstopPWM,
				BackstopFired:   r.BackstopFired,
				FinalPWM:        r.FinalPWM,
			}

			last := s.lastSentPWM[actuatorName]
			isReassert := isReassertTick && last != nil && !r.ShouldSend
			if r.ShouldSend || isReassert {
				la.Reassert = isReassert
				if s.cfg.Shadow {
					la.WouldSend = true
					pwm := r.FinalPWM
					s.lastSentPWM[actuatorName] = &pwm
					s.filterStates[actuatorName].LastSentPWM = &pwm
				} else {
					if err := s.sendFanSpeed(adef.Channel, r.FinalPWM); err != nil {
						logger.WarnCF("fanreflex", "SetFanSpeed failed",
							map[string]any{"channel": adef.Channel, "pwm": r.FinalPWM, "error": err.Error()})
					} else {
						la.Sent = true
						pwm := r.FinalPWM
						s.lastSentPWM[actuatorName] = &pwm
						s.filterStates[actuatorName].LastSentPWM = &pwm
						if !isReassert {
							s.updateTachState(actuatorName, r.FinalPWM, rawSensors)
						}
					}
				}
			}
			entry.Actuators[actuatorName] = la
		}
	}

	s.writeLog(entry)
}

// updateTachState updates per-actuator tach tracking after a command is issued.
//
// First command: establishes baseline (records prevTach and lastCommandedPWM) without
// starting direction validation — at service start we don't know the fan's current PWM.
//
// Subsequent command changes: start the direction-validation window.
func (s *Service) updateTachState(actuatorName string, targetPWM int, raw map[string]float64) {
	ts, ok := s.tachStates[actuatorName]
	if !ok || ts.lastCommandedPWM == targetPWM {
		return
	}
	prevTach := raw[s.tachSensorName(actuatorName)]
	if !ts.hasBaseline {
		ts.hasBaseline = true
		ts.lastCommandedPWM = targetPWM
		ts.prevTachValue = prevTach
		// Set pollsAfterNewCmd >= polls so the validation window doesn't open
		// until the next command change (the second distinct command).
		polls := 0
		if s.policy.Faults.UnresponsiveFan != nil {
			polls = s.policy.Faults.UnresponsiveFan.Polls
		}
		ts.pollsAfterNewCmd = polls
	} else {
		ts.commandedUp = targetPWM > ts.lastCommandedPWM
		ts.lastCommandedPWM = targetPWM
		ts.pollsAfterNewCmd = 0
		ts.prevTachValue = prevTach
		ts.failPolls = 0
		ts.suspect = false
	}
}

// handleSensorFailure increments the consecutive-missing counter and triggers
// fail-safe after 3 ticks.  When fail-safe is already active it retries the
// fail-safe commands and re-notifies (rate-limited inside sendFailSafe).
func (s *Service) handleSensorFailure(reason string) {
	s.consecutiveMissing++
	if s.consecutiveMissing >= 3 {
		if !s.failSafeActive {
			s.triggerFailSafe("telemetry failure: "+reason, "", nil)
		} else {
			s.sendFailSafe("", nil) // retry commands + periodic rate-limited notify
		}
	}
}

// checkFaults returns true if any immediate fault condition is detected.
func (s *Service) checkFaults(
	raw map[string]float64,
	fixedTargets map[string]int,
	results map[string]TickResult,
) (bool, string) {
	faults := s.policy.Faults
	tickSecs := float64(s.policy.TickSeconds)

	// pump_zero_tach: immediate fail-safe.
	if faults.PumpZeroTach != nil {
		pzt := faults.PumpZeroTach
		tach, hasTach := raw[pzt.Sensor]
		pumpTarget := fixedTargets["pump"]
		if hasTach && int(tach) == pzt.Eq && pumpTarget > pzt.WhileTargetGt {
			return true, fmt.Sprintf("pump_zero_tach: %s=0 while pump target=%d", pzt.Sensor, pumpTarget)
		}
	}

	// flow_low: sustained fault.
	if faults.FlowLow != nil {
		fl := faults.FlowLow
		flowVal, hasFlow := raw[fl.Sensor]
		pumpTarget := fixedTargets["pump"]
		if hasFlow && flowVal <= fl.Lte && pumpTarget >= fl.WhilePumpPct {
			s.flowLowSecs += tickSecs
			if int(s.flowLowSecs) >= fl.SustainS {
				return true, fmt.Sprintf("flow_low: %s=%.2f <= %.2f for %.0fs",
					fl.Sensor, flowVal, fl.Lte, s.flowLowSecs)
			}
		} else {
			s.flowLowSecs = 0
		}
	}

	return false, ""
}

// checkTachValidation verifies that tach-validated actuators responded to
// recent commands. Returns a list of alarm strings.
// Must not be called in shadow mode (no commands are issued in shadow mode).
func (s *Service) checkTachValidation(raw map[string]float64) []string {
	if s.policy.Faults.UnresponsiveFan == nil {
		return nil
	}
	polls := s.policy.Faults.UnresponsiveFan.Polls

	var alarms []string
	for actuatorName, ts := range s.tachStates {
		if !ts.hasBaseline {
			// No command has been issued yet; skip until baseline is established.
			continue
		}
		sensorName := s.tachSensorName(actuatorName)
		curTach, ok := raw[sensorName]
		if !ok {
			continue
		}

		if ts.pollsAfterNewCmd < polls {
			ts.pollsAfterNewCmd++
			moved := (ts.commandedUp && curTach > ts.prevTachValue) ||
				(!ts.commandedUp && curTach < ts.prevTachValue)
			if !moved {
				ts.failPolls++
			} else {
				ts.failPolls = 0
			}
			ts.prevTachValue = curTach
		}

		if ts.failPolls >= polls && !ts.suspect {
			ts.suspect = true
			alarm := fmt.Sprintf("unresponsive_fan: %s tach did not respond to command", actuatorName)
			alarms = append(alarms, alarm)
			s.notifyAlarm("unresponsive_fan_"+actuatorName, "[FanReflex] "+alarm)
		}
	}
	return alarms
}

// tachSensorName returns the logical sensor name whose full name matches the
// actuator's channel, used to look up tach readings in raw sensor maps.
func (s *Service) tachSensorName(actuatorName string) string {
	adef := s.policy.Actuators[actuatorName]
	for logicalName, fullName := range s.policy.Sensors {
		if fullName == adef.Channel {
			return logicalName
		}
	}
	return ""
}

// triggerFailSafe activates the fail-safe branch: sends 100% commands, sends
// StopLoad if configured, and notifies the user.
func (s *Service) triggerFailSafe(reason, mode string, raw map[string]float64) {
	if !s.failSafeActive {
		s.failSafeActive = true
		s.failSafeReason = reason
		s.consecutiveOK = 0
		s.snapMu.Lock()
		s.snapFail = true
		s.snapReason = reason
		s.snapMu.Unlock()
		logger.WarnCF("fanreflex", "fail-safe triggered", map[string]any{"reason": reason})
	}

	s.sendFailSafe(mode, raw)
}

// sendFailSafe sends the policy's failsafe commands (or records them in shadow mode).
func (s *Service) sendFailSafe(mode string, raw map[string]float64) {
	fault := "FAIL_SAFE: " + s.failSafeReason
	entry := tickLog{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Mode:      mode,
		Raw:       raw,
		Actuators: make(map[string]any),
		Fault:     &fault,
	}

	for _, cmd := range s.policy.FailSafe.Commands {
		la := &logActuatorFixed{FixedPct: cmd.PWM}
		if s.cfg.Shadow {
			la.WouldSend = true
		} else {
			if err := s.sendFanSpeed(cmd.Channel, cmd.PWM); err != nil {
				logger.WarnCF("fanreflex", "fail-safe SetFanSpeed failed",
					map[string]any{"channel": cmd.Channel, "error": err.Error()})
			} else {
				la.Sent = true
			}
		}
		// Use logical name as key when available for consistency with normal ticks.
		logicalName := cmd.Channel
		for name, adef := range s.policy.Actuators {
			if adef.Channel == cmd.Channel {
				logicalName = name
				break
			}
		}
		entry.Actuators[logicalName] = la
	}

	if s.policy.FailSafe.StopLoad && !s.cfg.Shadow {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := s.client.SendCommand(ctx, s.cfg.TargetBotID, "StopLoad", 10*time.Second); err != nil {
			logger.WarnCF("fanreflex", "StopLoad failed", map[string]any{"error": err.Error()})
		}
	}

	s.writeLog(entry)

	if s.rateLimit.canNotify("fail_safe") {
		msg := fmt.Sprintf("[FanReflex] FAIL-SAFE 觸發：%s\n%s",
			s.failSafeReason, s.policy.FailSafe.ShutdownRecommendation)
		s.notify("fanreflex_failsafe", msg)
	}
}

// readMode reads the current mode from mode_path, defaulting to "balanced".
func (s *Service) readMode() string {
	modePath := s.cfg.ModePath
	if !filepath.IsAbs(modePath) {
		modePath = filepath.Join(s.workspace, modePath)
	}
	data, err := os.ReadFile(modePath)
	if err != nil {
		return "balanced"
	}
	m := strings.ToLower(strings.TrimSpace(string(data)))
	if _, ok := s.policy.Modes[m]; ok {
		return m
	}
	logger.WarnCF("fanreflex", "invalid mode, using balanced", map[string]any{"read": m})
	return "balanced"
}

// readAllSensors calls GetAllSensorValues and returns a map of logical sensor
// name → float64 value.
func (s *Service) readAllSensors() (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.policy.TickSeconds)*time.Second)
	defer cancel()

	replyText, err := s.client.SendCommand(ctx, s.cfg.TargetBotID, "GetAllSensorValues",
		time.Duration(s.policy.TickSeconds)*time.Second)
	if err != nil {
		return nil, fmt.Errorf("GetAllSensorValues: %w", err)
	}

	var reply botReply
	if err := json.Unmarshal([]byte(replyText), &reply); err != nil {
		return nil, fmt.Errorf("parse sensor reply: %w", err)
	}
	if !reply.Success {
		return nil, fmt.Errorf("GetAllSensorValues failed: %s", reply.Result)
	}

	// Build full-name → value index.
	byFullName := make(map[string]float64, len(reply.Data))
	for _, d := range reply.Data {
		byFullName[d.Name] = d.Value
	}

	// Map to logical names.
	result := make(map[string]float64, len(s.policy.Sensors))
	for logicalName, fullName := range s.policy.Sensors {
		if v, ok := byFullName[fullName]; ok {
			result[logicalName] = v
		}
	}
	return result, nil
}

// actuatorSendOrder returns actuator names in a fixed send order:
// pump → board → radiator → any others (sorted).
// Radiator is last so it is the final writer on CTFanBot's bulk-write controller,
// preventing stale Fan1 cache from overwriting a just-sent radiator value.
func actuatorSendOrder(actuators map[string]ActuatorDef) []string {
	preferred := []string{"pump", "board", "radiator"}
	out := make([]string, 0, len(actuators))
	seen := make(map[string]bool, len(preferred))
	for _, name := range preferred {
		if _, ok := actuators[name]; ok {
			out = append(out, name)
			seen[name] = true
		}
	}
	rest := make([]string, 0, len(actuators)-len(out))
	for name := range actuators {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// sendFanSpeed sends a SetFanSpeed command to the target bot.
func (s *Service) sendFanSpeed(channel string, pwm int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := fmt.Sprintf("SetFanSpeed %s %d", channel, pwm)
	_, err := s.client.SendCommand(ctx, s.cfg.TargetBotID, cmd, 10*time.Second)
	return err
}

// notify sends a message to the user's last active channel via the message bus.
func (s *Service) notify(alarmType, message string) {
	if s.msgBus == nil {
		return
	}
	lastChannel := s.stateMan.GetLastChannel()
	if lastChannel == "" {
		return
	}
	parts := strings.SplitN(lastChannel, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return
	}
	platform, userID := parts[0], parts[1]
	if constants.IsInternalChannel(platform) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.msgBus.PublishOutbound(ctx, bus.OutboundMessage{
		Context: bus.NewOutboundContext(platform, userID, ""),
		Content: message,
	})
}

// notifyAlarm notifies only when the rate limit allows.
func (s *Service) notifyAlarm(alarmType, message string) {
	if s.rateLimit.canNotify(alarmType) {
		s.notify(alarmType, message)
	}
}

// writeLog appends one JSON line to the decision log, rotating at 10 MB.
func (s *Service) writeLog(entry tickLog) {
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')

	if err := s.ensureLogFile(); err != nil {
		return
	}

	if s.logSize+int64(len(line)) > logMaxBytes {
		s.rotateLog()
		if err := s.ensureLogFile(); err != nil {
			return
		}
	}

	n, err := s.logFile.Write(line)
	if err != nil {
		logger.WarnCF("fanreflex", "log write error", map[string]any{"error": err.Error()})
		return
	}
	s.logSize += int64(n)
}

func (s *Service) ensureLogFile() error {
	if s.logFile != nil {
		return nil
	}
	f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logger.WarnCF("fanreflex", "cannot open log file",
			map[string]any{"path": s.logPath, "error": err.Error()})
		return err
	}
	info, _ := f.Stat()
	if info != nil {
		s.logSize = info.Size()
	}
	s.logFile = f
	return nil
}

func (s *Service) rotateLog() {
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
	old := s.logPath + ".old"
	_ = os.Remove(old)
	_ = os.Rename(s.logPath, old)
	s.logSize = 0
}

func (s *Service) closeLog() {
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
}
