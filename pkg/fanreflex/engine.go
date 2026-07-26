package fanreflex

// avgSample is one timestamped reading for the moving-average window.
type avgSample struct {
	T float64 // seconds since service start (monotonic)
	V float64
}

// ActuatorFilterState carries all per-actuator state that persists across ticks.
type ActuatorFilterState struct {
	// moving-average window
	Samples []avgSample

	// hysteresis: the last level that was committed after hyst+dwell
	CommittedOut int

	// dwell tracking
	DwellTarget int     // level we're waiting to commit
	DwellSecs   float64 // accumulated seconds toward DwellTarget

	// step limiting and command suppression
	LastSentPWM *int // nil = never sent

	// true after the first tick; skips dwell on initialisation
	Initialized bool
}

// TickResult captures all intermediate values from one engine tick for one
// actuator. The service uses it to build the decision log.
type TickResult struct {
	AvgInput        float64
	PrimaryCurveOut int
	HystCurveOut    int
	DwellOut        int
	BackstopPWM     int
	BackstopFired   bool
	FinalPWM        int
	ShouldSend      bool // false when FinalPWM == LastSentPWM (command suppressed)
}

// LookupCurve evaluates the curve for the given (smoothed) value.
// Entries are checked in declaration order; the first match wins.
// A lt entry matches when value < lt; a gte entry matches when value >= gte.
func LookupCurve(curve []CurveEntry, value float64) int {
	for _, e := range curve {
		if e.Lt != nil && value < *e.Lt {
			return e.PWM
		}
		if e.Gte != nil && value >= *e.Gte {
			return e.PWM
		}
	}
	return curve[len(curve)-1].PWM
}

// addSample appends a new reading and drops samples older than windowSecs.
func (fs *ActuatorFilterState) addSample(now, value, windowSecs float64) {
	fs.Samples = append(fs.Samples, avgSample{T: now, V: value})
	cutoff := now - windowSecs
	i := 0
	for i < len(fs.Samples) && fs.Samples[i].T < cutoff {
		i++
	}
	fs.Samples = fs.Samples[i:]
}

// movingAvg returns the arithmetic mean of all current window samples.
func (fs *ActuatorFilterState) movingAvg() float64 {
	if len(fs.Samples) == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range fs.Samples {
		sum += s.V
	}
	return sum / float64(len(fs.Samples))
}

// applyHysteresis returns the hysteresis-gated desired level.
//
// To go UP the avg must exceed the next-higher band threshold by hyst_up.
// This is tested by evaluating the curve at (avg − hyst_up): if that gives a
// higher level than CommittedOut, the up-transition is allowed.
//
// To go DOWN the avg must fall below the current band threshold by hyst_down.
// Tested by evaluating the curve at (avg + hyst_down): if that gives a lower
// level than CommittedOut, the down-transition is allowed.
func (fs *ActuatorFilterState) applyHysteresis(f FilterConfig, curve []CurveEntry, avg float64) int {
	upBand := LookupCurve(curve, avg-f.HystUp)
	downBand := LookupCurve(curve, avg+f.HystDown)

	if upBand > fs.CommittedOut {
		return upBand
	}
	if downBand < fs.CommittedOut {
		return downBand
	}
	return fs.CommittedOut
}

// applyDwell accumulates transition time and only commits the new level after
// the required dwell delay has elapsed. On the very first tick it commits
// immediately (initialisation).
func (fs *ActuatorFilterState) applyDwell(f FilterConfig, hystOut int, tickSecs float64) int {
	if !fs.Initialized {
		fs.CommittedOut = hystOut
		fs.DwellTarget = hystOut
		fs.DwellSecs = 0
		fs.Initialized = true
		return hystOut
	}

	if hystOut == fs.CommittedOut {
		fs.DwellTarget = hystOut
		fs.DwellSecs = 0
		return fs.CommittedOut
	}

	if hystOut != fs.DwellTarget {
		fs.DwellTarget = hystOut
		fs.DwellSecs = 0
	}

	fs.DwellSecs += tickSecs

	requiredDelay := f.DelayUpS
	if hystOut < fs.CommittedOut {
		requiredDelay = f.DelayDownS
	}

	if fs.DwellSecs >= requiredDelay {
		fs.CommittedOut = hystOut
		fs.DwellSecs = 0
	}
	return fs.CommittedOut
}

