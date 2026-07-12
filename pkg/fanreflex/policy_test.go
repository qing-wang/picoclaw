package fanreflex

import (
	"encoding/json"
	"testing"
)

// basePolicy returns a minimal valid policy for testing.
func basePolicy() *Policy {
	lt50 := 50.0
	gte50 := 50.0
	fixed := 100
	return &Policy{
		Version:     1,
		TickSeconds: 15,
		Sensors: map[string]string{
			"cpu_package": "Pkg",
			"vrm":         "VRM",
			"coolant_a":   "NTC1",
		},
		Derived: map[string]DerivedDef{},
		RequiredSensors: []string{"cpu_package", "vrm"},
		Actuators: map[string]ActuatorDef{
			"fan1": {Channel: "Fan1", TachValidated: false},
			"pump": {Channel: "Pump", TachValidated: true},
		},
		Modes: map[string]map[string]ActuatorMode{
			"balanced": {
				"fan1": {
					Input: "vrm",
					Curve: []CurveEntry{
						{Lt: &lt50, PWM: 80},
						{Gte: &gte50, PWM: 100},
					},
					Filter: FilterConfig{FloorPct: 80, MaxStepPct: 10, AvgWindowS: 15},
					Backstops: []BackstopEntry{
						{Input: "vrm", Gte: 85, PWM: 100},
					},
				},
				"pump": {FixedPct: &fixed},
			},
		},
		FailSafe: FailSafeDef{
			Commands: []FailSafeCommand{{Channel: "Fan1", PWM: 100}},
		},
	}
}

func TestValidatePolicy_Valid(t *testing.T) {
	p := basePolicy()
	if err := ValidatePolicy(p); err != nil {
		t.Errorf("valid policy should pass, got: %v", err)
	}
}

func TestValidatePolicy_WrongVersion(t *testing.T) {
	p := basePolicy()
	p.Version = 2
	if err := ValidatePolicy(p); err == nil {
		t.Error("version != 1 should be rejected")
	}
}

func TestValidatePolicy_MissingActuatorInMode(t *testing.T) {
	p := basePolicy()
	delete(p.Modes["balanced"], "pump")
	if err := ValidatePolicy(p); err == nil {
		t.Error("missing actuator in mode should be rejected")
	}
}

func TestValidatePolicy_EmptyCurve(t *testing.T) {
	p := basePolicy()
	fan := p.Modes["balanced"]["fan1"]
	fan.Curve = nil
	p.Modes["balanced"]["fan1"] = fan
	if err := ValidatePolicy(p); err == nil {
		t.Error("empty curve should be rejected")
	}
}

func TestValidatePolicy_LastEntryNotGte(t *testing.T) {
	p := basePolicy()
	fan := p.Modes["balanced"]["fan1"]
	lt80 := 80.0
	fan.Curve = append(fan.Curve, CurveEntry{Lt: &lt80, PWM: 100})
	// Ensure last entry is an lt (not gte)
	p.Modes["balanced"]["fan1"] = fan
	if err := ValidatePolicy(p); err == nil {
		t.Error("last curve entry not gte should be rejected")
	}
}

func TestValidatePolicy_PWMOutOfRange(t *testing.T) {
	p := basePolicy()
	fan := p.Modes["balanced"]["fan1"]
	gte50 := 50.0
	fan.Curve = []CurveEntry{
		{Lt: ptr(50), PWM: 110}, // > 100
		{Gte: &gte50, PWM: 100},
	}
	p.Modes["balanced"]["fan1"] = fan
	if err := ValidatePolicy(p); err == nil {
		t.Error("pwm > 100 should be rejected")
	}
}

func TestValidatePolicy_FloorGreaterThanMinCurvePWM(t *testing.T) {
	p := basePolicy()
	fan := p.Modes["balanced"]["fan1"]
	fan.Filter.FloorPct = 90 // min curve pwm is 80
	p.Modes["balanced"]["fan1"] = fan
	if err := ValidatePolicy(p); err == nil {
		t.Error("floor > min curve pwm should be rejected")
	}
}

func TestValidatePolicy_BackstopInputNotInSensors(t *testing.T) {
	p := basePolicy()
	fan := p.Modes["balanced"]["fan1"]
	fan.Backstops = []BackstopEntry{{Input: "nonexistent", Gte: 85, PWM: 100}}
	p.Modes["balanced"]["fan1"] = fan
	if err := ValidatePolicy(p); err == nil {
		t.Error("backstop with unknown input should be rejected")
	}
}

