package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
)

// ---- tools WorkBuddy takes ----------------------------------------------------
//
// WorkBuddy AI answers 400 code 11133 "Invalid request parameters" to a chat
// whose tools it cannot pass on to the model (DeepSeek, Kimi, and some others
// each turn away different shapes), and 400 code 11153 "the conversation must
// end with a user message" to a chat whose tool results are followed by the
// user's message. Both are magpie's findings; this file carries its repairs:
// fitTools mends a tool's name and schema into a shape every model takes, its
// answer is named back to the client's own names, and replyAfterTools puts an
// empty assistant reply between the results and the user's message.

var workBuddyToolNameOK = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// workBuddySafeToolName is name with each character WorkBuddy refuses as _,
// kept apart from the names in taken.
func workBuddySafeToolName(name string, taken map[string]bool) string {
	var out strings.Builder
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '_', char == '-':
			out.WriteRune(char)
		default:
			out.WriteByte('_')
		}
	}
	base := out.String()
	if base == "" {
		base = "tool"
	}
	safe := base
	for i := 2; taken[safe]; i++ {
		safe = fmt.Sprintf("%s_%d", base, i)
	}
	taken[safe] = true
	return safe
}

// fitWorkBuddyTools mends payload's function tools in place: their names and
// their parameters. It returns the names it gave, each to the name the client
// knows, so the model's calls can be named back. A call in the chat's history
// is named as its tool now is.
func fitWorkBuddyTools(payload map[string]any) (map[string]string, bool) {
	tools, _ := payload["tools"].([]any)
	names := map[string]string{}
	if len(tools) == 0 {
		return names, false
	}
	changed := false
	taken := map[string]bool{}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if function, ok := tool["function"].(map[string]any); ok {
			if name, _ := function["name"].(string); workBuddyToolNameOK.MatchString(name) {
				taken[name] = true
			}
		}
	}
	theirs := map[string]string{}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := function["name"].(string); !workBuddyToolNameOK.MatchString(name) {
			safe := workBuddySafeToolName(name, taken)
			names[safe] = name
			theirs[name] = safe
			function["name"] = safe
			changed = true
		}
		parameters, exists := function["parameters"]
		if !exists {
			continue
		}
		schema, ok := parameters.(map[string]any)
		if !ok {
			function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			changed = true
			continue
		}
		if fitWorkBuddySchema(schema, 0) {
			changed = true
		}
		if fitWorkBuddyShape(schema, 0) {
			changed = true
		}
		if fitWorkBuddyObjectRoot(schema, schema, 0) {
			changed = true
		}
	}
	if len(theirs) == 0 {
		return names, changed
	}
	if messages, ok := payload["messages"].([]any); ok {
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					function, ok := call["function"].(map[string]any)
					if !ok {
						continue
					}
					if name, _ := function["name"].(string); theirs[name] != "" {
						function["name"] = theirs[name]
						changed = true
					}
				}
			}
			if role, _ := message["role"].(string); role == "tool" {
				if name, _ := message["name"].(string); theirs[name] != "" {
					message["name"] = theirs[name]
					changed = true
				}
			}
		}
	}
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		if function, ok := choice["function"].(map[string]any); ok {
			if name, _ := function["name"].(string); theirs[name] != "" {
				function["name"] = theirs[name]
				changed = true
			}
		}
	}
	return names, changed
}

// fitWorkBuddySchema mends a schema in place, at every depth: a pattern's \0
// (the NUL character), which DeepSeek's validator says is not a "regex", is
// \u0000, which matches the same; a tuple's items (a list) become one schema
// any of them fits. It reports whether it changed anything.
func fitWorkBuddySchema(schema any, depth int) bool {
	if depth > 64 {
		return false
	}
	switch typed := schema.(type) {
	case []any:
		changed := false
		for _, item := range typed {
			if fitWorkBuddySchema(item, depth+1) {
				changed = true
			}
		}
		return changed
	case map[string]any:
		changed := false
		if pattern, ok := typed["pattern"].(string); ok && strings.Contains(pattern, `\0`) {
			if fitted := fitWorkBuddyPattern(pattern); fitted != pattern {
				typed["pattern"] = fitted
				changed = true
			}
		}
		if items, ok := typed["items"].([]any); ok {
			list := make([]any, 0, len(items))
			for _, item := range items {
				if _, isSchema := item.(map[string]any); isSchema {
					list = append(list, item)
				}
			}
			switch len(list) {
			case 0:
				typed["items"] = map[string]any{}
			case 1:
				typed["items"] = list[0]
			default:
				typed["items"] = map[string]any{"anyOf": list}
			}
			delete(typed, "additionalItems")
			changed = true
		}
		for _, item := range typed {
			if fitWorkBuddySchema(item, depth+1) {
				changed = true
			}
		}
		return changed
	}
	return false
}

