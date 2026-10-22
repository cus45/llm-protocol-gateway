package gateway

import (
	"encoding/json"
	"testing"
)

// strict mode is a guarantee a client relies on: arguments must match the
// schema exactly. Dropping the flag on any hop silently downgraded the model to
// best-effort JSON, and the client then got arguments its parser rejected.

func TestOpenAIToolToClaudeAppliesStrictSchema(t *testing.T) {
	tool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":   "read_file",
			"strict": true,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":  map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer"},
				},
				"required": []any{"path"},
			},
		},
	}
	claudeTool := openAIToolToClaude(tool)
	schema, _ := claudeTool["input_schema"].(map[string]any)
	if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("additionalProperties not closed: %#v", schema)
	}
	required := asStringSlice(schema["required"])
	if len(required) != 2 {
		t.Fatalf("required = %v, want both properties", required)
	}
}

func TestOpenAIToolToClaudeLeavesNonStrictSchemaAlone(t *testing.T) {
	tool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "read_file",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		},
	}
	claudeTool := openAIToolToClaude(tool)
	schema, _ := claudeTool["input_schema"].(map[string]any)
	if _, exists := schema["additionalProperties"]; exists {
		t.Fatalf("non-strict schema was closed: %#v", schema)
	}
}

func TestClaudeToolToOpenAIReportsStrictFromClosedSchema(t *testing.T) {
	claudeTool := map[string]any{
		"name": "read_file",
		"input_schema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"path": map[string]any{"type": "string"}},
			"required":             []any{"path"},
		},
	}
	chatTool := claudeToolToOpenAI(claudeTool)
	functionValue, _ := chatTool["function"].(map[string]any)
	if strict, _ := functionValue["strict"].(bool); !strict {
		t.Fatalf("closed schema should come back as strict: %#v", chatTool)
	}
}

func TestResponsesToolToOpenAIChatKeepsStrict(t *testing.T) {
	tool := map[string]any{
		"type":   "function",
		"name":   "read_file",
		"strict": true,
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
	}
	chatTool := responsesToolToOpenAIChat(tool)
	if strict, _ := chatTool["strict"].(bool); !strict {
		t.Fatalf("strict lost at tool level: %#v", chatTool)
	}
	functionValue, _ := chatTool["function"].(map[string]any)
	if strict, _ := functionValue["strict"].(bool); !strict {
		t.Fatalf("strict lost at function level: %#v", chatTool)
	}
}

// End to end on the Claude path: a strict tool sent by a Responses client
// reaches the Claude request with a closed schema.
func TestResponsesToClaudeKeepsStrictTools(t *testing.T) {
	req := map[string]any{
		"model": "claude-sonnet-5",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "go"}}}},
		"tools": []any{map[string]any{
			"type":   "function",
			"name":   "read_file",
			"strict": true,
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
			},
		}},
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, _ := json.Marshal(claudeReq["tools"])
	if !containsAll(string(raw), `"additionalProperties":false`, `"required":["path"]`) {
		t.Fatalf("tools do not carry the closed schema: %s", raw)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		found := false
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
