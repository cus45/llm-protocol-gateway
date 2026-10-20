package gateway

// Qoder's chat transport: an OpenAI-shaped request in, Qoder's
// agent_chat_generation call out, and Qoder's reply converted back into OpenAI
// chat chunks so every converter, log writer and quota counter downstream sees
// what it already knows how to read (the same shape Trae is served with).
//
// Two things make this hop its own: the body goes out in Qoder's codec under a
// signed COSY envelope, and Qoder writes some of its tool calls as XML inside
// the answer text rather than as tool calls, so the text has to be split back
// apart while it streams.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// The XML markers are written with hex escapes for their angle brackets so this
// source holds no literal tag that tooling might mistake for a call.
const (
	qoderCallOpen  = "\x3ctool_call\x3e"
	qoderCallClose = "\x3c/tool_call\x3e"

	// qoderSys is the line Qoder's own client puts first: the endpoint routes
	// on it, so a request without it is not answered the same way.
	qoderSys = "You are a Qoder agent. Use the instructions below and the tools available to you to assist the user."

	// qoderDefaultMaxTokens is what the reply is given room for when the client
	// asked for none.
	qoderDefaultMaxTokens = 32000
)

var (
	qoderFuncRe  = regexp.MustCompile(`(?s)\x3cfunction=([^>]+)\x3e(.*?)\x3c/function\x3e`)
	qoderParamRe = regexp.MustCompile(`(?s)\x3cparameter=([^>]+)\x3e(.*?)\x3c/parameter\x3e`)
)

// qoderEffortRank orders every level a client may ask for, lowest first,
// including the two Qoder itself does not name.
var qoderEffortRank = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// proxyQoderChat is the pass-through path for a Qoder provider.
func (s *Server) proxyQoderChat(w http.ResponseWriter, r *http.Request, provider domain.Provider, model string, body []byte, skipIncomingAuth bool) (int, TokenUsage, []byte, error) {
	stream := requestBodyWantsStream(body)
	refreshed, err := s.ensureFreshQoderToken(provider)
	if err != nil {
		return 0, TokenUsage{}, nil, err
	}
	response, err := s.sendQoderChat(r.Context(), refreshed, model, body)
	if err != nil {
		return 0, TokenUsage{}, nil, err
	}
	if response == nil {
		return 0, TokenUsage{}, nil, errors.New("Qoder 没有应答")
	}
	defer response.Body.Close()
	if !stream && response.StatusCode < 400 {
		// Qoder only answers as a stream; a client that asked for a single
		// object gets the converted stream rebuilt into one.
		_ = forcedStreamAsCompletion(response, model, workBuddyStreamAsCompletion)
	}
	status, usage, responseBody, err := writePassThroughResponse(w, response, stream, ParseOpenAIUsage)
	return status, usage, responseBody, err
}

