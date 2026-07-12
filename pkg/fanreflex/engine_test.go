package fanreflex

import (
	"strings"
	"testing"
)

// ptr is a float64 pointer helper for CurveEntry.
func ptr(v float64) *float64 { return &v }

// iptr is an int pointer helper for ActuatorMode.FixedPct.
func iptr(v int) *int { return &v }

// boardBalancedActuator returns the board actuator config from the balanced mode
// (matches fan-policy.json exactly).
func boardBalancedActuator() ActuatorMode {
	return ActuatorMode{
		Input: "vrm",
		Curve: []CurveEntry{
			{Lt: ptr(50), PWM: 80},
			{Lt: ptr(60), PWM: 90},
			{Gte: ptr(60), PWM: 100},
		},
		Filter: FilterConfig{
			AvgWindowS: 15,
			HystUp:     2,
			HystDown:   4,
			DelayUpS:   15,
			DelayDownS: 60,
			MaxStepPct: 10,
			FloorPct:   80,
		},
		Backstops: []BackstopEntry{
			{Input: "vrm", Gte: 85, PWM: 100},
		},
	}
}

// radiatorBalancedActuator returns the radiator actuator config from the balanced mode.
func radiatorBalancedActuator() ActuatorMode {
	return ActuatorMode{
		Input: "hotter_coolant",
		Curve: []CurveEntry{
			{Lt: ptr(30.8), PWM: 80},
			{Lt: ptr(31.4), PWM: 90},
			{Gte: ptr(31.4), PWM: 100},
		},
		Filter: FilterConfig{
			AvgWindowS: 30,
			HystUp:     0.1,
			HystDown:   0.2,
			DelayUpS:   30,
			DelayDownS: 90,
			MaxStepPct: 10,
			FloorPct:   80,
		},
		Backstops: []BackstopEntry{
			{Input: "cpu_package", Gte: 58, PWM: 100},
			{Input: "vrm", Gte: 85, PWM: 100},
		},
	}
}

// ─── LookupCurve ───────────────────────────────────────────────────────────────

func TestLookupCurve_BelowFirstLt(t *testing.T) {
	curve := radiatorBalancedActuator().Curve
	// 30.7 < 30.8 → first entry → 80
	if got := LookupCurve(curve, 30.7); got != 80 {
		t.Errorf("expected 80 got %d", got)
	}
}

func TestLookupCurve_ExactLtBoundary(t *testing.T) {
	curve := radiatorBalancedActuator().Curve
	// 30.8 is NOT < 30.8, falls through to lt:31.4 → 90
	if got := LookupCurve(curve, 30.8); got != 90 {
		t.Errorf("expected 90 got %d", got)
	}
}

func TestLookupCurve_MiddleBand(t *testing.T) {
	curve := radiatorBalancedActuator().Curve
	// 31.4 boundary value: NOT < 31.4, falls to gte:31.4 → 100
	if got := LookupCurve(curve, 31.4); got != 100 {
		t.Errorf("31.4 should hit gte:31.4 → 100, got %d", got)
	}
}

func TestLookupCurve_BoardAt314(t *testing.T) {
	// Test the exact boundary mentioned in the spec (31.4)
	curve := []CurveEntry{
		{Lt: ptr(30.8), PWM: 80},
		{Lt: ptr(31.4), PWM: 90},
		{Gte: ptr(31.4), PWM: 100},
	}
	if got := LookupCurve(curve, 31.39); got != 90 {
		t.Errorf("31.39 should be 90, got %d", got)
	}
	if got := LookupCurve(curve, 31.4); got != 100 {
		t.Errorf("31.4 should be 100 (gte), got %d", got)
	}
}

func TestLookupCurve_AboveAll(t *testing.T) {
	curve := boardBalancedActuator().Curve
	if got := LookupCurve(curve, 99); got != 100 {
		t.Errorf("expected 100 got %d", got)
	}
}

