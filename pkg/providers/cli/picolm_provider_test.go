package cliprovider

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewPicoLMProvider_Defaults(t *testing.T) {
	p := NewPicoLMProvider("", "model.gguf", "", 4, 256, "/workspace")
	if p.command != "picolm" {
		t.Fatalf("command = %q, want picolm", p.command)
	}
	if p.template != "chatml" {
		t.Fatalf("template = %q, want chatml", p.template)
	}
}

func TestPicoLMMessagesToPrompt_ChatML(t *testing.T) {
	p := NewPicoLMProvider("picolm", "model.gguf", "chatml", 4, 256, "/workspace")

	prompt, err := p.messagesToPrompt([]Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there"},
		{Role: "tool", ToolCallID: "call_1", Content: "file content"},
	}, []ToolDefinition{
		{
			Type: "function",
			Function: ToolFunctionDefinition{
				Name:        "read_file",
				Description: "Read a file",
				Parameters:  map[string]any{"type": "object"},
			},
		},
	})
	if err != nil {
		t.Fatalf("messagesToPrompt() error = %v", err)
	}

	if !strings.Contains(prompt, "<|system|>\nYou are helpful.") {
		t.Fatalf("prompt missing system block: %q", prompt)
	}
	if !strings.Contains(prompt, "## Available Tools") {
		t.Fatalf("prompt missing tool instructions: %q", prompt)
	}
	if !strings.Contains(prompt, "<|user|>\nHello\n</s>") {
		t.Fatalf("prompt missing user turn: %q", prompt)
	}
	if !strings.Contains(prompt, "Tool result for call_1:\nfile content") {
		t.Fatalf("prompt missing tool result: %q", prompt)
	}
	if !strings.HasSuffix(prompt, "<|assistant|>\n") {
		t.Fatalf("prompt should end with assistant cue, got %q", prompt)
	}
}

func TestPicoLMChat_AddsExpectedArgsAndParsesToolCalls(t *testing.T) {
	script := writeFakePicoLMScript(t)
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	outputFile := filepath.Join(t.TempDir(), "stdout.txt")
	requireWriteFile(t, outputFile, "Thinking...\n{\"tool_calls\":[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]}")

	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("OUTPUT_FILE", outputFile)
	t.Setenv("EXIT_CODE", "0")

	workspace := t.TempDir()
	p := NewPicoLMProvider(script, "models\\tinyllama.gguf", "chatml", 4, 256, workspace)
	resp, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "Read a file"}}, []ToolDefinition{
		{
			Type:     "function",
			Function: ToolFunctionDefinition{Name: "read_file"},
		},
	}, "picolm-local", map[string]any{"temperature": 0.3, "max_tokens": 128})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("ToolCalls = %#v, want read_file", resp.ToolCalls)
	}
	if resp.Content != "Thinking..." {
		t.Fatalf("Content = %q, want Thinking...", resp.Content)
	}

	args := readLines(t, argsFile)
	wantSubseq := []string{filepath.Join(workspace, "models\\tinyllama.gguf"), "--json", "-n", "128", "-t", "0.3", "-j", "4"}
	if !containsSequence(args, wantSubseq) {
		t.Fatalf("args = %#v, want subsequence %#v", args, wantSubseq)
	}
}

func TestPicoLMChat_ReportsProcessErrors(t *testing.T) {
	script := writeFakePicoLMScript(t)
	stderrFile := filepath.Join(t.TempDir(), "stderr.txt")
	requireWriteFile(t, stderrFile, "boom")

	t.Setenv("STDERR_FILE", stderrFile)
	t.Setenv("EXIT_CODE", "3")

	p := NewPicoLMProvider(script, "model.gguf", "chatml", 4, 256, t.TempDir())
	_, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "Hi"}}, nil, "picolm-local", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want stderr content", err)
	}
}

func requireWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func writeFakePicoLMScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "fake-picolm.cmd")
		script := "@echo off\r\n" +
			"setlocal\r\n" +
			"if not \"%ARGS_FILE%\"==\"\" (\r\n" +
			"  > \"%ARGS_FILE%\" (\r\n" +
			"    for %%a in (%*) do echo %%~a\r\n" +
			"  )\r\n" +
			")\r\n" +
			"if not \"%STDERR_FILE%\"==\"\" type \"%STDERR_FILE%\" 1>&2\r\n" +
			"if not \"%OUTPUT_FILE%\"==\"\" type \"%OUTPUT_FILE%\"\r\n" +
			"if \"%EXIT_CODE%\"==\"\" exit /b 0\r\n" +
			"exit /b %EXIT_CODE%\r\n"
		requireWriteFile(t, path, script)
		return path
	}

	path := filepath.Join(dir, "fake-picolm.sh")
	script := "#!/bin/sh\n" +
		"if [ -n \"$ARGS_FILE\" ]; then\n" +
		"  : > \"$ARGS_FILE\"\n" +
		"  for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> \"$ARGS_FILE\"; done\n" +
		"fi\n" +
		"if [ -n \"$STDERR_FILE\" ]; then cat \"$STDERR_FILE\" >&2; fi\n" +
		"if [ -n \"$OUTPUT_FILE\" ]; then cat \"$OUTPUT_FILE\"; fi\n" +
		"exit ${EXIT_CODE:-0}\n"
	requireWriteFile(t, path, script)
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("Chmod(%q): %v", path, err)
	}
	return path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func containsSequence(haystack, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if strings.TrimSpace(haystack[i+j]) != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
