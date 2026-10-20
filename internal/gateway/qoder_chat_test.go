package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

func qoderTestModelInfo(t *testing.T) qoderModelInfo {
	t.Helper()
	provider := domain.Provider{
		ID:       "qoder",
		AuthType: domain.AuthTypeQoderPAT,
		QoderPAT: &domain.QoderCredential{Models: json.RawMessage(qoderSampleListing)},
	}
	info, ok := qoderModelFor(provider, "ultimate")
	if !ok {
		t.Fatal("sample listing must carry Ultimate")
	}
	return info
}

func decodeQoderBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("Qoder's body is not valid JSON: %v", err)
	}
	return body
}

// Qoder's own system line comes first and the caller's follows it: the endpoint
// routes on that line, so a request built the OpenAI way — caller's system as
// the first message — is not the same request.
func TestQoderChatBodyPutsQodersSystemLineFirst(t *testing.T) {
	info := qoderTestModelInfo(t)
	raw, err := qoderChatBody("ultimate", []byte(`{
		"model":"ultimate",
		"messages":[
			{"role":"system","content":"Be terse."},
			{"role":"user","content":"hi"}
		]}`), info)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeQoderBody(t, raw)
	system, ok := body["system"].([]any)
	if !ok || len(system) != 1 {
		t.Fatalf("system blocks wrong: %v", body["system"])
	}
	text := system[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, qoderSys) {
		t.Fatalf("Qoder's line must come first, got %q", text)
	}
	if !strings.HasSuffix(text, "Be terse.") {
		t.Fatalf("the caller's system must ride with it, got %q", text)
	}
	messages := body["messages"].([]any)
	first := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("the body's first turn must be the system block, got %v", first["role"])
	}
	for _, item := range messages[1:] {
		if role := item.(map[string]any)["role"]; role == "system" {
			t.Fatal("the caller's system turn must not be repeated as a message")
		}
	}
	// Everything the endpoint keys on besides the messages.
	if body["agent_id"] != "agent_common" || body["task_id"] != "common" || body["session_type"] != "app" {
		t.Fatalf("agent identity wrong: %v", body)
	}
	if _, ok := body["model_config"].(map[string]any); !ok {
		t.Fatal("the model's own configuration has to travel with the request")
	}
	params := body["parameters"].(map[string]any)
	if params["max_tokens"] != float64(qoderDefaultMaxTokens) {
		t.Fatalf("max_tokens = %v, want the default the client asks with", params["max_tokens"])
	}
}

func TestQoderChatBodyCarriesToolsAndToolTurns(t *testing.T) {
	info := qoderTestModelInfo(t)
	raw, err := qoderChatBody("ultimate", []byte(`{
		"model":"ultimate",
		"messages":[
			{"role":"user","content":"read it"},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"the file says hi"},
			{"role":"user","content":[{"type":"text","text":"and now?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
		],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{}}}}]
	}`), info)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeQoderBody(t, raw)
	tools := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools wrong: %v", body["tools"])
	}
	function := tools[0].(map[string]any)["function"].(map[string]any)
	// Qoder validates the description as a string while the OpenAI spec makes it
	// optional and clients omit it — Claude Code's web_search tool is one.
	if _, ok := function["description"]; !ok {
		t.Fatal("a missing description must still be sent as an empty string")
	}
	if function["description"] != "" {
		t.Fatalf("description = %v", function["description"])
	}

	messages := body["messages"].([]any)
	var assistant, toolTurn, lastUser map[string]any
	for _, item := range messages {
		message := item.(map[string]any)
		switch message["role"] {
		case "assistant":
			assistant = message
		case "tool":
			toolTurn = message
		case "user":
			lastUser = message
		}
	}
	if assistant == nil {
		t.Fatal("the assistant turn with the call was dropped")
	}
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("tool call not carried: %v", assistant["tool_calls"])
	}
	if toolTurn == nil || toolTurn["tool_call_id"] != "call_1" {
		t.Fatalf("the result must answer its call by id, got %v", toolTurn)
	}
	if _, ok := toolTurn["content"].(string); !ok {
		t.Fatalf("a tool turn holds text only, got %T", toolTurn["content"])
	}
	// A user turn's image survives as an image_url block.
	blocks := lastUser["content"].([]any)
	var sawImage bool
	for _, block := range blocks {
		if item := block.(map[string]any); item["type"] == "image_url" {
			sawImage = true
		}
	}
	if !sawImage {
		t.Fatalf("image dropped: %v", lastUser["content"])
	}
}