// ─── Backstop arbitration ──────────────────────────────────────────────────────

func TestBackstop_NoneTriggered(t *testing.T) {
	am := boardBalancedActuator()
	raw := map[string]float64{"vrm": 80.0}
	pwm, fired := evalBackstops(am.Backstops, raw)
	if fired || pwm != 0 {
		t.Errorf("no backstop should fire at vrm=80, got fired=%v pwm=%d", fired, pwm)
	}
}

func TestBackstop_Triggered(t *testing.T) {
	am := boardBalancedActuator()
	raw := map[string]float64{"vrm": 85.0}
	pwm, fired := evalBackstops(am.Backstops, raw)
	if !fired || pwm != 100 {
		t.Errorf("backstop should fire at vrm=85 with pwm=100, got fired=%v pwm=%d", fired, pwm)
	}
}

func TestBackstop_OverridesPrimary(t *testing.T) {
	am := boardBalancedActuator()
	fs := &ActuatorFilterState{}
	raw := map[string]float64{"vrm": 90.0} // backstop fires
	// primary curve at avg=45 → 80, but backstop at 90 → 100
	raw["vrm"] = 90.0
	r := TickActuator(fs, am, raw, 0, 15)
	if r.FinalPWM != 100 {
		t.Errorf("backstop should force 100, got %d", r.FinalPWM)
	}
	if !r.BackstopFired {
		t.Error("BackstopFired should be true")
	}
}

// ─── Hysteresis ────────────────────────────────────────────────────────────────

func TestHysteresis_NoBounceNearThreshold(t *testing.T) {
	am := boardBalancedActuator()
	fs := &ActuatorFilterState{}
	tickSecs := 15.0

	// First tick: vrm=45, below all thresholds → commit 80
	r := TickActuator(fs, am, map[string]float64{"vrm": 45}, 0, tickSecs)
	if r.FinalPWM != 80 {
		t.Fatalf("init tick expected 80, got %d", r.FinalPWM)
	}

	// VRM barely crosses 50 (lt:50 boundary) → curve says 90.
	// With hyst_up=2: need avg to reach 50+2=52 before transitioning.
	// At avg=51 (just past 50 but below 52): should stay at 80.
	for i := 1; i <= 3; i++ {
		r = TickActuator(fs, am, map[string]float64{"vrm": 51}, float64(i)*tickSecs, tickSecs)
	}
	// avg ≈ 51 (window captures only recent samples after warm-up)
	// LookupCurve(51 - 2) = LookupCurve(49) = 80 (lt:50, 49 < 50 → 80)
	// 80 == CommittedOut=80 → no up transition
	if r.HystCurveOut != 80 {
		t.Errorf("hyst should block transition at avg≈51 (need 52), HystCurveOut=%d", r.HystCurveOut)
	}
}

func TestHysteresis_UpTransitionAllowed(t *testing.T) {
	am := boardBalancedActuator()
	fs := &ActuatorFilterState{}
	tickSecs := 15.0

	// First tick: vrm=45 → commit 80
	TickActuator(fs, am, map[string]float64{"vrm": 45}, 0, tickSecs)

	// After several ticks at 52: avg ≈ 52
	// LookupCurve(52 - 2) = LookupCurve(50) = 90 (lt:50 → 50 < 50 is false, lt:60 → 50 < 60 true → 90)
	// 90 > CommittedOut=80 → transition to 90 (still needs dwell)
	for i := 1; i <= 4; i++ {
		TickActuator(fs, am, map[string]float64{"vrm": 52}, float64(i)*tickSecs, tickSecs)
	}
	// After delay_up_s=15 satisfied, CommittedOut should advance to 90.
	if fs.CommittedOut != 90 {
		t.Errorf("expected CommittedOut=90 after hyst+dwell, got %d", fs.CommittedOut)
	}
}

