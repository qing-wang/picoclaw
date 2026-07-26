package config

import "testing"

// TestAlohaClawConfig_EffectiveTransport verifies the "mqtt"/"botlink"
// transport selector for the alohaclaw LLM tool defaults to "mqtt"
// (preserving pre-existing behaviour for configs written before BotLink
// existed) and is case-insensitive/whitespace-tolerant/unknown-value-tolerant
// for explicit values — mirroring FanReflexConfig.EffectiveTransport, see
// botlink_test.go.
func TestAlohaClawConfig_EffectiveTransport(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty defaults to mqtt", "", AlohaClawTransportMQTT},
		{"explicit mqtt", "mqtt", AlohaClawTransportMQTT},
		{"explicit botlink", "botlink", AlohaClawTransportBotLink},
		{"case-insensitive", "BotLink", AlohaClawTransportBotLink},
		{"case-insensitive mqtt", "MQTT", AlohaClawTransportMQTT},
		{"whitespace tolerant", "  botlink  ", AlohaClawTransportBotLink},
		{"unknown falls back to mqtt", "carrier-pigeon", AlohaClawTransportMQTT},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := AlohaClawConfig{Transport: tc.value}
			if got := cfg.EffectiveTransport(); got != tc.want {
				t.Errorf("EffectiveTransport() with Transport=%q = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestAlohaClawConfig_DefaultTransportUnaffectsExistingConfig is a regression
// guard: a zero-value AlohaClawConfig (i.e. what every config.json written
// before this feature existed produces) must resolve to the mqtt transport.
func TestAlohaClawConfig_DefaultTransportUnaffectsExistingConfig(t *testing.T) {
	var cfg AlohaClawConfig
	if got := cfg.EffectiveTransport(); got != AlohaClawTransportMQTT {
		t.Fatalf("zero-value AlohaClawConfig.EffectiveTransport() = %q, want %q (regression: old configs must keep working)",
			got, AlohaClawTransportMQTT)
	}
}