func TestValidatePolicy_BackstopInputInDerived(t *testing.T) {
	p := basePolicy()
	p.Derived["hotter_coolant"] = DerivedDef{Fn: "max", Of: []string{"coolant_a"}}
	fan := p.Modes["balanced"]["fan1"]
	fan.Backstops = []BackstopEntry{{Input: "hotter_coolant", Gte: 35, PWM: 100}}
	p.Modes["balanced"]["fan1"] = fan
	if err := ValidatePolicy(p); err != nil {
		t.Errorf("derived input in backstop should be valid: %v", err)
	}
}

func TestValidatePolicy_RequiredSensorNotInMap(t *testing.T) {
	p := basePolicy()
	p.RequiredSensors = append(p.RequiredSensors, "ghost_sensor")
	if err := ValidatePolicy(p); err == nil {
		t.Error("required_sensor not in sensors map should be rejected")
	}
}

func TestValidatePolicy_FixedPctOutOfRange(t *testing.T) {
	p := basePolicy()
	bad := 150
	p.Modes["balanced"]["pump"] = ActuatorMode{FixedPct: &bad}
	if err := ValidatePolicy(p); err == nil {
		t.Error("fixed_pct > 100 should be rejected")
	}
}

func TestValidatePolicy_TickSecondsZero(t *testing.T) {
	p := basePolicy()
	p.TickSeconds = 0
	if err := ValidatePolicy(p); err == nil {
		t.Error("tick_seconds=0 should be rejected")
	}
}

func TestValidatePolicy_InputNotInSensors(t *testing.T) {
	p := basePolicy()
	fan := p.Modes["balanced"]["fan1"]
	fan.Input = "ghost_sensor"
	p.Modes["balanced"]["fan1"] = fan
	if err := ValidatePolicy(p); err == nil {
		t.Error("input not in sensors/derived should be rejected")
	}
}

func TestValidatePolicy_PumpFaultWithoutPumpActuator(t *testing.T) {
	p := basePolicy()
	// Add pump fault but rename the pump actuator so it doesn't exist.
	p.Faults.PumpZeroTach = &PumpZeroTachFault{Sensor: "pump_tach", Eq: 0, WhileTargetGt: 0}
	delete(p.Actuators, "pump")
	delete(p.Modes["balanced"], "pump")
	if err := ValidatePolicy(p); err == nil {
		t.Error("pump fault without 'pump' actuator should be rejected")
	}
}

func TestValidatePolicy_PumpFaultPumpNotFixed(t *testing.T) {
	p := basePolicy()
	p.Faults.FlowLow = &FlowLowFault{Sensor: "coolant_a", Lte: 0.5, SustainS: 30, WhilePumpPct: 100}
	// Make pump a curve actuator (not fixed).
	p.Sensors["pump_tach"] = "Pump Tach"
	fan := p.Modes["balanced"]["fan1"]
	p.Modes["balanced"]["pump"] = fan // curve, not fixed
	if err := ValidatePolicy(p); err == nil {
		t.Error("pump fault with non-fixed pump actuator should be rejected")
	}
}

func TestValidatePolicy_MissingBalancedMode(t *testing.T) {
	p := basePolicy()
	// Rename "balanced" to a custom name so the required fallback anchor is absent.
	p.Modes["night"] = p.Modes["balanced"]
	delete(p.Modes, "balanced")
	if err := ValidatePolicy(p); err == nil {
		t.Error("policy without 'balanced' mode should be rejected")
	}
}

// TestValidatePolicy_RealFanPolicy loads a verbatim copy of fan-policy.json and
// validates it, avoiding a file-system dependency in the test binary.
func TestValidatePolicy_RealFanPolicy(t *testing.T) {
	var p Policy
	if err := json.Unmarshal([]byte(realFanPolicyJSON()), &p); err != nil {
		t.Fatalf("parse real policy: %v", err)
	}
	if err := ValidatePolicy(&p); err != nil {
		t.Errorf("real fan-policy.json should be valid: %v", err)
	}
}

