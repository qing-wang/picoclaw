package cliprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sipeed/picoclaw/pkg/isolation"
)

// PicoLMProvider implements LLMProvider using the picolm CLI as a subprocess.
type PicoLMProvider struct {
	command   string
	modelPath string
	template  string
	threads   int
	maxTokens int
	workspace string
}

// NewPicoLMProvider creates a new PicoLM subprocess provider.
func NewPicoLMProvider(command, modelPath, template string, threads, maxTokens int, workspace string) *PicoLMProvider {
	if strings.TrimSpace(command) == "" {
		command = "picolm"
	}
	if strings.TrimSpace(template) == "" {
		template = "chatml"
	}
	return &PicoLMProvider{
		command:   command,
		modelPath: modelPath,
		template:  template,
		threads:   threads,
		maxTokens: maxTokens,
		workspace: workspace,
	}
}

func (p *PicoLMProvider) Chat(
	ctx context.Context, messages []Message, tools []ToolDefinition, model string, options map[string]any,
) (*LLMResponse, error) {
	_ = model
	prompt, err := p.messagesToPrompt(messages, tools)
	if err != nil {
		return nil, err
	}

	args := []string{resolveCLIPath(p.modelPath, p.workspace)}
	if len(tools) > 0 {
		args = append(args, "--json")
	}
	if maxTokens := p.resolveMaxTokens(options); maxTokens > 0 {
		args = append(args, "-n", strconv.Itoa(maxTokens))
	}
	if temperature, ok := floatOption(options["temperature"]); ok {
		args = append(args, "-t", strconv.FormatFloat(temperature, 'f', -1, 64))
	}
	if p.threads > 0 {
		args = append(args, "-j", strconv.Itoa(p.threads))
	}

	cmd := exec.CommandContext(ctx, resolveCommandPath(p.command, p.workspace), args...)
	if p.workspace != "" {
		cmd.Dir = p.workspace
	}
	cmd.Stdin = bytes.NewReader([]byte(prompt))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := isolation.Run(cmd); err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		stdoutStr := strings.TrimSpace(stdout.String())
		switch {
		case stderrStr != "" && stdoutStr != "":
			return nil, fmt.Errorf("picolm error: %w\nstderr: %s\nstdout: %s", err, stderrStr, stdoutStr)
		case stderrStr != "":
			return nil, fmt.Errorf("picolm error: %s", stderrStr)
		case stdoutStr != "":
			return nil, fmt.Errorf("picolm error: %w\noutput: %s", err, stdoutStr)
		default:
			return nil, fmt.Errorf("picolm error: %w", err)
		}
	}

	return p.parseResponse(stdout.String())
}

func (p *PicoLMProvider) GetDefaultModel() string {
	return "picolm-local"
}

func (p *PicoLMProvider) messagesToPrompt(messages []Message, tools []ToolDefinition) (string, error) {
	switch strings.ToLower(strings.TrimSpace(p.template)) {
	case "", "chatml":
		return p.messagesToChatML(messages, tools), nil
	default:
		return "", fmt.Errorf("unsupported picolm template %q", p.template)
	}
}

func (p *PicoLMProvider) messagesToChatML(messages []Message, tools []ToolDefinition) string {
	var sb strings.Builder

	systemPrompt := p.buildSystemPrompt(messages, tools)
	if systemPrompt != "" {
		appendChatMLMessage(&sb, "system", systemPrompt, true)
	}

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			continue
		case "user":
			appendChatMLMessage(&sb, "user", msg.Content, true)
		case "assistant":
			content := strings.TrimSpace(msg.Content)
			if len(msg.ToolCalls) > 0 {
				if tcJSON := marshalToolCalls(msg.ToolCalls); tcJSON != "" {
					if content != "" {
						content += "\n\n"
					}
					content += tcJSON
				}
			}
			appendChatMLMessage(&sb, "assistant", content, true)
		case "tool":
			appendChatMLMessage(&sb, "user", fmt.Sprintf("Tool result for %s:\n%s", msg.ToolCallID, msg.Content), true)
		}
	}

	appendChatMLMessage(&sb, "assistant", "", false)
	return sb.String()
}

func (p *PicoLMProvider) buildSystemPrompt(messages []Message, tools []ToolDefinition) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role == "system" && strings.TrimSpace(msg.Content) != "" {
			parts = append(parts, msg.Content)
		}
	}
	if len(tools) > 0 {
		parts = append(parts, buildCLIToolsPrompt(tools))
	}
	return strings.Join(parts, "\n\n")
}

func (p *PicoLMProvider) parseResponse(output string) (*LLMResponse, error) {
	output = strings.TrimSpace(output)
	toolCalls := extractToolCallsFromText(output)

	finishReason := "stop"
	content := output
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
		content = stripToolCallsFromText(output)
	}

	return &LLMResponse{
		Content:      strings.TrimSpace(content),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}, nil
}

func (p *PicoLMProvider) resolveMaxTokens(options map[string]any) int {
	if options != nil {
		if value, ok := intOption(options["max_tokens"]); ok && value > 0 {
			return value
		}
	}
	if p.maxTokens > 0 {
		return p.maxTokens
	}
	return 0
}

func appendChatMLMessage(sb *strings.Builder, role, content string, close bool) {
	sb.WriteString("<|")
	sb.WriteString(role)
	sb.WriteString("|>\n")
	if content != "" {
		sb.WriteString(content)
		sb.WriteString("\n")
	}
	if close {
		sb.WriteString("</s>\n")
	}
}

func marshalToolCalls(toolCalls []ToolCall) string {
	if len(toolCalls) == 0 {
		return ""
	}
	wrapper := struct {
		ToolCalls []ToolCall `json:"tool_calls"`
	}{ToolCalls: toolCalls}
	data, err := json.Marshal(wrapper)
	if err != nil {
		return ""
	}
	return string(data)
}

func resolveCommandPath(path, workspace string) string {
	path = expandHome(strings.TrimSpace(path))
	if path == "" {
		return path
	}
	if filepath.IsAbs(path) || (!strings.Contains(path, "/") && !strings.Contains(path, `\`) && !strings.HasPrefix(path, ".")) {
		return path
	}
	if workspace == "" {
		return filepath.Clean(path)
	}
	return filepath.Join(workspace, path)
}

func resolveCLIPath(path, workspace string) string {
	path = expandHome(strings.TrimSpace(path))
	if path == "" || filepath.IsAbs(path) || workspace == "" {
		return path
	}
	return filepath.Join(workspace, path)
}

func expandHome(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if len(path) == 1 {
		return home
	}
	if path[1] == '/' || path[1] == '\\' {
		return filepath.Join(home, path[2:])
	}
	return path
}

func intOption(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

func floatOption(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}
