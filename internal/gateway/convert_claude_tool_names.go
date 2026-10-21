package gateway

import (
	"bytes"
	"encoding/json"
)

// Naming the model's calls back, on an Anthropic Messages body.
//
// The Claude OAuth cloak renames third-party tool names into Claude Code's
// TitleCase on the way out (bash → Bash, get_weather → GetWeather), and a
// client only dispatches the tool names it declared. Everything the cloak
// renames is therefore renamed back here before the answer leaves the gateway,
// on a whole JSON message and on the SSE event that opens a tool_use block.

// renameClaudeToolUseNamesJSON renames the tool_use blocks of one Anthropic
// Messages JSON body.
func renameClaudeToolUseNamesJSON(raw []byte, names map[string]string) []byte {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	if !renameClaudeToolUseNames(value, names) {
		return raw
	}
	if encoded := encodeJSONWithoutEscaping(value); encoded != nil {
		return encoded
	}
	return raw
}

// renameClaudeToolUseNamesLine does the same on one SSE line: only a
// content_block_start carries a tool name, and only its data line holds it, so
// the event framing is never touched.
func renameClaudeToolUseNamesLine(line []byte, names map[string]string) []byte {
	body := bytes.TrimSuffix(line, []byte("\n"))
	body = bytes.TrimSuffix(body, []byte("\r"))
	data, ok := bytes.CutPrefix(body, []byte("data:"))
	if !ok {
		return line
	}
	payload := bytes.TrimSpace(data)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	var value any
	if json.Unmarshal(payload, &value) != nil || !renameClaudeToolUseNames(value, names) {
		return line
	}
	encoded := encodeJSONWithoutEscaping(value)
	if encoded == nil {
		return line
	}
	out := append([]byte("data: "), encoded...)
	if bytes.HasSuffix(line, []byte("\n")) {
		out = append(out, '\n')
	}
	return out
}

// renameClaudeToolUseNames renames a tool_use block's name in a decoded
// Anthropic value: a whole message's content[], or one content_block_start's
// content_block.
func renameClaudeToolUseNames(value any, names map[string]string) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}
	changed := false
	if block, ok := root["content_block"].(map[string]any); ok {
		if renameClaudeToolUseBlock(block, names) {
			changed = true
		}
	}
	if blocks, ok := root["content"].([]any); ok {
		for _, item := range blocks {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if renameClaudeToolUseBlock(block, names) {
				changed = true
			}
		}
	}
	return changed
}

func renameClaudeToolUseBlock(block map[string]any, names map[string]string) bool {
	if stringValue(block["type"]) != "tool_use" {
		return false
	}
	name := stringValue(block["name"])
	declared, found := names[name]
	if !found || declared == "" || declared == name {
		return false
	}
	block["name"] = declared
	return true
}

// claudeRequestToolAliases is the OAuth cloak's alias map (cloaked name → the
// name the client declared) for one client body, empty when the request carries
// no tool name the cloak would rewrite — which is the case for every Claude
// Code request, since its own names are already TitleCase or mcp__-prefixed.
func claudeRequestToolAliases(body []byte) map[string]string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	return claudeOAuthToolNameAliases(payload)
}