func realFanPolicyJSON() string {
	return `{
  "version": 1,
  "machine": "bot-fanbot-2",
  "generated_from": "fan-profile.md @ 2026-07-12",
  "tick_seconds": 15,
  "sensors": {
    "cpu_package": "13th Gen Intel Core i7-13700 / CPU Package",
    "vrm": "ITE IT8689E / Temperature #5",
    "coolant_a": "CoolTek M4600 / Temp NTC1",
    "coolant_b": "CoolTek M4600 / Temp NTC2",
    "flow": "CoolTek M4600 / Flow Rate",
    "pump_tach": "CoolTek M4600 / Fan 2"
  },
  "derived": {
    "hotter_coolant": {"fn": "max", "of": ["coolant_a", "coolant_b"]}
  },
  "required_sensors": ["cpu_package", "vrm", "coolant_a", "coolant_b", "flow"],
  "actuators": {
    "radiator": {"channel": "CoolTek M4600 / Fan 1", "tach_validated": true},
    "pump":     {"channel": "CoolTek M4600 / Fan 2", "tach_validated": true},
    "board":    {"channel": "ITE IT8689E / Fan #4",  "tach_validated": true}
  },
  "modes": {
    "quiet": {
      "radiator": {
        "input": "hotter_coolant",
        "curve": [
          {"lt": 31.0, "pwm": 80}, {"lt": 31.6, "pwm": 90}, {"gte": 31.6, "pwm": 100}
        ],
        "filter": {"avg_window_s": 30, "hyst_up": 0.1, "hyst_down": 0.2,
                   "delay_up_s": 30, "delay_down_s": 120, "max_step_pct": 10, "floor_pct": 80},
        "backstops": [
          {"input": "cpu_package", "gte": 58, "pwm": 100},
          {"input": "vrm", "gte": 85, "pwm": 100}
        ]
      },
      "pump": {"fixed_pct": 100},
      "board": {
        "input": "vrm",
        "curve": [
          {"lt": 50, "pwm": 80}, {"lt": 64, "pwm": 90}, {"gte": 64, "pwm": 100}
        ],
        "filter": {"avg_window_s": 15, "hyst_up": 2, "hyst_down": 4,
                   "delay_up_s": 15, "delay_down_s": 90, "max_step_pct": 10, "floor_pct": 80},
        "backstops": [{"input": "vrm", "gte": 85, "pwm": 100}]
      }
    },
    "balanced": {
      "radiator": {
        "input": "hotter_coolant",
        "curve": [
          {"lt": 30.8, "pwm": 80}, {"lt": 31.4, "pwm": 90}, {"gte": 31.4, "pwm": 100}
        ],
        "filter": {"avg_window_s": 30, "hyst_up": 0.1, "hyst_down": 0.2,
                   "delay_up_s": 30, "delay_down_s": 90, "max_step_pct": 10, "floor_pct": 80},
        "backstops": [
          {"input": "cpu_package", "gte": 58, "pwm": 100},
          {"input": "vrm", "gte": 85, "pwm": 100}
        ]
      },
      "pump": {"fixed_pct": 100},
      "board": {
        "input": "vrm",
        "curve": [
          {"lt": 50, "pwm": 80}, {"lt": 60, "pwm": 90}, {"gte": 60, "pwm": 100}
        ],
        "filter": {"avg_window_s": 15, "hyst_up": 2, "hyst_down": 4,
                   "delay_up_s": 15, "delay_down_s": 60, "max_step_pct": 10, "floor_pct": 80},
        "backstops": [{"input": "vrm", "gte": 85, "pwm": 100}]
      }
    },
    "performance": {
      "radiator": {
        "input": "hotter_coolant",
        "curve": [
          {"lt": 30.6, "pwm": 90}, {"lt": 31.2, "pwm": 95}, {"gte": 31.2, "pwm": 100}
        ],
        "filter": {"avg_window_s": 15, "hyst_up": 0.05, "hyst_down": 0.15,
                   "delay_up_s": 15, "delay_down_s": 60, "max_step_pct": 10, "floor_pct": 90},
        "backstops": [
          {"input": "cpu_package", "gte": 58, "pwm": 100},
          {"input": "vrm", "gte": 85, "pwm": 100}
        ]
      },
      "pump": {"fixed_pct": 100},
      "board": {
        "input": "vrm",
        "curve": [
          {"lt": 48, "pwm": 90}, {"lt": 58, "pwm": 95}, {"gte": 58, "pwm": 100}
        ],
        "filter": {"avg_window_s": 15, "hyst_up": 1, "hyst_down": 3,
                   "delay_up_s": 15, "delay_down_s": 45, "max_step_pct": 10, "floor_pct": 90},
        "backstops": [{"input": "vrm", "gte": 85, "pwm": 100}]
      }
    }
  },
  "faults": {
    "pump_zero_tach": {"sensor": "pump_tach", "eq": 0, "while_target_gt": 0},
    "flow_low": {"sensor": "flow", "lte": 0.65, "sustain_s": 30, "while_pump_pct": 100},
    "unresponsive_fan": {"polls": 2}
  },
  "failsafe": {
    "commands": [
      ["CoolTek M4600 / Fan 1", 100],
      ["CoolTek M4600 / Fan 2", 100],
      ["ITE IT8689E / Fan #4", 100]
    ],
    "stop_load": true,
    "notify": true,
    "shutdown_recommendation": "Cooling telemetry is unreliable."
  }
}`
}