// fitWorkBuddyPattern replaces a regex's \0 with \u0000, stepping over each \\
// so that a \\0 is left, and over \01 (an octal escape), also left.
func fitWorkBuddyPattern(pattern string) string {
	var out strings.Builder
	out.Grow(len(pattern))
	for i := 0; i < len(pattern); i++ {
		char := pattern[i]
		if char != '\\' {
			out.WriteByte(char)
			continue
		}
		if i+1 < len(pattern) && pattern[i+1] == '\\' {
			out.WriteString(`\\`)
			i++
			continue
		}
		if i+1 < len(pattern) && pattern[i+1] == '0' &&
			!(i+2 < len(pattern) && pattern[i+2] >= '0' && pattern[i+2] <= '9') {
			out.WriteString(`\u0000`)
			i++
			continue
		}
		out.WriteByte(char)
	}
	return out.String()
}

// workBuddySchemaTypes are the types WorkBuddy's models take.
var workBuddySchemaTypes = map[string]bool{
	"string": true, "number": true, "integer": true,
	"boolean": true, "object": true, "array": true, "null": true,
}

// The JSON Schema keywords whose value is a schema, a list of schemas, or a
// map of names to schemas. additionalProperties and unevaluatedProperties are
// left as they are when true or false.
var (
	workBuddySubOne = []string{"items", "additionalItems", "unevaluatedItems", "contains",
		"not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"}
	workBuddySubList = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
	workBuddySubMap  = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	workBuddyBoolOK  = map[string]bool{"additionalProperties": true, "unevaluatedProperties": true}
)

// fitWorkBuddyShape mends a schema and each schema under it into the shape
// WorkBuddy's models take: a known lowercase type, a list-valued required of
// strings, a typed enum, and objects where a schema is true or false. A name
// under properties is never read as a keyword. It reports whether it changed
// anything.
func fitWorkBuddyShape(schema any, depth int) bool {
	if depth > 64 {
		return false
	}
	object, ok := schema.(map[string]any)
	if !ok {
		return false
	}
	changed := false
	if raw, exists := object["type"]; exists {
		listed := []string{}
		switch typed := raw.(type) {
		case string:
			listed = []string{typed}
		case []any:
			for _, item := range typed {
				if name, ok := item.(string); ok {
					listed = append(listed, name)
				}
			}
		}
		unique := make([]string, 0, len(listed))
		seen := map[string]bool{}
		for _, name := range listed {
			name = strings.ToLower(name)
			if workBuddySchemaTypes[name] && !seen[name] {
				seen[name] = true
				unique = append(unique, name)
			}
		}
		switch len(unique) {
		case 0:
			delete(object, "type")
			changed = true
		case 1:
			if name, isString := raw.(string); !isString || name != unique[0] {
				object["type"] = unique[0]
				changed = true
			}
		default:
			if !workBuddySameJSON(workBuddyStringsToAny(unique), raw) {
				object["type"] = workBuddyStringsToAny(unique)
				changed = true
			}
		}
	}
	if raw, exists := object["required"]; exists {
		if list, ok := raw.([]any); !ok {
			delete(object, "required")
			changed = true
		} else {
			kept := make([]any, 0, len(list))
			for _, item := range list {
				if name, ok := item.(string); ok {
					kept = append(kept, name)
				}
			}
			if len(kept) != len(list) {
				object["required"] = kept
				changed = true
			}
		}
	}
	if fitWorkBuddyEnum(object) {
		changed = true
	}
	for _, key := range workBuddySubOne {
		raw, exists := object[key]
		if !exists {
			continue
		}
		value, mended := workBuddySubSchema(raw, key)
		if mended {
			object[key] = value
			changed = true
		}
		if fitWorkBuddyShape(value, depth+1) {
			changed = true
		}
	}
	for _, key := range workBuddySubList {
		list, ok := object[key].([]any)
		if !ok {
			continue
		}
		for index, raw := range list {
			value, mended := workBuddySubSchema(raw, key)
			if mended {
				list[index] = value
				changed = true
			}
			if fitWorkBuddyShape(value, depth+1) {
				changed = true
			}
		}
	}
	for _, key := range workBuddySubMap {
		entries, ok := object[key].(map[string]any)
		if !ok {
			continue
		}
		for name, raw := range entries {
			value, mended := workBuddySubSchema(raw, key)
			if mended {
				entries[name] = value
				changed = true
			}
			if fitWorkBuddyShape(value, depth+1) {
				changed = true
			}
		}
	}
	return changed
}