// sendQoderChat signs one chat request and hands back an OpenAI-shaped
// response. A refusal is answered with the status a client of this gateway
// understands, because Qoder's own codes mean something else than its HTTP
// status suggests.
func (s *Server) sendQoderChat(ctx context.Context, provider domain.Provider, model string, body []byte) (*http.Response, error) {
	if !isQoderProvider(provider) {
		return nil, errors.New("不是 Qoder 连接方式")
	}
	if provider.QoderPAT == nil || provider.QoderPAT.Disconnected {
		return nil, errors.New("Qoder 尚未登录，请先在控制台完成登录")
	}
	credential := *provider.QoderPAT
	if strings.TrimSpace(model) == "" {
		model = strings.TrimSpace(provider.DefaultModel)
	}
	info, ok := qoderModelFor(provider, model)
	if !ok {
		// The account's own listing carries each model's configuration, which a
		// request has to send with it. Ask for the listing once here: a
		// connection made before this gateway read it, or one whose first sync
		// failed, is still usable without a manual sync.
		if refreshed, fetched := s.qoderWithListing(ctx, provider); fetched {
			provider = refreshed
			credential = *provider.QoderPAT
			info, ok = qoderModelFor(provider, model)
		}
	}
	if !ok {
		return nil, fmt.Errorf("Qoder 的模型列表里没有 %q，请在控制台同步模型或换一个模型", model)
	}
	plain, err := qoderChatBody(model, body, info)
	if err != nil {
		return nil, err
	}
	wire := qoderEncodeBody(plain)
	endpoint := qoderSiteOf(provider).qoderChatURL()
	headers, err := qoderCOSYHeaders(endpoint, qoderAccount{
		UID:       credential.UID,
		Name:      credential.Name,
		Email:     credential.Email,
		Token:     credential.AccessToken,
		MachineID: credential.MachineID,
	}, wire, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(wire))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("X-Model-Key", info.Key)
	if info.Source != "" {
		request.Header.Set("X-Model-Source", info.Source)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode/100 != 2 {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		status, message, wait := qoderFailure(response.StatusCode, payload)
		return qoderFailureResponse(status, message, wait), nil
	}
	response.Body = newQoderChunkReader(response.Body, model)
	response.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
	response.Header.Set("Cache-Control", "no-cache")
	response.ContentLength = -1
	return response, nil
}

// qoderWithListing refetches the account's model list, reporting whether it now
// has one.
func (s *Server) qoderWithListing(ctx context.Context, provider domain.Provider) (domain.Provider, bool) {
	if provider.QoderPAT == nil {
		return provider, false
	}
	listing, err := qoderFetchListing(ctx, qoderSiteOf(provider), qoderAccountOf(provider))
	if err != nil {
		return provider, false
	}
	credential := *provider.QoderPAT
	credential.Models = listing
	updated, err := s.router.SetProviderQoderPAT(provider.ID, credential)
	if err != nil {
		return provider, false
	}
	_ = s.persistProviderOAuth(updated)
	return updated, len(qoderListingModels(mustQoderListing(listing))) > 0
}

func mustQoderListing(raw json.RawMessage) qoderListing {
	var listing qoderListing
	_ = json.Unmarshal(raw, &listing)
	return listing
}

// qoderChatBody is the plaintext JSON Qoder's agent_chat_generation takes for
// an OpenAI chat completion request.
func qoderChatBody(model string, raw []byte, info qoderModelInfo) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		return nil, fmt.Errorf("解析对话请求失败: %w", err)
	}
	system := qoderSystemText(chat)
	tools := qoderToolsOf(chat)
	if len(tools) > 0 {
		system += "\nYou must call an available function in this response."
	}
	sysBlock := map[string]any{"type": "text", "text": system}
	thinking, effort := qoderEffort(qoderAskedEffort(chat), info)
	maxTokens := qoderRequestedMaxTokens(chat)
	params := map[string]any{"enable_thinking": thinking, "max_tokens": maxTokens}
	if effort != "" {
		params["reasoning_effort"] = effort
	}
	if window := qoderWindowFor(chat, info, maxTokens); window > 0 {
		params["context_length"] = window
	}
	messages := append([]any{map[string]any{"role": "system", "content": []any{sysBlock}}}, qoderMessagesOf(chat)...)
	body := map[string]any{
		"parameters": params,
		"business": map[string]any{
			"product": "app", "version": qoderCOSYVersion, "type": "agent",
			"id": randomHex(16), "name": model, "begin_at": time.Now().UnixMilli(), "stage": "start",
		},
		"agent_id":     "agent_common",
		"task_id":      "common",
		"session_type": "app",
		"model_config": info.Config,
		"system":       []any{sysBlock},
		"messages":     messages,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	return json.Marshal(body)
}

