package alohaclawtools

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// transportBotLink must match config.AlohaClawTransportBotLink (pkg/config).
// The tool package intentionally does not import pkg/config to avoid coupling
// a low-level tool package to the config package; AlohaClawTool receives an
// already-normalized transport string (via config.AlohaClawConfig.
// EffectiveTransport()) from its constructor.
const transportBotLink = "botlink"

// AlohaClawTool lets the LLM send commands and messages to other bots on the
// AlohaClaw network (e.g. CTFanBot). Two transports are supported:
//
//   - "mqtt" (default): connects to the AlohaClaw MQTT broker lazily on first
//     use and keeps the connection alive for the lifetime of the agent. This
//     preserves the tool's original, pre-BotLink behaviour exactly.
//   - "botlink": uses a Sender obtained from a running botlink.Server, wired
//     in after construction via SetBotLinkProvider (see that method's doc
//     comment for why this is deferred rather than passed to the
//     constructor).
type AlohaClawTool struct {
	brokerIP        string
	port            int
	botID           string
	botPassword     string
	replyTimeout    time.Duration
	transport       string
	defaultTargetID string

	mu       sync.Mutex
	initOnce sync.Once
	client   Sender
	initErr  error

	botlinkProvider func() (Sender, error)
}

// NewAlohaClawTool creates a new alohaclaw tool.
//
// transport should be the result of config.AlohaClawConfig.EffectiveTransport()
// ("mqtt" or "botlink"); any value other than "botlink" is treated as "mqtt".
//
// defaultTargetID, if non-empty, is used as the target_id for send_command/
// send_message when the LLM omits that argument.
func NewAlohaClawTool(
	brokerIP string,
	port int,
	botID, botPassword string,
	replyTimeout time.Duration,
	transport string,
	defaultTargetID string,
) *AlohaClawTool {
	return &AlohaClawTool{
		brokerIP:        brokerIP,
		port:            port,
		botID:           botID,
		botPassword:     botPassword,
		replyTimeout:    replyTimeout,
		transport:       transport,
		defaultTargetID: defaultTargetID,
	}
}

// SetBotLinkProvider wires in the function used to resolve a BotLink Sender
// when transport=="botlink". It is safe to call at any time, including after
// Execute has already run.
//
// This is deferred rather than being an argument to NewAlohaClawTool because
// of the construction order in pkg/gateway/gateway.go: the agent loop (and
// the tools registered on it, including AlohaClawTool) is created well before
// the BotLink server exists. Moving botlink.New earlier would just trade one
// fragile ordering requirement for another; a provider function lets the
// gateway wire the connection up whenever the BotLink server becomes
// available (initial startup, and again after every config reload — see
// registerSharedTools/ReloadProviderAndConfig, which construct a fresh
// AlohaClawTool instance that needs rewiring each time).
//
// provider may be nil to explicitly clear a previously-set provider (e.g. if
// the caller wants transport=="botlink" to fail loudly rather than reuse a
// stale sender).
func (t *AlohaClawTool) SetBotLinkProvider(provider func() (Sender, error)) {
	t.mu.Lock()
	t.botlinkProvider = provider
	t.mu.Unlock()
}

func (t *AlohaClawTool) Name() string { return "alohaclaw" }

func (t *AlohaClawTool) Description() string {
	desc := "Send commands and messages to other bots on the AlohaClaw network. " +
		"Use send_command to send a text command to a bot and receive its reply. " +
		"Use send_message to send a one-way notification. " +
		"Use status to check whether the AlohaClaw connection is active."
	if t.defaultTargetID != "" {
		desc += fmt.Sprintf(
			" If target_id is not specified, send_command and send_message default to %q.",
			t.defaultTargetID)
	}
	return desc
}

func (t *AlohaClawTool) Parameters() map[string]any {
	targetDesc := "BotId of the target bot (e.g. \"CTFanBot\")."
	if t.defaultTargetID != "" {
		targetDesc += fmt.Sprintf(" Optional — defaults to %q when omitted.", t.defaultTargetID)
	} else {
		targetDesc += " Required for send_command and send_message."
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"send_command", "send_message", "status"},
				"description": "Action to perform: send_command (send command and wait for reply), send_message (one-way notification), status (check connection)",
			},
			"target_id": map[string]any{
				"type":        "string",
				"description": targetDesc,
			},
			"text": map[string]any{
				"type":        "string",
				"description": "Text content of the command or message. Required for send_command and send_message.",
			},
			"wait_reply": map[string]any{
				"type":        "boolean",
				"description": "For send_command: whether to wait for the target bot's reply. Defaults to true. Set false to fire-and-forget.",
			},
		},
		"required": []string{"action"},
	}
}

func (t *AlohaClawTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, _ := args["action"].(string)
	switch action {
	case "send_command":
		return t.doSendCommand(ctx, args)
	case "send_message":
		return t.doSendMessage(args)
	case "status":
		return t.doStatus()
	default:
		return ErrorResult(fmt.Sprintf("unknown action %q — valid: send_command, send_message, status", action))
	}
}

// resolveTargetID returns the target_id to use for send_command/send_message:
// the LLM-supplied argument if present, otherwise defaultTargetID. Returns an
// error if both are empty.
func (t *AlohaClawTool) resolveTargetID(args map[string]any) (string, error) {
	if targetID, _ := args["target_id"].(string); targetID != "" {
		return targetID, nil
	}
	if t.defaultTargetID != "" {
		return t.defaultTargetID, nil
	}
	return "", fmt.Errorf("target_id not specified and no default_target_id configured")
}