// workBuddySubSchema is a schema found under key, as the models take it:
// true is {}, false is {"not": {}}.
func workBuddySubSchema(value any, key string) (any, bool) {
	boolean, ok := value.(bool)
	if !ok || workBuddyBoolOK[key] {
		return value, false
	}
	if boolean {
		return map[string]any{}, true
	}
	return map[string]any{"not": map[string]any{}}, true
}

// fitWorkBuddyEnum gives an enum the type its values have, or that and
// "null"; values of several types become an anyOf of an enum for each. It
// reports whether it changed anything.
func fitWorkBuddyEnum(object map[string]any) bool {
	raw, exists := object["enum"]
	if !exists {
		return false
	}
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		delete(object, "enum")
		return true
	}
	listed := []string{}
	switch typed := object["type"].(type) {
	case string:
		listed = []string{typed}
	case []any:
		for _, item := range typed {
			if name, ok := item.(string); ok {
				listed = append(listed, name)
			}
		}
	}
	typed := 0
	for _, name := range listed {
		if name != "null" {
			typed++
		}
	}
	if typed == 1 {
		return false
	}
	order := make([]string, 0, 4)
	groups := map[string][]any{}
	for _, value := range values {
		kind := workBuddyJSONType(value)
		if _, ok := groups[kind]; !ok {
			order = append(order, kind)
		}
		groups[kind] = append(groups[kind], value)
	}
	if numbers, ok := groups["number"]; ok {
		if integers, ok := groups["integer"]; ok {
			groups["number"] = append(numbers, integers...)
			delete(groups, "integer")
			order = workBuddyWithout(order, "integer")
		}
	}
	hasNull := false
	if _, ok := groups["null"]; ok {
		hasNull = true
		delete(groups, "null")
		order = workBuddyWithout(order, "null")
	}
	if len(order) <= 1 {
		var want any
		switch {
		case len(order) == 0:
			want = "null"
		case hasNull:
			want = []any{order[0], "null"}
		default:
			want = order[0]
		}
		if workBuddySameJSON(want, object["type"]) {
			return false
		}
		object["type"] = want
		return true
	}
	split := make([]any, 0, len(order)+1)
	for _, kind := range order {
		split = append(split, map[string]any{"type": kind, "enum": groups[kind]})
	}
	if hasNull {
		split = append(split, map[string]any{"type": "null"})
	}
	delete(object, "enum")
	delete(object, "type")
	if _, ok := object["anyOf"].([]any); ok {
		allOf, _ := object["allOf"].([]any)
		object["allOf"] = append(allOf, map[string]any{"anyOf": split})
	} else {
		object["anyOf"] = split
	}
	return true
}

// workBuddyJSONType is the JSON Schema type of an enum's value.
func workBuddyJSONType(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case json.Number:
		if strings.ContainsAny(typed.String(), ".eE") {
			return "number"
		}
		return "integer"
	case float64:
		if typed == math.Trunc(typed) {
			return "integer"
		}
		return "number"
	case int, int32, int64, uint, uint32, uint64:
		return "integer"
	case bool:
		return "boolean"
	case string:
		return "string"
	case map[string]any:
		return "object"
	}
	return "string"
}