// A client that says tools must not be used still gets no tools: Qoder answers a
// request that offers them with a call whatever the choice said.
func TestQoderChatBodyDropsToolsWhenChoiceIsNone(t *testing.T) {
	info := qoderTestModelInfo(t)
	raw, err := qoderChatBody("ultimate", []byte(`{
		"model":"ultimate",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"read_file","description":"read","parameters":{}}}],
		"tool_choice":"none"}`), info)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeQoderBody(t, raw)
	if _, ok := body["tools"]; ok {
		t.Fatalf("tools must be taken off the request: %v", body["tools"])
	}
}

func TestQoderChatBodyAsksForRequiredToolChoiceInWords(t *testing.T) {
	info := qoderTestModelInfo(t)
	raw, err := qoderChatBody("ultimate", []byte(`{
		"model":"ultimate",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"read_file","description":"read","parameters":{}}}],
		"tool_choice":"required"}`), info)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeQoderBody(t, raw)
	text := body["system"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "You must call an available function") {
		t.Fatalf("the vendor's API is told the answer's format in words, got %q", text)
	}
}

// The level asked for has to become one the model actually offers, and thinking
// has to be turned off only for a model that can.
func TestQoderEffortSelection(t *testing.T) {
	offering := qoderTestModelInfo(t) // low, high (default), max; can be turned off
	alwaysThinking := qoderModelInfo{Thinks: true, AlwaysThinks: true, Efforts: []string{"low", "medium", "high"}}
	noThinking := qoderModelInfo{}
	cases := []struct {
		name         string
		asked        string
		info         qoderModelInfo
		wantThinking bool
		wantEffort   string
	}{
		{"nothing asked uses the model's default", "", offering, true, "high"},
		{"an offered level is passed through", "max", offering, true, "max"},
		{"nearest level, higher on a tie", "xhigh", offering, true, "max"},
		{"a model without thinking never thinks", "high", noThinking, false, ""},
		{"asking none on a model that can stop", "none", offering, false, ""},
		{"a model that always thinks thinks at its lowest", "none", alwaysThinking, true, "low"},
		{"an unnamed level falls back to the default", "turbo", offering, true, "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			thinking, effort := qoderEffort(tc.asked, tc.info)
			if thinking != tc.wantThinking || effort != tc.wantEffort {
				t.Fatalf("qoderEffort(%q) = (%v, %q), want (%v, %q)", tc.asked, thinking, effort, tc.wantThinking, tc.wantEffort)
			}
		})
	}
}

// A bigger window can cost more credits, so it is asked for only when the
// conversation needs it — and the reply's own budget counts into it.
func TestQoderWindowFor(t *testing.T) {
	model := qoderModelInfo{MaxInputTokens: 180000, Windows: []int{200000, 400000, 1000000}, DefaultWindow: 200000}
	small, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	var chat map[string]any
	if err := json.Unmarshal(small, &chat); err != nil {
		t.Fatal(err)
	}
	if got := qoderWindowFor(chat, model, 32000); got != 200000 {
		t.Fatalf("a short conversation should keep the default window, got %d", got)
	}
	// The same conversation asking for a long reply needs the next window up:
	// a window holds the prompt and the reply together.
	if got := qoderWindowFor(chat, model, 250000); got != 400000 {
		t.Fatalf("a 250K reply does not fit the default window and should raise it, got %d", got)
	}
	huge := map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 3_000_000)}}}
	if got := qoderWindowFor(huge, model, 32000); got != 1000000 {
		t.Fatalf("an oversized conversation should take the largest window, got %d", got)
	}
	// A model that offers no windows at all is sent none: it is asked with what
	// it says it takes.
	if got := qoderWindowFor(chat, qoderModelInfo{MaxInputTokens: 180000}, 32000); got != 180000 {
		t.Fatalf("no windows should fall back to max_input_tokens, got %d", got)
	}
}

func TestQoderAskedEffortFromEitherDialect(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"reasoning_effort":"high"}`, "high"},
		{`{"thinking":{"type":"disabled"}}`, "none"},
		{`{"thinking":{"type":"enabled"}}`, ""},
		{`{}`, ""},
	}
	for _, tc := range cases {
		var chat map[string]any
		if err := json.Unmarshal([]byte(tc.body), &chat); err != nil {
			t.Fatal(err)
		}
		if got := qoderAskedEffort(chat); got != tc.want {
			t.Fatalf("qoderAskedEffort(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
