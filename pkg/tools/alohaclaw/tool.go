package alohaclawtools

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// AlohaClawTool lets the LLM send commands and messages to other bots on the
// AlohaClaw MQTT network (e.g. CTFanBot). The MQTT connection is established
// lazily on first use and kept alive for the lifetime of the agent.
type AlohaClawTool struct {
	brokerIP     string
	port         int
	botID        string
	botPassword  string
	replyTimeout time.Duration

	mu       sync.Mutex
	initOnce sync.Once
	client   Sender
	initErr  error
}

func NewAlohaClawTool(brokerIP string, port int, botID, botPassword string, replyTimeout time.Duration) *AlohaClawTool {
	return &AlohaClawTool{
		brokerIP:     brokerIP,
		port:         port,
		botID:        botID,
		botPassword:  botPassword,
		replyTimeout: replyTimeout,
	}
}

func (t *AlohaClawTool) Name() string { return "alohaclaw" }

func (t *AlohaClawTool) Description() string {
	return "Send commands and messages to other bots on the AlohaClaw MQTT network. " +
		"Use send_command to send a text command to a bot and receive its reply. " +
		"Use send_message to send a one-way notification. " +
		"Use status to check whether the AlohaClaw connection is active."
}

func (t *AlohaClawTool) Parameters() map[string]any {
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
				"description": "BotId of the target bot (e.g. \"CTFanBot\"). Required for send_command and send_message.",
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

func (t *AlohaClawTool) getClient() (Sender, error) {
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
	targetID, _ := args["target_id"].(string)
	text, _ := args["text"].(string)
	if targetID == "" {
		return ErrorResult("target_id is required for send_command")
	}
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
	targetID, _ := args["target_id"].(string)
	text, _ := args["text"].(string)
	if targetID == "" {
		return ErrorResult("target_id is required for send_message")
	}
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