// fitWorkBuddyObjectRoot makes a tool's parameters an object at the root:
// allOf's branches are merged whole; of anyOf's and oneOf's object branches
// the properties are merged and a field each requires stays required. A root
// with no type, or an empty one, is an object. It reports whether it changed
// anything.
func fitWorkBuddyObjectRoot(schema map[string]any, root map[string]any, depth int) bool {
	unions := make([]string, 0, 3)
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := schema[key].([]any); ok {
			unions = append(unions, key)
		}
	}
	if schema["type"] == "object" && len(unions) == 0 {
		return false
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
	}
	required := []any{}
	if list, ok := schema["required"].([]any); ok {
		required = append(required, list...)
	}
	if branches, ok := schema["allOf"].([]any); ok {
		for _, raw := range branches {
			branch, ok := workBuddyLocalRef(root, raw).(map[string]any)
			if !ok {
				continue
			}
			if depth < 8 && workBuddyHasUnion(branch) {
				branch, _ = workBuddyCloneJSON(branch).(map[string]any)
				fitWorkBuddyObjectRoot(branch, root, depth+1)
			}
			if more, ok := branch["properties"].(map[string]any); ok {
				for name, value := range more {
					if _, exists := properties[name]; !exists {
						properties[name] = value
					}
				}
			}
			if list, ok := branch["required"].([]any); ok {
				required = append(required, list...)
			}
		}
	}
	branches := make([]map[string]any, 0, 4)
	for _, key := range []string{"anyOf", "oneOf"} {
		list, _ := schema[key].([]any)
		for _, raw := range list {
			branch, ok := workBuddyLocalRef(root, raw).(map[string]any)
			if !ok {
				continue
			}
			if depth < 8 && workBuddyHasUnion(branch) {
				branch, _ = workBuddyCloneJSON(branch).(map[string]any)
				fitWorkBuddyObjectRoot(branch, root, depth+1)
			}
			if branch["type"] != "object" {
				if _, hasProperties := branch["properties"]; !hasProperties {
					continue
				}
			}
			branches = append(branches, branch)
		}
	}
	from := map[string][]any{}
	order := []string{}
	for _, branch := range branches {
		more, _ := branch["properties"].(map[string]any)
		for name, value := range more {
			if _, exists := properties[name]; exists {
				continue
			}
			seen := from[name]
			duplicate := false
			for _, known := range seen {
				if workBuddySameJSON(known, value) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				if _, exists := from[name]; !exists {
					order = append(order, name)
				}
				from[name] = append(seen, value)
			}
		}
	}
	for _, name := range order {
		values := from[name]
		if len(values) == 1 {
			properties[name] = values[0]
		} else {
			properties[name] = map[string]any{"anyOf": values}
		}
	}
	if len(branches) > 0 {
		if first, ok := branches[0]["properties"].(map[string]any); ok {
			for name := range first {
				every := true
				for _, branch := range branches {
					list, ok := branch["required"].([]any)
					if !ok {
						every = false
						break
					}
					found := false
					for _, item := range list {
						if text, ok := item.(string); ok && text == name {
							found = true
							break
						}
					}
					if !found {
						every = false
						break
					}
				}
				if every {
					required = append(required, name)
				}
			}
		}
	}
	for _, key := range unions {
		delete(schema, key)
	}
	schema["type"] = "object"
	schema["properties"] = properties
	kept := []string{}
	seen := map[string]bool{}
	for _, item := range required {
		name, ok := item.(string)
		if !ok || seen[name] {
			continue
		}
		if _, exists := properties[name]; !exists {
			continue
		}
		seen[name] = true
		kept = append(kept, name)
	}
	if len(kept) > 0 {
		schema["required"] = workBuddyStringsToAny(kept)
	} else {
		delete(schema, "required")
	}
	return true
}

func workBuddyHasUnion(schema map[string]any) bool {
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := schema[key].([]any); ok {
			return true
		}
	}
	return false
}

// workBuddyLocalRef is the schema a local $ref in root names ("#/$defs/x",
// "#/definitions/x"), or value itself.
func workBuddyLocalRef(root map[string]any, value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	ref, _ := object["$ref"].(string)
	if !strings.HasPrefix(ref, "#/") {
		return value
	}
	var at any = root
	for _, segment := range strings.Split(ref[2:], "/") {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		container, ok := at.(map[string]any)
		if !ok {
			return value
		}
		next, exists := container[segment]
		if !exists {
			return value
		}
		at = next
	}
	if _, ok := at.(map[string]any); ok {
		return at
	}
	return value
}

func workBuddyCloneJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = workBuddyCloneJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = workBuddyCloneJSON(item)
		}
		return out
	}
	return value
}

func workBuddySameJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func workBuddyStringsToAny(values []string) []any {
	out := make([]any, len(values))
	for index, value := range values {
		out[index] = value
	}
	return out
}

func workBuddyWithout(values []string, drop string) []string {
	out := values[:0]
	for _, value := range values {
		if value != drop {
			out = append(out, value)
		}
	}
	return out
}

// ---- tools a message carries -------------------------------------------------

// liftWorkBuddyMessageTools moves the tools a message carries into the
// request's own tools, each once by name: a tool the request already has, or
// an earlier message gave, keeps that first definition. A system or developer
// message that carried only tools is left out rather than sent on as an empty
// system message; one with content keeps its place without its tools. It
// reports whether it changed anything.
func liftWorkBuddyMessageTools(payload map[string]any) bool {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	tools, _ := payload["tools"].([]any)
	have := map[string]bool{}
	for _, raw := range tools {
		have[workBuddyToolKey(raw)] = true
	}
	changed := false
	kept := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		carried, exists := message["tools"]
		if !exists {
			kept = append(kept, raw)
			continue
		}
		changed = true
		if list, ok := carried.([]any); ok {
			for _, item := range list {
				tool, ok := item.(map[string]any)
				if !ok {
					continue
				}
				key := workBuddyToolKey(tool)
				if have[key] {
					continue
				}
				have[key] = true
				tools = append(tools, tool)
			}
		}
		delete(message, "tools")
		if role, _ := message["role"].(string); (role == "system" || role == "developer") &&
			workBuddyContentEmpty(message["content"]) {
			continue
		}
		kept = append(kept, message)
	}
	if !changed {
		return false
	}
	payload["messages"] = kept
	if len(tools) > 0 {
		payload["tools"] = tools
	} else {
		delete(payload, "tools")
	}
	return true
}

// workBuddyToolKey is a Chat tool's name, a hosted tool's without one by its
// type.
func workBuddyToolKey(raw any) string {
	tool, _ := raw.(map[string]any)
	if function, ok := tool["function"].(map[string]any); ok {
		if name, _ := function["name"].(string); name != "" {
			return name
		}
	}
	if name, _ := tool["name"].(string); name != "" {
		return name
	}
	return "#" + stringValue(tool["type"])
}

// workBuddyContentEmpty reports whether a message's content holds only empty
// text blocks.
func workBuddyContentEmpty(content any) bool {
	switch typed := content.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		empty := true
		for _, item := range typed {
			part, _ := item.(map[string]any)
			empty = stringValue(part["type"]) == "text" && stringValue(part["text"]) == ""
			if !empty {
				return false
			}
		}
		return empty
	}
	return false
}

// ---- 11153: tool results then the user's message ------------------------------

// workBuddyAfterToolsRefused is WorkBuddy's 400 code 11153, "the conversation
// must end with a user message", which its gemini models answer to a chat
// whose tool results are followed by the user's message. A chat with an empty
// assistant reply between them is served instead.
var workBuddyAfterToolsRefusedPattern = regexp.MustCompile(`(?i)must end with a user message|["']code["']\s*:\s*["']?11153(?:[^0-9.]|$)`)

func workBuddyAfterToolsRefused(body []byte) bool {
	return workBuddyAfterToolsRefusedPattern.Match(body)
}