// qoderSystemText is Qoder's own line, then the caller's: the endpoint takes
// one system block, so every system turn of the request is folded into it.
func qoderSystemText(chat map[string]any) string {
	var parts []string
	for _, item := range messageList(chat) {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role := strings.ToLower(qoderStringOf(message["role"])); role == "system" || role == "developer" {
			if text := qoderTextOf(message["content"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	system := qoderSys
	if len(parts) > 0 {
		system += "\n\n" + strings.Join(parts, "\n\n")
	}
	return system
}

// qoderToolChoiceNone is the one case where the caller's tools must not be
// sent: Qoder answers a request that offers tools with a call whatever the
// choice said, so "none" means the tools go away rather than that they are
// discouraged.
func qoderToolChoiceNone(chat map[string]any) bool {
	switch choice := chat["tool_choice"].(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(choice), "none")
	default:
		return false
	}
}

func qoderToolsOf(chat map[string]any) []any {
	if qoderToolChoiceNone(chat) {
		return nil
	}
	declared, ok := chat["tools"].([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, item := range declared {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(qoderStringOf(function["name"]))
		if name == "" {
			continue
		}
		description := qoderStringOf(function["description"])
		// Qoder validates the description as a string, while the OpenAI spec
		// makes it optional and clients omit it — Claude Code's built-in
		// web_search tool is one that does.
		parameters := function["parameters"]
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name":        name,
			"description": description,
			"parameters":  parameters,
		}})
	}
	return out
}

// qoderMessagesOf renders the caller's turns as Qoder takes them: text and
// image blocks, an assistant's tool calls as tool_calls, and their results as
// tool turns named by tool_call_id. A tool turn holds text only, so images a
// tool returned follow in a user turn — as the start of the user's own turn
// when one comes next.
func qoderMessagesOf(chat map[string]any) []any {
	var out []any
	names := map[string]string{}
	var queued []any
	flushQueued := func() {
		if len(queued) > 0 {
			out = append(out, map[string]any{"role": "user", "content": queued})
			queued = nil
		}
	}
	for _, item := range messageList(chat) {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(qoderStringOf(message["role"]))
		if role == "system" || role == "developer" {
			continue
		}
		if role != "user" && role != "tool" {
			flushQueued()
		}
		switch role {
		case "tool":
			text := qoderTextOf(message["content"])
			images := qoderImageBlocks(message["content"])
			if len(images) > 0 {
				callID := qoderStringOf(message["tool_call_id"])
				of := "tool call " + callID
				if name := firstNonEmpty(names[callID], qoderStringOf(message["name"])); name != "" {
					of = name + " (" + of + ")"
				}
				note := fmt.Sprintf("[The tool returned %d images; they follow in the next message.]", len(images))
				if len(images) == 1 {
					note = "[The tool returned an image; it follows in the next message.]"
				}
				queued = append(queued, map[string]any{"type": "text", "text": "[From the result of " + of + ":]"})
				queued = append(queued, images...)
				if strings.TrimSpace(text) != "" {
					text += "\n\n"
				}
				text += note
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": qoderStringOf(message["tool_call_id"]),
				"content":      text,
			})
		case "assistant":
			calls := qoderAssistantCalls(message, names)
			text := qoderTextOf(message["content"])
			if len(calls) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": text, "tool_calls": calls})
				continue
			}
			if text == "" {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}})
		default:
			blocks := qoderContentBlocks(message["content"])
			if len(blocks) == 0 {
				continue
			}
			if role == "user" && len(queued) > 0 {
				blocks = append(queued, blocks...)
				queued = nil
			}
			out = append(out, map[string]any{"role": role, "content": blocks})
		}
	}
	flushQueued()
	return out
}

// qoderAssistantCalls is an assistant turn's tool calls as Qoder takes them,
// remembering each id so a result's images can name the tool they came from.
func qoderAssistantCalls(message map[string]any, names map[string]string) []any {
	declared, ok := message["tool_calls"].([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, item := range declared {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(qoderStringOf(function["name"]))
		if name == "" {
			continue
		}
		id := strings.TrimSpace(qoderStringOf(call["id"]))
		if id == "" {
			id = "call_" + randomHex(16)
		}
		names[id] = name
		arguments := function["arguments"]
		switch value := arguments.(type) {
		case string:
		default:
			if arguments != nil {
				raw, _ := json.Marshal(value)
				arguments = string(raw)
			} else {
				arguments = "{}"
			}
		}
		out = append(out, map[string]any{"id": id, "type": "function", "function": map[string]any{
			"name":      name,
			"arguments": arguments,
		}})
	}
	return out
}

func messageList(chat map[string]any) []any {
	messages, _ := chat["messages"].([]any)
	return messages
}

func qoderStringOf(value any) string {
	text, _ := value.(string)
	return text
}

// qoderTextOf is the plain text of a content field, which a client may send as
// a string or as blocks.
func qoderTextOf(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if strings.EqualFold(qoderStringOf(block["type"]), "text") {
				builder.WriteString(qoderStringOf(block["text"]))
			}
		}
		return builder.String()
	default:
		return ""
	}
}

// qoderImageBlocks lifts a content field's images into Qoder's image_url
// blocks, which carry a data: URL.
func qoderImageBlocks(content any) []any {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || !strings.EqualFold(qoderStringOf(block["type"]), "image_url") {
			continue
		}
		url := ""
		switch value := block["image_url"].(type) {
		case string:
			url = value
		case map[string]any:
			url = qoderStringOf(value["url"])
		}
		if strings.TrimSpace(url) != "" {
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		}
	}
	return out
}

