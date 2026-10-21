package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A Claude Code conversation replayed to a Chat provider kept its tool names
// only in tools[]; every tool_use in the history was renamed through the OAuth
// cloak's reverse map, so "Bash" arrived as "bash". The model then copied that
// spelling when it called the tool and Claude Code answered "No such tool
// available: bash. Tool names are case-sensitive: call Bash instead."
func TestClaudeToChatKeepsDeclaredToolNameCase(t *testing.T) {
	claudeReq := map[string]any{
		"model": "deepseek-v4-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "list files"},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
					map[string]any{"type": "tool_use", "id": "toolu_2", "name": "Read", "input": map[string]any{"file_path": "a.txt"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": "hello"},
				},
			},
		},
		"tools": []any{
			map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}},
			map[string]any{"name": "Read", "description": "read", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}},
		},
	}
	out, err := claudeRequestToOpenAIChat(claudeReq, "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Bash", "Read"}
	got := make([]string, 0, 2)
	for _, item := range out["messages"].([]any) {
		message := item.(map[string]any)
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			got = append(got, stringValue(rawCall.(map[string]any)["function"].(map[string]any)["name"]))
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("history tool_calls names = %v, want %v", got, want)
	}
}

// A client whose tools[] really are lowercase (Cursor-style) still gets its
// lowercase names back in the history.
func TestClaudeToChatRestoresLowercaseClientNames(t *testing.T) {
	claudeReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{}},
				},
			},
		},
		"tools": []any{map[string]any{"name": "bash"}},
	}
	out, err := claudeRequestToOpenAIChat(claudeReq, "deepseek-chat")
	if err != nil {
		t.Fatal(err)
	}
	message := out["messages"].([]any)[0].(map[string]any)
	name := stringValue(message["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"])
	if name != "bash" {
		t.Fatalf("history tool_calls name = %q, want bash", name)
	}
}

// restoreDeclaredToolName puts back a case the model dropped, and leaves an
// ambiguous or unknown name alone.
func TestRestoreDeclaredToolName(t *testing.T) {
	declared := map[string]struct{}{"Bash": {}, "Read": {}, "TodoWrite": {}}
	cases := map[string]string{
		"Bash":      "Bash",
		"bash":      "Bash",
		"READ":      "Read",
		"todowrite": "TodoWrite",
		"grep":      "grep",
		"":          "",
	}
	for in, want := range cases {
		if got := restoreDeclaredToolName(in, declared); got != want {
			t.Fatalf("restoreDeclaredToolName(%q) = %q, want %q", in, got, want)
		}
	}
	ambiguous := map[string]struct{}{"Read": {}, "read": {}}
	if got := restoreDeclaredToolName("READ", ambiguous); got != "READ" {
		t.Fatalf("ambiguous case should be left alone, got %q", got)
	}
}

// The non-stream answer goes through the same restore, so a model that answers
// "bash" still reaches Claude Code as "Bash".
func TestOpenAIChatResponseToClaudeRestoresDeclaredCase(t *testing.T) {
	openAIResp := []byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}}]}`)
	body, _, err := openAIChatResponseToClaude(openAIResp, "deepseek-chat", map[string]struct{}{"Bash": {}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"name":"Bash"`) {
		t.Fatalf("expected Bash in %s", body)
	}
}

// Same for a streamed answer.
func TestStreamOpenAIChatToClaudeRestoresDeclaredCase(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"ls\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"tool_calls","delta":{}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	recorder := httptest.NewRecorder()
	if _, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(sse), "deepseek-chat", map[string]struct{}{"Bash": {}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), `"name":"Bash"`) {
		t.Fatalf("expected Bash in %s", recorder.Body.String())
	}
}

// A Claude Code history reaching a WorkBuddy chat is written as Chat tool
// calls. Every call is spelled as the chat's tools[] spell it, so a model that
// reads the history back does not copy a case the client never sent.
func TestWorkBuddyHistoryCallsWearDeclaredToolNameCase(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "list files"},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": map[string]any{"command": "ls"}},
					map[string]any{"type": "tool_use", "id": "toolu_2", "name": "Read", "input": map[string]any{"file_path": "a.txt"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": "hello"},
				},
			},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "Read"}},
		},
	}
	messages, changed := workBuddyToolBlocksToCalls(payload["messages"].([]any), chatDeclaredToolNames(payload))
	if !changed {
		t.Fatal("expected the tool blocks to be rewritten as Chat calls")
	}
	got := make([]string, 0, 2)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, rawCall := range asMapSlice(message["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			function, _ := call["function"].(map[string]any)
			got = append(got, stringValue(function["name"]))
		}
	}
	if strings.Join(got, ",") != "Bash,Read" {
		t.Fatalf("history calls = %v, want Bash,Read", got)
	}
}
