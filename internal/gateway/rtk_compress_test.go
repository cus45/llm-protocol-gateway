package gateway

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newFakeRTKCompressor(t *testing.T, output string, calls *int32) *rtkCompressor {
	t.Helper()
	return &rtkCompressor{
		path:      "fake-rtk",
		version:   "rtk test",
		checkedAt: time.Now(),
		cache:     make(map[[32]byte]string),
		sem:       make(chan struct{}, rtkMaxConcurrent),
		runner: func(context.Context, string, string, []byte) ([]byte, error) {
			if calls != nil {
				atomic.AddInt32(calls, 1)
			}
			return []byte(output), nil
		},
	}
}

func longToolOutput() string {
	return strings.Repeat("test line: ok\n", 120)
}

func TestRTKCompressClaudeToolResultIsCopyOnWrite(t *testing.T) {
	t.Parallel()
	original := longToolOutput()
	request := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "tool_use", "id": "tool-1", "name": "Bash",
						"input": map[string]any{"command": "git status --short"},
					},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": original},
				},
			},
		},
	}

	compressor := newFakeRTKCompressor(t, "compressed output", nil)
	compressed, stats := compressor.compressRequest(request, "claude")

	if stats.Commands != 1 {
		t.Fatalf("commands = %d, want 1", stats.Commands)
	}
	if stats.CompressedBytes >= stats.OriginalBytes {
		t.Fatalf("compressed bytes = %d, original = %d", stats.CompressedBytes, stats.OriginalBytes)
	}
	messages := compressed["messages"].([]any)
	block := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["content"] != "compressed output" {
		t.Fatalf("compressed content = %q", block["content"])
	}
	originalBlock := request["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if originalBlock["content"] != original {
		t.Fatal("original request was mutated")
	}
}

func TestRTKCompressSkipsUnsafeCommands(t *testing.T) {
	t.Parallel()
	cases := []string{
		"rm -rf /tmp/foo",
		"git status && echo done",
		"cd /repo && wc -l file",
		"git status | sort",
		"git diff > out.txt",
		"grep foo . 2>&1",
		"$(which git) status",
	}
	for _, command := range cases {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			request := map[string]any{
				"messages": []any{
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "tool_use", "id": "t", "name": "Bash", "input": map[string]any{"command": command}},
					}},
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "t", "content": longToolOutput()},
					}},
				},
			}
			compressor := newFakeRTKCompressor(t, "compressed", nil)
			got, stats := compressor.compressRequest(request, "claude")
			if stats.Commands != 0 {
				t.Fatalf("commands = %d, want 0 for %q", stats.Commands, command)
			}
			block := got["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
			if block["content"] != longToolOutput() {
				t.Fatal("unsafe command output was compressed")
			}
		})
	}
}

func TestRTKCompressSkipsErrorResults(t *testing.T) {
	t.Parallel()
	request := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t", "name": "Bash", "input": map[string]any{"command": "git status"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t", "is_error": true, "content": longToolOutput()},
			}},
		},
	}
	compressor := newFakeRTKCompressor(t, "compressed", nil)
	_, stats := compressor.compressRequest(request, "claude")
	if stats.Commands != 0 {
		t.Fatalf("commands = %d, want 0", stats.Commands)
	}
}

func TestRTKCompressOpenAIChatToolResult(t *testing.T) {
	t.Parallel()
	original := longToolOutput()
	request := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{
					"id": "call-1",
					"function": map[string]any{
						"name": "Bash", "arguments": `{"command":"go test ./..."}`,
					},
				},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": original},
		},
	}
	compressor := newFakeRTKCompressor(t, "filtered test output", nil)
	compressed, stats := compressor.compressRequest(request, "openai_chat")
	if stats.Commands != 1 {
		t.Fatalf("commands = %d, want 1", stats.Commands)
	}
	message := compressed["messages"].([]any)[1].(map[string]any)
	if message["content"] != "filtered test output" {
		t.Fatalf("content = %q", message["content"])
	}
	originalMessage := request["messages"].([]any)[1].(map[string]any)
	if originalMessage["content"] != original {
		t.Fatal("original request was mutated")
	}
}