// TickActuator runs one full decision tick for a curve-based actuator.
//
// nowSecs is the service-local monotonic time in seconds.
// rawValues holds all raw sensor + derived values keyed by logical name.
func TickActuator(
	fs *ActuatorFilterState,
	am ActuatorMode,
	rawValues map[string]float64,
	nowSecs float64,
	tickSecs float64,
) TickResult {
	input := rawValues[am.Input]
	fs.addSample(nowSecs, input, am.Filter.AvgWindowS)
	avg := fs.movingAvg()

	primaryOut := LookupCurve(am.Curve, avg)
	hystOut := fs.applyHysteresis(am.Filter, am.Curve, avg)
	dwellOut := fs.applyDwell(am.Filter, hystOut, tickSecs)

	backstopPWM, backstopFired := evalBackstops(am.Backstops, rawValues)

	// Arbitrate: backstop can override the filtered primary output.
	target := dwellOut
	backstopActive := backstopFired && backstopPWM > dwellOut
	if backstopActive {
		target = backstopPWM
	}

	// Step limit.
	// Backstop in the UP direction bypasses the limit (safety must react immediately).
	// All other changes (primary up/down, backstop down) are subject to max_step_pct.
	if fs.LastSentPWM != nil {
		diff := target - *fs.LastSentPWM
		bypassStep := backstopActive && diff > 0
		if !bypassStep {
			if diff > am.Filter.MaxStepPct {
				target = *fs.LastSentPWM + am.Filter.MaxStepPct
			} else if diff < -am.Filter.MaxStepPct {
				target = *fs.LastSentPWM - am.Filter.MaxStepPct
			}
		}
	}

	// Floor.
	if target < am.Filter.FloorPct {
		target = am.Filter.FloorPct
	}

	shouldSend := fs.LastSentPWM == nil || *fs.LastSentPWM != target

	return TickResult{
		AvgInput:        avg,
		PrimaryCurveOut: primaryOut,
		HystCurveOut:    hystOut,
		DwellOut:        dwellOut,
		BackstopPWM:     backstopPWM,
		BackstopFired:   backstopFired,
		FinalPWM:        target,
		ShouldSend:      shouldSend,
	}
}

// evalBackstops checks all backstop entries against raw (unsmoothed) sensor
// values. Returns the maximum backstop PWM and whether any backstop fired.
func evalBackstops(backstops []BackstopEntry, rawValues map[string]float64) (int, bool) {
	maxPWM := 0
	fired := false
	for _, bs := range backstops {
		v, ok := rawValues[bs.Input]
		if !ok {
			continue
		}
		if v >= bs.Gte {
			if bs.PWM > maxPWM {
				maxPWM = bs.PWM
			}
			fired = true
		}
	}
	return maxPWM, fired
}

// ComputeDerived evaluates all derived variables and returns a combined map
// containing both raw sensor values and derived values.
func ComputeDerived(derived map[string]DerivedDef, rawSensors map[string]float64) map[string]float64 {
	result := make(map[string]float64, len(rawSensors)+len(derived))
	for k, v := range rawSensors {
		result[k] = v
	}
	for name, def := range derived {
		switch def.Fn {
		case "max":
			var maxVal float64
			first := true
			for _, src := range def.Of {
				if v, ok := result[src]; ok {
					if first || v > maxVal {
						maxVal = v
						first = false
					}
				}
			}
			result[name] = maxVal
		}
	}
	return result
}