func qoderContentBlocks(content any) []any {
	switch value := content.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": value}}
	case []any:
		var out []any
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(qoderStringOf(block["type"])) {
			case "text":
				if text := qoderStringOf(block["text"]); text != "" {
					out = append(out, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				out = append(out, qoderImageBlocks([]any{block})...)
			}
		}
		return out
	default:
		return nil
	}
}

// qoderAskedEffort is the reasoning level the client asked for. Claude clients
// arrive converted, so a thinking block counts as "asked to think" without a
// level of its own.
func qoderAskedEffort(chat map[string]any) string {
	if effort := strings.ToLower(strings.TrimSpace(qoderStringOf(chat["reasoning_effort"]))); effort != "" {
		return effort
	}
	if thinking, ok := chat["thinking"].(map[string]any); ok {
		if kind := strings.ToLower(qoderStringOf(thinking["type"])); kind == "disabled" {
			return "none"
		}
	}
	return ""
}

func qoderRequestedMaxTokens(chat map[string]any) int {
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if tokens, ok := chat[key].(float64); ok && int(tokens) > 0 {
			return int(tokens)
		}
	}
	return qoderDefaultMaxTokens
}

// qoderEffort is whether to think and at which of the model's own levels ("" to
// send none). Asked for none, a model that always thinks thinks at its lowest;
// asked for nothing, or for a level Qoder doesn't name, at its own default.
func qoderEffort(asked string, info qoderModelInfo) (bool, string) {
	want := strings.ToLower(strings.TrimSpace(asked))
	ranked := false
	for _, level := range qoderEffortRank {
		if level == want {
			ranked = true
			break
		}
	}
	if !ranked {
		want = ""
	}
	if !info.Thinks || (want == "none" && !info.AlwaysThinks) {
		return false, ""
	}
	if len(info.Efforts) == 0 {
		return true, ""
	}
	switch want {
	case "":
		return true, info.DefaultEffort
	case "none":
		return true, info.Efforts[0]
	}
	return true, qoderFitEffort(want, info.Efforts)
}

// qoderFitEffort is the offered level nearest the one asked, the higher one on a
// tie, so asking "max" of a model that stops at "high" is not silently read as
// asking the lowest.
func qoderFitEffort(want string, levels []string) string {
	if want == "ultra" {
		for _, level := range levels {
			if level == "ultra" {
				return level
			}
		}
		want = "max"
	}
	for _, level := range levels {
		if level == want {
			return level
		}
	}
	at := -1
	for index, level := range qoderEffortRank {
		if level == want {
			at = index
			break
		}
	}
	if at < 0 {
		return want
	}
	best, distance := want, len(qoderEffortRank)
	for _, level := range levels {
		index := -1
		for position, known := range qoderEffortRank {
			if known == level {
				index = position
				break
			}
		}
		if index < 0 || level == "none" {
			continue
		}
		gap := index - at
		if gap < 0 {
			gap = -gap
		}
		if gap < distance || (gap == distance && index > at) {
			best, distance = level, gap
		}
	}
	return best
}

// qoderWindowFor is the context window a request is sent on a model that offers
// several: the one Qoder's client picks by default while the request fits it,
// else the smallest that holds it, else the largest. A larger window may cost
// more, so it is asked for only when the conversation needs it. The request's
// size is counted generously — a token per three bytes of its JSON — so a window
// is raised a little early rather than late, and the reply's own budget counts
// in it too.
func qoderWindowFor(chat map[string]any, info qoderModelInfo, reply int) int {
	base := info.DefaultWindow
	if base <= 0 {
		base = info.MaxInputTokens
	}
	if base <= 0 {
		base = info.largestContext()
	}
	if len(info.Windows) == 0 {
		return info.largestContext()
	}
	messages, _ := chat["messages"].([]any)
	tools := qoderToolsOf(chat)
	measured, _ := json.Marshal(map[string]any{"m": messages, "t": tools})
	need := (len(measured)+2)/3 + reply
	if base > 0 && need <= base {
		return base
	}
	for _, window := range info.Windows {
		if window >= need {
			return window
		}
	}
	return info.Windows[len(info.Windows)-1]
}

