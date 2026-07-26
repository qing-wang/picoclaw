package alohaclawtools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSender is a minimal in-memory Sender used to exercise AlohaClawTool's
// botlink code path without any real network connection.
type fakeSender struct {
	connected     bool
	targetOK      bool
	sendCmdReply  string
	sendCmdErr    error
	sendMsgErr    error
	lastTargetID  string
	lastText      string
	commandCalled bool
	messageCalled bool
}

func (f *fakeSender) SendCommand(_ context.Context, targetID, text string, _ time.Duration) (string, error) {
	f.commandCalled = true
	f.lastTargetID = targetID
	f.lastText = text
	return f.sendCmdReply, f.sendCmdErr
}

func (f *fakeSender) SendMessage(targetID, text string) error {
	f.messageCalled = true
	f.lastTargetID = targetID
	f.lastText = text
	return f.sendMsgErr
}

func (f *fakeSender) IsConnected() bool               { return f.connected }
func (f *fakeSender) IsTargetConnected(_ string) bool { return f.targetOK }

var _ Sender = (*fakeSender)(nil)

// --- target_id / default_target_id resolution -----------------------------

func TestResolveTargetID_UsesArgumentWhenPresent(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "mqtt", "default-bot")
	got, err := tool.resolveTargetID(map[string]any{"target_id": "explicit-bot"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "explicit-bot" {
		t.Errorf("resolveTargetID = %q, want %q (explicit arg must win over default)", got, "explicit-bot")
	}
}

func TestResolveTargetID_FallsBackToDefaultWhenOmitted(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "mqtt", "default-bot")
	got, err := tool.resolveTargetID(map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "default-bot" {
		t.Errorf("resolveTargetID = %q, want %q", got, "default-bot")
	}
}

func TestResolveTargetID_ErrorsWhenBothEmpty(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "mqtt", "")
	_, err := tool.resolveTargetID(map[string]any{})
	if err == nil {
		t.Fatal("expected an explicit error when target_id and default_target_id are both empty, got nil")
	}
	if !strings.Contains(err.Error(), "target_id") {
		t.Errorf("error message should mention target_id, got: %v", err)
	}
}

func TestExecute_SendCommand_NoTargetNoDefault_ReturnsExplicitError(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "mqtt", "")
	res := tool.Execute(context.Background(), map[string]any{
		"action": "send_command",
		"text":   "Ping",
	})
	if !res.IsError {
		t.Fatalf("expected an error result, got: %+v", res)
	}
}

// --- Description() reflects configured default target ----------------------

func TestDescription_MentionsDefaultTargetWhenConfigured(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "mqtt", "bot-fanbot-1")
	desc := tool.Description()
	if !strings.Contains(desc, "bot-fanbot-1") {
		t.Errorf("Description() should mention the configured default_target_id, got: %q", desc)
	}
}

func TestDescription_OmitsDefaultTargetMentionWhenUnconfigured(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "mqtt", "")
	desc := tool.Description()
	if strings.Contains(desc, "default") {
		t.Errorf("Description() should not claim a default target when none is configured, got: %q", desc)
	}
}

func TestParameters_TargetIDDescriptionReflectsDefault(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "mqtt", "bot-fanbot-1")
	params := tool.Parameters()
	props := params["properties"].(map[string]any)
	targetID := props["target_id"].(map[string]any)
	desc := targetID["description"].(string)
	if !strings.Contains(desc, "bot-fanbot-1") {
		t.Errorf("target_id parameter description should mention configured default, got: %q", desc)
	}
}

// --- transport=botlink with no provider wired: must fail loudly, never fall
// back to mqtt silently ------------------------------------------------------

func TestBotLinkTransport_NoProviderWired_SendCommandReturnsExplicitError(t *testing.T) {
	tool := NewAlohaClawTool("broker.example", 8883, "my-bot", "pw", time.Second, "botlink", "target-bot")
	res := tool.Execute(context.Background(), map[string]any{
		"action": "send_command",
		"text":   "Ping",
	})
	if !res.IsError {
		t.Fatalf("expected an error result when botlink provider is not wired, got: %+v", res)
	}
	if !strings.Contains(res.ForLLM, "botlink") {
		t.Errorf("error should mention botlink transport being unavailable, got: %q", res.ForLLM)
	}
	// Must not silently claim success or fall back to mqtt.
	if strings.Contains(res.ForLLM, "Reply from") || strings.Contains(res.ForLLM, "Command sent") {
		t.Errorf("must not silently succeed / fall back to mqtt when botlink provider is unset, got: %q", res.ForLLM)
	}
}