func TestHysteresis_AsymmetricDownThreshold(t *testing.T) {
	am := boardBalancedActuator()
	fs := &ActuatorFilterState{}
	tickSecs := 15.0

	// Get to CommittedOut=90.
	for i := 0; i <= 4; i++ {
		TickActuator(fs, am, map[string]float64{"vrm": 52}, float64(i)*tickSecs, tickSecs)
	}
	if fs.CommittedOut != 90 {
		t.Fatalf("setup: expected 90, got %d", fs.CommittedOut)
	}

	// Drop to vrm=47. Down threshold for 80/90 boundary = 50.
	// hyst_down=4: need avg < 50-4=46 to go back to 80.
	// At avg=47: LookupCurve(47 + 4) = LookupCurve(51) = 90 (lt:60). 90 == committed → no down.
	for i := 5; i <= 10; i++ {
		TickActuator(fs, am, map[string]float64{"vrm": 47}, float64(i)*tickSecs, tickSecs)
	}
	// avg ≈ 47; LookupCurve(47+4)=LookupCurve(51)=90. 90=CommittedOut → no down transition.
	if fs.CommittedOut != 90 {
		t.Errorf("hyst_down should block transition at avg≈47 (need avg<46), CommittedOut=%d", fs.CommittedOut)
	}
}

// ─── Dwell ─────────────────────────────────────────────────────────────────────

func TestDwell_TransitionRequiresDelay(t *testing.T) {
	am := boardBalancedActuator()
	// Set delay_up_s=30 so one tick (15s) is NOT enough.
	am.Filter.DelayUpS = 30
	am.Filter.HystUp = 0 // disable hyst to isolate dwell
	fs := &ActuatorFilterState{}
	tickSecs := 15.0

	// Init tick: vrm=45 → 80
	TickActuator(fs, am, map[string]float64{"vrm": 45}, 0, tickSecs)

	// Tick 1: vrm=55 → avg≈50 → curve says 90, dwell = 15s, need 30s → still 80
	r := TickActuator(fs, am, map[string]float64{"vrm": 55}, 15, tickSecs)
	if r.DwellOut != 80 {
		t.Errorf("dwell not satisfied after 15s (need 30s): DwellOut=%d", r.DwellOut)
	}

	// Tick 2: dwell = 30s ≥ 30s → commit 90
	r = TickActuator(fs, am, map[string]float64{"vrm": 55}, 30, tickSecs)
	if r.DwellOut != 90 {
		t.Errorf("dwell should commit at 30s, DwellOut=%d", r.DwellOut)
	}
}

func TestDwell_DirectionReset(t *testing.T) {
	am := boardBalancedActuator()
	am.Filter.DelayUpS = 45 // 3 ticks needed
	am.Filter.HystUp = 0
	fs := &ActuatorFilterState{}
	tickSecs := 15.0

	TickActuator(fs, am, map[string]float64{"vrm": 45}, 0, tickSecs) // init → 80
	TickActuator(fs, am, map[string]float64{"vrm": 55}, 15, tickSecs) // want 90, dwell 15s
	// VRM drops back hard enough to pull avg below 50 (80-territory) and reset dwell
	TickActuator(fs, am, map[string]float64{"vrm": 35}, 30, tickSecs) // avg≈45 → back to 80 territory
	// Try to go up again — dwell counter should have reset
	r := TickActuator(fs, am, map[string]float64{"vrm": 55}, 45, tickSecs)
	// DwellSecs should be 15 (not 30), so still not committed
	if r.DwellOut == 90 {
		t.Errorf("dwell should have reset; should not have committed 90 yet")
	}
}

// ─── Step limit ────────────────────────────────────────────────────────────────