// qoderFailure maps a refused call to the status a client of this gateway
// understands, the message it should read, and how long Qoder said to wait
// before asking again. Qoder's own code in the body says what the failure is,
// whatever the HTTP status: its client's error table counts 105 as the only
// lapsed sign-in, so a 401 or 403 carrying another code is not one.
func qoderFailure(status int, payload []byte) (int, string, time.Duration) {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	var answer struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
		Details struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"details"`
		Queue struct {
			IsQueued          bool   `json:"isQueued"`
			ServiceAvailable  *bool  `json:"serviceAvailable"`
			RetryAfterSeconds int    `json:"retryAfterSeconds"`
			ModelKey          string `json:"modelKey"`
			QueueType         string `json:"queueType"`
		} `json:"queue"`
	}
	code := ""
	message := strings.TrimSpace(string(payload))
	if json.Unmarshal(payload, &answer) == nil {
		code = strings.TrimSpace(answer.Code.String())
		if answer.Message != "" {
			message = answer.Message
		}
		if answer.Details.Error.Message != "" {
			message += ": " + answer.Details.Error.Message
		}
		// Qoder's queue sometimes answers with its state as the message itself.
		if !answer.Queue.IsQueued && answer.Queue.RetryAfterSeconds == 0 && strings.HasPrefix(strings.TrimSpace(answer.Message), "{") {
			var nested struct {
				IsQueued          bool   `json:"isQueued"`
				ServiceAvailable  *bool  `json:"serviceAvailable"`
				RetryAfterSeconds int    `json:"retryAfterSeconds"`
				ModelKey          string `json:"modelKey"`
				QueueType         string `json:"queueType"`
			}
			if json.Unmarshal([]byte(answer.Message), &nested) == nil {
				answer.Queue.IsQueued = nested.IsQueued
				answer.Queue.ServiceAvailable = nested.ServiceAvailable
				answer.Queue.RetryAfterSeconds = nested.RetryAfterSeconds
				answer.Queue.ModelKey = nested.ModelKey
				answer.Queue.QueueType = nested.QueueType
			}
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if len(message) > 600 {
		message = message[:600] + "…"
	}
	queued := code == "10605" || answer.Queue.IsQueued || (answer.Queue.ServiceAvailable != nil && !*answer.Queue.ServiceAvailable)
	if queued {
		wait := 30 * time.Second
		if answer.Queue.RetryAfterSeconds > 0 {
			wait = time.Duration(answer.Queue.RetryAfterSeconds) * time.Second
		}
		what := "is busy"
		if answer.Queue.IsQueued {
			what = "已把请求排进队列"
		}
		if answer.Queue.ModelKey != "" {
			what += "（模型 " + answer.Queue.ModelKey + "）"
		}
		if answer.Queue.QueueType != "" {
			what += "（该套餐队列：" + answer.Queue.QueueType + "）"
		}
		// A queue is the model being busy, not the account being out of
		// standing: the same token reads models and usage fine, so nothing here
		// marks the sign-in.
		return http.StatusServiceUnavailable, fmt.Sprintf("Qoder %s，请 %ds 后重试", what, int(wait.Seconds())+1), wait
	}
	switch {
	case qoderCodeIn(code, []string{"110", "112", "113", "114", "115", "116", "117", "118", "119", "122"}):
		return http.StatusTooManyRequests, "额度已用完：" + message, 0
	case qoderCodeIn(code, []string{"406", "416", "430", "80411", "80412", "100400", "100401", "100403"}):
		return http.StatusBadRequest, fmt.Sprintf("Qoder 拒绝了这个请求（%s）：%s", code, message), 0
	case code == "105":
		return http.StatusUnauthorized, "Qoder 的登录已失效，请重新登录", 0
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if code == "" {
			return http.StatusUnauthorized, "Qoder 的登录已失效，请重新登录", 0
		}
		return status, "Qoder：" + message, 0
	case status == http.StatusTooManyRequests || strings.Contains(strings.ToLower(message), "quota"):
		return http.StatusTooManyRequests, "额度已用完：" + message, 0
	}
	return status, "Qoder：" + message, 0
}

func qoderCodeIn(code string, set []string) bool {
	if code == "" {
		return false
	}
	for _, known := range set {
		if known == code {
			return true
		}
	}
	return false
}

// qoderFailureResponse answers a refusal with what a client of this gateway
// expects, and Qoder's waiting time as Retry-After so the failover and the
// client both honor it.
func qoderFailureResponse(status int, message string, wait time.Duration) *http.Response {
	kind := "api_error"
	if status == http.StatusTooManyRequests {
		kind = "rate_limit_error"
	}
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message,
		"type":    kind,
		"status":  status,
	}})
	response := &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}
	response.ContentLength = int64(len(payload))
	response.Header.Set("Content-Type", "application/json; charset=utf-8")
	if wait > 0 {
		response.Header.Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	}
	return response
}
