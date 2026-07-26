package config

import (
	"encoding/json"
	"testing"
)

// TestFanReflexConfig_EffectiveTransport verifies the "alohaclaw"/"botlink"
// transport selector defaults to "alohaclaw" (preserving pre-existing MQTT
// behaviour for configs written before BotLink existed) and is
// case-insensitive/whitespace-tolerant for explicit values.
func TestFanReflexConfig_EffectiveTransport(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty defaults to alohaclaw", "", FanReflexTransportAlohaClaw},
		{"explicit alohaclaw", "alohaclaw", FanReflexTransportAlohaClaw},
		{"explicit botlink", "botlink", FanReflexTransportBotLink},
		{"case-insensitive", "BotLink", FanReflexTransportBotLink},
		{"whitespace tolerant", "  botlink  ", FanReflexTransportBotLink},
		{"unknown falls back to alohaclaw", "carrier-pigeon", FanReflexTransportAlohaClaw},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := FanReflexConfig{Transport: tc.value}
			if got := cfg.EffectiveTransport(); got != tc.want {
				t.Errorf("EffectiveTransport() with Transport=%q = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestBotLinkConfig_JSONRoundTrip verifies BotLinkConfig/BotLinkBot survive a
// JSON round-trip through config.json (token_sha256 is a hash, so — unlike
// AlohaClawConfig.BotPassword — it lives in plain config.json, not
// .security.yml; see the BotLinkConfig doc comment).
func TestBotLinkConfig_JSONRoundTrip(t *testing.T) {
	cfg := BotLinkConfig{
		Enabled: true,
		Bots: []BotLinkBot{
			{BotID: "CTFanBot", TokenSHA256: "deadbeef", Name: "Fan Controller"},
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got BotLinkConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !got.Enabled {
		t.Error("Enabled should round-trip true")
	}
	if len(got.Bots) != 1 {
		t.Fatalf("expected 1 bot, got %d", len(got.Bots))
	}
	b := got.Bots[0]
	if b.BotID != "CTFanBot" || b.TokenSHA256 != "deadbeef" || b.Name != "Fan Controller" {
		t.Errorf("bot round-trip mismatch: %+v", b)
	}
}