func TestBotLinkTransport_NoProviderWired_StatusReportsDisconnectedNotMQTT(t *testing.T) {
	tool := NewAlohaClawTool("broker.example", 8883, "my-bot", "pw", time.Second, "botlink", "")
	res := tool.Execute(context.Background(), map[string]any{"action": "status"})
	if !strings.Contains(res.ForLLM, "botlink") {
		t.Errorf("status should say the botlink transport is unavailable, got: %q", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "not yet initialized") {
		t.Errorf("status must not report the mqtt-path message when transport=botlink, got: %q", res.ForLLM)
	}
}

func TestBotLinkTransport_ProviderReturnsError_PropagatesExplicitError(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "botlink", "target-bot")
	wantErr := errors.New("boom: hub unreachable")
	tool.SetBotLinkProvider(func() (Sender, error) {
		return nil, wantErr
	})
	res := tool.Execute(context.Background(), map[string]any{
		"action": "send_command",
		"text":   "Ping",
	})
	if !res.IsError {
		t.Fatalf("expected an error result, got: %+v", res)
	}
	if !strings.Contains(res.ForLLM, "boom: hub unreachable") {
		t.Errorf("error should propagate the provider's underlying error, got: %q", res.ForLLM)
	}
}

func TestBotLinkTransport_ProviderReturnsNilSenderNilError_ReturnsExplicitError(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "botlink", "target-bot")
	tool.SetBotLinkProvider(func() (Sender, error) {
		return nil, nil
	})
	res := tool.Execute(context.Background(), map[string]any{
		"action": "send_command",
		"text":   "Ping",
	})
	if !res.IsError {
		t.Fatalf("expected an error result when provider yields a nil sender, got: %+v", res)
	}
}

// --- transport=botlink with a working provider: full success path ----------

func TestBotLinkTransport_ProviderWired_SendCommandUsesResolvedTargetAndSender(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "botlink", "default-bot")
	fake := &fakeSender{connected: true, targetOK: true, sendCmdReply: "42C"}
	tool.SetBotLinkProvider(func() (Sender, error) { return fake, nil })

	res := tool.Execute(context.Background(), map[string]any{
		"action": "send_command",
		"text":   "GetTemp",
	})
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if !fake.commandCalled {
		t.Fatal("expected SendCommand to be called on the botlink sender")
	}
	if fake.lastTargetID != "default-bot" {
		t.Errorf("SendCommand target = %q, want default_target_id %q", fake.lastTargetID, "default-bot")
	}
	if !strings.Contains(res.ForLLM, "42C") {
		t.Errorf("result should contain the reply, got: %q", res.ForLLM)
	}
}

// --- transport=mqtt (default) regression: existing behaviour must be
// unaffected. Status must not touch the network (no lazy-connect on status),
// so this exercises the pre-existing code path without needing a real
// broker. ---------------------------------------------------------------

func TestMQTTTransport_StatusBeforeAnyCommand_ReportsNotYetInitialized(t *testing.T) {
	tool := NewAlohaClawTool("broker.example", 8883, "my-bot", "pw", time.Second, "mqtt", "")
	res := tool.Execute(context.Background(), map[string]any{"action": "status"})
	if !strings.Contains(res.ForLLM, "not yet initialized") {
		t.Errorf("expected pre-existing 'not yet initialized' mqtt status message, got: %q", res.ForLLM)
	}
}

func TestMQTTTransport_EmptyTransportStringBehavesAsMQTT(t *testing.T) {
	// Regression: config.AlohaClawConfig.EffectiveTransport() normalizes "" to
	// "mqtt" before it ever reaches the tool, but the tool itself must also
	// treat anything other than the literal "botlink" as mqtt, so that a
	// zero-value AlohaClawConfig (pre-existing configs) is unaffected even if
	// wired through directly.
	tool := NewAlohaClawTool("broker.example", 8883, "my-bot", "pw", time.Second, "", "")
	res := tool.Execute(context.Background(), map[string]any{"action": "status"})
	if !strings.Contains(res.ForLLM, "not yet initialized") {
		t.Errorf("expected mqtt-path status message for empty transport string, got: %q", res.ForLLM)
	}
}

func TestUnknownAction_ReturnsError(t *testing.T) {
	tool := NewAlohaClawTool("", 0, "", "", time.Second, "mqtt", "")
	res := tool.Execute(context.Background(), map[string]any{"action": "frobnicate"})
	if !res.IsError {
		t.Fatalf("expected error result for unknown action, got: %+v", res)
	}
}