// getClient returns the Sender to use for this call, per t.transport.
func (t *AlohaClawTool) getClient() (Sender, error) {
	if t.transport == transportBotLink {
		return t.getBotLinkClient()
	}
	return t.getMQTTClient()
}

// getBotLinkClient resolves the BotLink Sender via the provider wired in by
// SetBotLinkProvider. It deliberately never falls back to MQTT: a silent
// fallback would make the LLM (and the user asking it a question) believe
// they are talking over the LAN-only BotLink transport when the reply
// actually went over the cloud MQTT broker.
func (t *AlohaClawTool) getBotLinkClient() (Sender, error) {
	t.mu.Lock()
	provider := t.botlinkProvider
	t.mu.Unlock()

	if provider == nil {
		return nil, fmt.Errorf(
			"alohaclaw: transport=botlink but no BotLink sender is connected yet " +
				"(is tools.botlink.enabled true, and has the gateway finished starting up?)")
	}
	cl, err := provider()
	if err != nil {
		return nil, fmt.Errorf("alohaclaw: transport=botlink but BotLink is unavailable: %w", err)
	}
	if cl == nil {
		return nil, fmt.Errorf(
			"alohaclaw: transport=botlink but no BotLink sender is connected yet " +
				"(is tools.botlink.enabled true?)")
	}
	return cl, nil
}

// getMQTTClient lazily connects to the AlohaClaw MQTT broker on first use,
// exactly as before transport selection was introduced.
func (t *AlohaClawTool) getMQTTClient() (Sender, error) {
	t.initOnce.Do(func() {
		cl, err := GetOrCreateClient(t.brokerIP, t.port, t.botID, t.botPassword)
		if err != nil {
			t.initErr = fmt.Errorf("AlohaClaw connect failed: %w", err)
			return
		}
		t.mu.Lock()
		t.client = cl
		t.mu.Unlock()
	})
	t.mu.Lock()
	cl := t.client
	err := t.initErr
	t.mu.Unlock()
	return cl, err
}

func (t *AlohaClawTool) doSendCommand(ctx context.Context, args map[string]any) *ToolResult {
	targetID, err := t.resolveTargetID(args)
	if err != nil {
		return ErrorResult(err.Error())
	}
	text, _ := args["text"].(string)
	if text == "" {
		return ErrorResult("text is required for send_command")
	}

	waitReply := true
	if v, ok := args["wait_reply"].(bool); ok {
		waitReply = v
	}

	cl, err := t.getClient()
	if err != nil {
		return ErrorResult(err.Error())
	}

	var timeout time.Duration
	if waitReply {
		timeout = t.replyTimeout
	}

	reply, err := cl.SendCommand(ctx, targetID, text, timeout)
	if err != nil {
		return ErrorResult(fmt.Sprintf("send_command to %q: %v", targetID, err))
	}
	if !waitReply || reply == "" {
		return SilentResult(fmt.Sprintf("Command sent to %q.", targetID))
	}
	return SilentResult(fmt.Sprintf("Reply from %q: %s", targetID, reply))
}

func (t *AlohaClawTool) doSendMessage(args map[string]any) *ToolResult {
	targetID, err := t.resolveTargetID(args)
	if err != nil {
		return ErrorResult(err.Error())
	}
	text, _ := args["text"].(string)
	if text == "" {
		return ErrorResult("text is required for send_message")
	}

	cl, err := t.getClient()
	if err != nil {
		return ErrorResult(err.Error())
	}
	if err = cl.SendMessage(targetID, text); err != nil {
		return ErrorResult(fmt.Sprintf("send_message to %q: %v", targetID, err))
	}
	return SilentResult(fmt.Sprintf("Message sent to %q.", targetID))
}

func (t *AlohaClawTool) doStatus() *ToolResult {
	if t.transport == transportBotLink {
		return t.doStatusBotLink()
	}
	return t.doStatusMQTT()
}

func (t *AlohaClawTool) doStatusBotLink() *ToolResult {
	cl, err := t.getBotLinkClient()
	if err != nil {
		return SilentResult(fmt.Sprintf("AlohaClaw status: disconnected — %v", err))
	}
	if cl.IsConnected() {
		return SilentResult("AlohaClaw status: connected (botlink transport)")
	}
	return SilentResult("AlohaClaw status: no bots currently connected (botlink transport)")
}

func (t *AlohaClawTool) doStatusMQTT() *ToolResult {
	t.mu.Lock()
	cl := t.client
	initErr := t.initErr
	t.mu.Unlock()

	if initErr != nil {
		return SilentResult(fmt.Sprintf("AlohaClaw status: disconnected — %v", initErr))
	}
	if cl == nil {
		return SilentResult("AlohaClaw status: not yet initialized (no commands sent)")
	}
	if cl.IsConnected() {
		return SilentResult(fmt.Sprintf("AlohaClaw status: connected as %q to %s:%d", t.botID, t.brokerIP, t.port))
	}
	return SilentResult(fmt.Sprintf("AlohaClaw status: reconnecting to %s:%d", t.brokerIP, t.port))
}