func TestRTKCompressResponsesToolResult(t *testing.T) {
	t.Parallel()
	original := longToolOutput()
	request := map[string]any{
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call-1", "name": "Bash", "arguments": `{"command":"rg TODO"}`},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": original},
		},
	}
	compressor := newFakeRTKCompressor(t, "filtered rg output", nil)
	compressed, stats := compressor.compressRequest(request, "openai_responses")
	if stats.Commands != 1 {
		t.Fatalf("commands = %d, want 1", stats.Commands)
	}
	item := compressed["input"].([]any)[1].(map[string]any)
	if item["output"] != "filtered rg output" {
		t.Fatalf("output = %q", item["output"])
	}
}

func TestRTKCompressCachesIdenticalOutput(t *testing.T) {
	t.Parallel()
	var calls int32
	compressor := newFakeRTKCompressor(t, "compressed", &calls)
	text := longToolOutput()
	if _, ok := compressor.compressText("git-status", text); !ok {
		t.Fatal("first compression failed")
	}
	if _, ok := compressor.compressText("git-status", text); !ok {
		t.Fatal("cached compression failed")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("runner calls = %d, want 1", got)
	}
}

func TestRTKCompressSkipsShortOutput(t *testing.T) {
	t.Parallel()
	compressor := newFakeRTKCompressor(t, "compressed", nil)
	if _, ok := compressor.compressText("git-status", "short output"); ok {
		t.Fatal("short output should not be compressed")
	}
}

func TestRTKFilterForCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		command string
		filter  string
		ok      bool
	}{
		{"git status", "git-status", true},
		{"git diff --stat", "git-diff", true},
		{"git log --oneline -10", "git-log", true},
		{"rg TODO internal", "rg", true},
		{"grep -R foo .", "grep", true},
		{"find . -name '*.go'", "find", true},
		{"go test ./...", "go-test", true},
		{"go build ./...", "go-build", true},
		{"python3 -m pytest -q", "pytest", true},
		{"npx vitest run", "vitest", true},
		{"cargo test --quiet", "cargo-test", true},
		{"ruff check .", "ruff-check", true},
		{"git status | head", "git-status", true},
		{"git status | grep foo", "", false},
		{"grep -rliE \"hbase|redis\" src", "grep", true},
		{"cd /repo && git log --stat -n 200", "git-log", true},
		{"cd /repo; git status", "git-status", true},
		{"cd /repo && find . -name '*.go' | head -100", "find", true},
		{"grep -rn foo . 2>/dev/null | head -50", "grep", true},
		{"grep -rn foo . 2> /dev/null | tail -20", "grep", true},
		{"git diff HEAD~5 2>>/dev/null", "git-diff", true},
		{"grep -rn foo . 2>&1", "", false},
		{"grep -rn foo . > out.txt", "", false},
		{"grep -rn foo . &> /dev/null", "", false},
		{"cat file.go | grep foo", "", false},
		{"git log | sort", "", false},
		{"cd /repo && wc -l file", "", false},
		{"cd /repo && git log && rm -rf build", "", false},
		{"grep $(cat pattern) file", "", false},
		{"FOO=1 git status", "git-status", true},
		{"sudo git status", "git-status", true},
		{"rm -rf build", "", false},
		{"echo hi", "", false},
	}
	for _, tc := range cases {
		filter, ok := rtkFilterForCommand(tc.command)
		if ok != tc.ok || filter != tc.filter {
			t.Errorf("rtkFilterForCommand(%q) = (%q, %v), want (%q, %v)", tc.command, filter, ok, tc.filter, tc.ok)
		}
	}
}

func TestRTKCandidatePathsUsesExplicitOverride(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "rtk-custom.exe")
	t.Setenv("GATEWAY_RTK_PATH", explicit)
	paths := rtkCandidatePaths()
	if len(paths) != 1 || paths[0] != explicit {
		t.Fatalf("candidates = %v, want only %q", paths, explicit)
	}
}

func TestRTKCandidatePathsIncludesWinGetLinks(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only install layout")
	}
	localAppData := filepath.Join(t.TempDir(), "AppData", "Local")
	t.Setenv("GATEWAY_RTK_PATH", "")
	t.Setenv("LOCALAPPDATA", localAppData)
	want := filepath.Join(localAppData, "Microsoft", "WinGet", "Links", "rtk.exe")
	for _, candidate := range rtkCandidatePaths() {
		if candidate == want {
			return
		}
	}
	t.Fatalf("candidates = %v, want %q", rtkCandidatePaths(), want)
}