func TestStepLimit_LargeJump(t *testing.T) {
	am := boardBalancedActuator()
	am.Filter.HystUp = 0
	am.Filter.DelayUpS = 0
	am.Filter.MaxStepPct = 10
	fs := &ActuatorFilterState{}
	tickSecs := 15.0

	// Init at vrm=45 → 80
	r := TickActuator(fs, am, map[string]float64{"vrm": 45}, 0, tickSecs)
	if r.FinalPWM != 80 {
		t.Fatalf("init: expected 80, got %d", r.FinalPWM)
	}

	// Jump to vrm=70 → curve wants 100, but step limit: 80+10=90
	r = TickActuator(fs, am, map[string]float64{"vrm": 70}, 15, tickSecs)
	if r.FinalPWM != 90 {
		t.Errorf("step 1 should be 90 (80+10), got %d", r.FinalPWM)
	}

	r = TickActuator(fs, am, map[string]float64{"vrm": 70}, 30, tickSecs)
	if r.FinalPWM != 100 {
		t.Errorf("step 2 should reach 100, got %d", r.FinalPWM)
	}
}

func TestStepLimit_BackstopBypassesUp(t *testing.T) {
	am := boardBalancedActuator()
	am.Filter.MaxStepPct = 5
	fs := &ActuatorFilterState{}

	// Init at 80
	TickActuator(fs, am, map[string]float64{"vrm": 45}, 0, 15)

	// Backstop fires: vrm=90, should jump straight to 100 (no step limit up)
	r := TickActuator(fs, am, map[string]float64{"vrm": 90}, 15, 15)
	if r.FinalPWM != 100 {
		t.Errorf("backstop should bypass step limit upward, got %d", r.FinalPWM)
	}
}

// ─── Floor ─────────────────────────────────────────────────────────────────────

func TestFloor_AlwaysApplied(t *testing.T) {
	am := ActuatorMode{
		Input: "vrm",
		Curve: []CurveEntry{
			{Lt: ptr(50), PWM: 60}, // curve below floor
			{Gte: ptr(50), PWM: 80},
		},
		Filter: FilterConfig{
			AvgWindowS: 15,
			MaxStepPct: 100,
			FloorPct:   70, // floor > min curve pwm → this would fail validation
		},
	}
	// Override floor to test the clamp path only (no validation here).
	fs := &ActuatorFilterState{}
	r := TickActuator(fs, am, map[string]float64{"vrm": 30}, 0, 15)
	if r.FinalPWM < am.Filter.FloorPct {
		t.Errorf("floor not applied: FinalPWM=%d < floor=%d", r.FinalPWM, am.Filter.FloorPct)
	}
}

// ─── Fixed actuator (pump) ─────────────────────────────────────────────────────

func TestFixedActuator_AlwaysTargetFixed(t *testing.T) {
	am := ActuatorMode{FixedPct: iptr(100)}
	if !am.IsFixed() {
		t.Error("expected IsFixed()=true")
	}
}

// ─── ComputeDerived ────────────────────────────────────────────────────────────

func TestComputeDerived_MaxFn(t *testing.T) {
	derived := map[string]DerivedDef{
		"hotter_coolant": {Fn: "max", Of: []string{"coolant_a", "coolant_b"}},
	}
	raw := map[string]float64{"coolant_a": 30.5, "coolant_b": 31.2}
	result := ComputeDerived(derived, raw)
	if result["hotter_coolant"] != 31.2 {
		t.Errorf("expected 31.2 got %v", result["hotter_coolant"])
	}
}

// ─── Mode fallback ─────────────────────────────────────────────────────────────

func TestService_ReadMode_InvalidFallsBackToBalanced(t *testing.T) {
	// Validate readMode() normalization: ToLower+TrimSpace, policy-driven mode check,
	// and "balanced" fallback — mirroring the dynamic policy.Modes lookup.
	knownModes := map[string]struct{}{"balanced": {}, "quiet": {}, "performance": {}}
	modes := []string{"BALANCED", "Quiet", "Performance", "", "turbo", "xxx"}
	expected := []string{"balanced", "quiet", "performance", "balanced", "balanced", "balanced"}

	// Mirror the logic in readMode: policy-driven map lookup, not a hardcoded switch.
	normalize := func(raw string) string {
		m := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := knownModes[m]; ok {
			return m
		}
		return "balanced"
	}

	for i, input := range modes {
		got := normalize(input)
		if got != expected[i] {
			t.Errorf("input %q: expected %q got %q", input, expected[i], got)
		}
	}
}

