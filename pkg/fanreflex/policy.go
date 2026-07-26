package fanreflex

import (
	"encoding/json"
	"fmt"
	"os"
)

// Policy is the machine fan policy (fan-policy.json format, version 1).
type Policy struct {
	Version         int                                `json:"version"`
	Machine         string                             `json:"machine"`
	TickSeconds     int                                `json:"tick_seconds"`
	Sensors         map[string]string                  `json:"sensors"`
	Derived         map[string]DerivedDef              `json:"derived"`
	RequiredSensors []string                           `json:"required_sensors"`
	Actuators       map[string]ActuatorDef             `json:"actuators"`
	Modes           map[string]map[string]ActuatorMode `json:"modes"`
	Faults          FaultsDef                          `json:"faults"`
	FailSafe        FailSafeDef                        `json:"failsafe"`
}

// DerivedDef describes a computed variable (currently only "max" is supported).
type DerivedDef struct {
	Fn string   `json:"fn"`
	Of []string `json:"of"`
}

// ActuatorDef describes a physical fan channel.
type ActuatorDef struct {
	Channel       string `json:"channel"`
	TachValidated bool   `json:"tach_validated"`
}

// ActuatorMode is the control configuration for one actuator in one mode.
// Either FixedPct (pump) or Input+Curve+Filter (fan) must be set.
type ActuatorMode struct {
	FixedPct  *int            `json:"fixed_pct,omitempty"`
	Input     string          `json:"input,omitempty"`
	Curve     []CurveEntry    `json:"curve,omitempty"`
	Filter    FilterConfig    `json:"filter,omitempty"`
	Backstops []BackstopEntry `json:"backstops,omitempty"`
}

// IsFixed returns true when this actuator uses a fixed PWM value.
func (a *ActuatorMode) IsFixed() bool { return a.FixedPct != nil }

// CurveEntry is one step in a piecewise-constant fan curve.
// Exactly one of Lt or Gte is set; entries are evaluated in order and the
// first matching entry wins.
type CurveEntry struct {
	Lt  *float64 `json:"lt,omitempty"`
	Gte *float64 `json:"gte,omitempty"`
	PWM int      `json:"pwm"`
}

// FilterConfig holds the five anti-oscillation filter parameters.
type FilterConfig struct {
	AvgWindowS float64 `json:"avg_window_s"`
	HystUp     float64 `json:"hyst_up"`
	HystDown   float64 `json:"hyst_down"`
	DelayUpS   float64 `json:"delay_up_s"`
	DelayDownS float64 `json:"delay_down_s"`
	MaxStepPct int     `json:"max_step_pct"`
	FloorPct   int     `json:"floor_pct"`
}

// BackstopEntry is a raw-input safety override for an actuator.
type BackstopEntry struct {
	Input string  `json:"input"`
	Gte   float64 `json:"gte"`
	PWM   int     `json:"pwm"`
}

// FaultsDef groups the fault detection configurations.
type FaultsDef struct {
	PumpZeroTach    *PumpZeroTachFault    `json:"pump_zero_tach,omitempty"`
	FlowLow         *FlowLowFault         `json:"flow_low,omitempty"`
	UnresponsiveFan *UnresponsiveFanFault `json:"unresponsive_fan,omitempty"`
}

// PumpZeroTachFault fires immediately when pump tach reads zero while a
// non-zero pump target is commanded.
type PumpZeroTachFault struct {
	Sensor        string `json:"sensor"`
	Eq            int    `json:"eq"`
	WhileTargetGt int    `json:"while_target_gt"`
}

// FlowLowFault fires when flow stays at or below the threshold for a
// sustained duration while the pump is at full speed.
type FlowLowFault struct {
	Sensor       string  `json:"sensor"`
	Lte          float64 `json:"lte"`
	SustainS     int     `json:"sustain_s"`
	WhilePumpPct int     `json:"while_pump_pct"`
}

// UnresponsiveFanFault tracks how many consecutive polls a tach-validated
// actuator can fail to respond before an alarm is raised.
type UnresponsiveFanFault struct {
	Polls int `json:"polls"`
}

// FailSafeDef describes actions taken when a critical fault is detected.
type FailSafeDef struct {
	Commands               []FailSafeCommand `json:"commands"`
	StopLoad               bool              `json:"stop_load"`
	Notify                 bool              `json:"notify"`
	ShutdownRecommendation string            `json:"shutdown_recommendation"`
}

// FailSafeCommand is a [channel, pwm] pair encoded as a 2-element JSON array.
type FailSafeCommand struct {
	Channel string
	PWM     int
}