// replyAfterWorkBuddyTools is a chat ending with tool results then the user's
// message(s), with an empty assistant message between them, as if the model
// had said nothing after the results. It is nil for any other chat — one
// ending with the assistant's message is a prefill and is refused as it is.
func replyAfterWorkBuddyTools(body []byte) []byte {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil || payload == nil {
		return nil
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return nil
	}
	at := len(messages) - 1
	for at >= 0 {
		message, _ := messages[at].(map[string]any)
		if role, _ := message["role"].(string); role != "user" {
			break
		}
		at--
	}
	if at == len(messages)-1 || at < 0 {
		return nil
	}
	last, _ := messages[at].(map[string]any)
	if role, _ := last["role"].(string); role != "tool" {
		return nil
	}
	withReply := make([]any, 0, len(messages)+1)
	withReply = append(withReply, messages[:at+1]...)
	withReply = append(withReply, map[string]any{"role": "assistant", "content": ""})
	withReply = append(withReply, messages[at+1:]...)
	payload["messages"] = withReply
	return encodeJSONWithoutEscaping(payload)
}

// ---- naming the model's calls back -------------------------------------------

// restoreWorkBuddyToolNames gives the model's calls in a successful response
// the names the client gave its tools, on one JSON body or an SSE stream.
func restoreWorkBuddyToolNames(response *http.Response, names map[string]string) {
	wrapToolNameRestore(response, names, renameWorkBuddyToolCallNamesJSON, renameWorkBuddyToolCallNamesLine)
}

// wrapToolNameRestore renames the tool calls in a successful response back to
// the names the client declared, streaming or not. Every rename a request makes
// to a tool name must be undone here: a client only dispatches the names it
// registered, so a renamed call reaches it as an unknown tool ("No such tool
// available: bash. Tool names are case-sensitive: call Bash instead.").
func wrapToolNameRestore(response *http.Response, names map[string]string, renameJSON, renameLine func([]byte, map[string]string) []byte) {
	if response == nil || response.Body == nil || len(names) == 0 || response.StatusCode >= 400 {
		return
	}
	jsonBody := strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json")
	response.Body = &toolNameRestoringBody{
		src:        response.Body,
		names:      names,
		reader:     bufio.NewReader(response.Body),
		jsonBody:   jsonBody,
		renameJSON: renameJSON,
		renameLine: renameLine,
	}
	response.Header.Del("Content-Length")
	response.ContentLength = -1
}

type toolNameRestoringBody struct {
	src        io.ReadCloser
	names      map[string]string
	reader     *bufio.Reader
	jsonBody   bool
	renameJSON func([]byte, map[string]string) []byte
	renameLine func([]byte, map[string]string) []byte
	out        bytes.Buffer
	done       bool
}

func (b *toolNameRestoringBody) Read(p []byte) (int, error) {
	for b.out.Len() == 0 {
		if b.done {
			return 0, io.EOF
		}
		if b.jsonBody {
			raw, err := io.ReadAll(b.src)
			if len(raw) > 0 {
				b.out.Write(b.renameJSON(raw, b.names))
			}
			b.done = true
			if err != nil && b.out.Len() == 0 {
				return 0, err
			}
			break
		}
		line, err := b.reader.ReadBytes('\n')
		if len(line) > 0 {
			b.out.Write(b.renameLine(line, b.names))
		}
		if err != nil {
			b.done = true
			if err != io.EOF && b.out.Len() == 0 {
				return 0, err
			}
			break
		}
	}
	return b.out.Read(p)
}

func (b *toolNameRestoringBody) Close() error {
	return b.src.Close()
}

func renameWorkBuddyToolCallNamesJSON(raw []byte, names map[string]string) []byte {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	if !renameWorkBuddyToolCallNames(value, names) {
		return raw
	}
	if encoded := encodeJSONWithoutEscaping(value); encoded != nil {
		return encoded
	}
	return raw
}

func renameWorkBuddyToolCallNamesLine(line []byte, names map[string]string) []byte {
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
	if json.Unmarshal(payload, &value) != nil || !renameWorkBuddyToolCallNames(value, names) {
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

func renameWorkBuddyToolCallNames(value any, names map[string]string) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}
	choices, _ := root["choices"].([]any)
	changed := false
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"message", "delta"} {
			at, ok := choice[key].(map[string]any)
			if !ok {
				continue
			}
			calls, _ := at["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				function, ok := call["function"].(map[string]any)
				if !ok {
					continue
				}
				name, _ := function["name"].(string)
				if ours, found := names[name]; found && ours != name {
					function["name"] = ours
					changed = true
				}
			}
		}
	}
	return changed
}

func encodeJSONWithoutEscaping(payload any) []byte {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(payload) != nil {
		return nil
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}