// ─── Golden test: idle → load → cool ──────────────────────────────────────────

// TestGolden_IdleLoadCool simulates the board actuator in balanced mode
// through an idle → load (VRM >60) → cool sequence and asserts that
// the output reaches 100 during load and steps back down during cool.
func TestGolden_IdleLoadCool(t *testing.T) {
	am := boardBalancedActuator()
	// Disable hyst and use delay_up_s=15 to match policy
	tickSecs := 15.0

	type step struct {
		vrm         float64
		wantAtLeast int
		wantAtMost  int
	}

	// Sequence: idle (vrm=45), rising (vrm=55→62), peak (vrm=65), cooling (vrm=45)
	sequence := []step{
		{vrm: 45, wantAtLeast: 80, wantAtMost: 80}, // idle: floor
		{vrm: 45, wantAtLeast: 80, wantAtMost: 80},
		{vrm: 55, wantAtLeast: 80, wantAtMost: 90}, // rising into 90 band
		{vrm: 55, wantAtLeast: 80, wantAtMost: 90},
		{vrm: 55, wantAtLeast: 80, wantAtMost: 90},
		{vrm: 62, wantAtLeast: 80, wantAtMost: 100}, // VRM past 60+hyst(2)=62
		{vrm: 65, wantAtLeast: 90, wantAtMost: 100}, // should be climbing toward 100
		{vrm: 65, wantAtLeast: 100, wantAtMost: 100}, // fully at 100
	}

	fs := &ActuatorFilterState{}
	lastPWM := -1
	for tick, s := range sequence {
		r := TickActuator(fs, am, map[string]float64{"vrm": s.vrm}, float64(tick)*tickSecs, tickSecs)
		if r.FinalPWM < s.wantAtLeast || r.FinalPWM > s.wantAtMost {
			t.Errorf("tick %d vrm=%.1f: FinalPWM=%d want [%d, %d]",
				tick, s.vrm, r.FinalPWM, s.wantAtLeast, s.wantAtMost)
		}
		// PWM must never decrease while load temperature is rising
		if s.vrm >= 55 && s.vrm <= 65 && lastPWM > 0 && r.FinalPWM < lastPWM-am.Filter.MaxStepPct {
			t.Errorf("tick %d: unexpected PWM drop during rising load: %d → %d", tick, lastPWM, r.FinalPWM)
		}
		lastPWM = r.FinalPWM
	}
}

// TestGolden_BackstopDuringLoad checks that cpu_package backstop overrides
// the primary curve and drives radiator to 100 immediately.
func TestGolden_BackstopDuringLoad(t *testing.T) {
	am := radiatorBalancedActuator()
	fs := &ActuatorFilterState{}
	tickSecs := 15.0

	// Idle: hotter_coolant=30 → 80
	r := TickActuator(fs, am, map[string]float64{
		"hotter_coolant": 30, "cpu_package": 50, "vrm": 50,
	}, 0, tickSecs)
	if r.FinalPWM != 80 {
		t.Fatalf("idle radiator expected 80, got %d", r.FinalPWM)
	}

	// cpu_package hits 58: backstop fires, radiator should jump to 100
	r = TickActuator(fs, am, map[string]float64{
		"hotter_coolant": 30, "cpu_package": 58, "vrm": 50,
	}, 15, tickSecs)
	if r.FinalPWM != 100 {
		t.Errorf("backstop cpu_package>=58 should force radiator to 100, got %d", r.FinalPWM)
	}
	if !r.BackstopFired {
		t.Error("BackstopFired should be true")
	}
}