func (c *FailSafeCommand) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("failsafe command must be [channel, pwm], got %d elements", len(raw))
	}
	if err := json.Unmarshal(raw[0], &c.Channel); err != nil {
		return fmt.Errorf("failsafe command channel: %w", err)
	}
	if err := json.Unmarshal(raw[1], &c.PWM); err != nil {
		return fmt.Errorf("failsafe command pwm: %w", err)
	}
	return nil
}

// LoadAndValidatePolicy reads, parses, and validates a policy file.
func LoadAndValidatePolicy(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	if err := ValidatePolicy(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ValidatePolicy checks all policy invariants. A non-nil error describes the
// first violation; the service refuses to start if validation fails.
func ValidatePolicy(p *Policy) error {
	if p.Version != 1 {
		return fmt.Errorf("policy version must be 1, got %d", p.Version)
	}

	if p.TickSeconds < 1 {
		return fmt.Errorf("tick_seconds must be >= 1, got %d", p.TickSeconds)
	}

	for modeName, modeMap := range p.Modes {
		for actuatorName := range p.Actuators {
			if _, ok := modeMap[actuatorName]; !ok {
				return fmt.Errorf("mode %q missing actuator %q", modeName, actuatorName)
			}
		}
		for actuatorName, am := range modeMap {
			if err := validateActuatorMode(p, modeName, actuatorName, am); err != nil {
				return err
			}
		}
	}

	for _, rs := range p.RequiredSensors {
		if _, ok := p.Sensors[rs]; !ok {
			return fmt.Errorf("required_sensor %q not in sensors map", rs)
		}
	}

	// "balanced" is the hard-coded fallback in readMode(); without it a missing
	// or invalid mode file would silently result in no modeMap and no control.
	if _, ok := p.Modes["balanced"]; !ok {
		return fmt.Errorf("modes must include \"balanced\" (required fallback)")
	}

	// pump_zero_tach and flow_low hard-code the "pump" actuator name; require
	// it to exist as a fixed actuator in every mode so faults never silently misfire.
	if p.Faults.PumpZeroTach != nil || p.Faults.FlowLow != nil {
		if _, hasPump := p.Actuators["pump"]; !hasPump {
			return fmt.Errorf("pump fault configured but no 'pump' actuator defined")
		}
		for modeName, modeMap := range p.Modes {
			am := modeMap["pump"]
			if !am.IsFixed() {
				return fmt.Errorf("pump fault configured but mode %q 'pump' actuator is not fixed_pct", modeName)
			}
		}
	}

	return nil
}

func validateActuatorMode(p *Policy, modeName, actuatorName string, am ActuatorMode) error {
	if am.FixedPct != nil {
		if *am.FixedPct < 0 || *am.FixedPct > 100 {
			return fmt.Errorf("mode %q actuator %q fixed_pct %d out of [0,100]", modeName, actuatorName, *am.FixedPct)
		}
		return nil
	}

	_, inputInSensors := p.Sensors[am.Input]
	_, inputInDerived := p.Derived[am.Input]
	if !inputInSensors && !inputInDerived {
		return fmt.Errorf("mode %q actuator %q input %q not in sensors/derived", modeName, actuatorName, am.Input)
	}

	if len(am.Curve) == 0 {
		return fmt.Errorf("mode %q actuator %q has empty curve", modeName, actuatorName)
	}
	if am.Curve[len(am.Curve)-1].Gte == nil {
		return fmt.Errorf("mode %q actuator %q last curve entry must be gte", modeName, actuatorName)
	}

	minCurvePWM := 101
	for _, e := range am.Curve {
		if e.PWM < 0 || e.PWM > 100 {
			return fmt.Errorf("mode %q actuator %q curve pwm %d out of [0,100]", modeName, actuatorName, e.PWM)
		}
		if e.PWM < minCurvePWM {
			minCurvePWM = e.PWM
		}
	}

	if am.Filter.FloorPct < 0 || am.Filter.FloorPct > 100 {
		return fmt.Errorf("mode %q actuator %q floor_pct %d out of [0,100]", modeName, actuatorName, am.Filter.FloorPct)
	}
	if am.Filter.FloorPct > minCurvePWM {
		return fmt.Errorf("mode %q actuator %q floor_pct %d > min curve pwm %d",
			modeName, actuatorName, am.Filter.FloorPct, minCurvePWM)
	}

	for _, bs := range am.Backstops {
		_, inSensors := p.Sensors[bs.Input]
		_, inDerived := p.Derived[bs.Input]
		if !inSensors && !inDerived {
			return fmt.Errorf("mode %q actuator %q backstop input %q not in sensors/derived",
				modeName, actuatorName, bs.Input)
		}
	}

	return nil
}
