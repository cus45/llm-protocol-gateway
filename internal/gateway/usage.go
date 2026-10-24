package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// TokenUsage holds parsed token counts from an upstream LLM response.
//
// InputTokens always uses OpenAI-compatible semantics: total prompt/input
// tokens INCLUDING cache hits (and Claude cache creation). CacheTokens is the
// cache-read/hit portion only, never added again on top of InputTokens.
type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
	CacheTokens  int64
	// CacheWriteTokens is the cache_creation half: already folded into
	// InputTokens (it is billed input), but the console shows the split because
	// a cache write costs several times a cache read.
	CacheWriteTokens int64
}

// normalizeClaudeUsageFields converts Anthropic usage fields into the gateway's
// OpenAI-compatible TokenUsage: Claude reports input_tokens as non-cached only,
// so total input = input_tokens + cache_read_input_tokens + cache_creation_input_tokens.
func normalizeClaudeUsageFields(inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int64) TokenUsage {
	totalInput := inputTokens + cacheReadTokens + cacheCreationTokens
	if totalInput < 0 {
		totalInput = 0
	}
	return TokenUsage{
		InputTokens:      totalInput,
		OutputTokens:     outputTokens,
		CacheTokens:      cacheReadTokens,
		CacheWriteTokens: cacheCreationTokens,
	}
}

// normalizeInclusiveInputTokens converts provider usage fields into total prompt
// tokens INCLUDING cache hits. OpenAI-compatible providers usually report inclusive
// prompt_tokens, but some proxies mirror Anthropic semantics (exclusive prompt +
// separate cache hit/miss counts).
func normalizeInclusiveInputTokens(baseInput, cacheRead, cacheCreation, cacheMiss int64) int64 {
	if baseInput < 0 {
		baseInput = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheCreation < 0 {
		cacheCreation = 0
	}
	if cacheMiss < 0 {
		cacheMiss = 0
	}
	if cacheRead == 0 && cacheCreation == 0 {
		return baseInput
	}
	if baseInput >= cacheRead {
		return baseInput
	}
	if cacheMiss > 0 {
		return cacheMiss + cacheRead + cacheCreation
	}
	return baseInput + cacheRead + cacheCreation
}

// claudeExclusiveInputTokens converts inclusive InputTokens back to Anthropic's
// non-cached input_tokens field for Claude client responses.
func claudeExclusiveInputTokens(usage TokenUsage, cacheCreationTokens int64) int64 {
	exclusive := usage.InputTokens - usage.CacheTokens - cacheCreationTokens
	if exclusive < 0 {
		return 0
	}
	return exclusive
}

func int64FromAny(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	default:
		return 0
	}
}

// ParseOpenAIUsage extracts prompt/completion/cache-hit tokens from an OpenAI
// Chat Completions JSON response body, or from an SSE stream body.
func ParseOpenAIUsage(body []byte) TokenUsage {
	if usage := parseOpenAIUsageJSON(body); usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheTokens > 0 {
		return usage
	}
	return parseOpenAIUsageFromSSE(body)
}

func parseOpenAIUsageJSON(body []byte) TokenUsage {
	return openAIUsageFromMap(extractCacheUsage(body))
}

// openAIUsageFromMap reads one usage object. Shared with the incremental
// stream parser so a JSON body and a stream chunk cannot drift apart.
func openAIUsageFromMap(usage map[string]any) TokenUsage {
	if usage == nil {
		return TokenUsage{}
	}
	promptTokens := int64FromAny(usage["prompt_tokens"])
	cacheRead := int64(cacheHitTokenCount(usage))
	cacheCreation := int64FromAny(usage["cache_creation_input_tokens"])
	cacheMiss := int64FromAny(usage["prompt_cache_miss_tokens"])
	return TokenUsage{
		InputTokens:      normalizeInclusiveInputTokens(promptTokens, cacheRead, cacheCreation, cacheMiss),
		OutputTokens:     int64FromAny(usage["completion_tokens"]),
		CacheTokens:      cacheRead,
		CacheWriteTokens: cacheCreation,
	}
}

// usageEvent decodes only what the fold needs out of one streamed event: its
// type and its usage object. Every other field — the answer's own text, which
// makes up nearly all of a stream — is skipped rather than decoded, so billing
// a long answer costs one extra pass over its bytes and no parsing.
func usageEvent(payload []byte) (eventType string, usage map[string]any) {
	// A byte-level probe lets the common case (an event that carries no usage
	// at all) skip the decoder entirely: the answer's own chunks make up nearly
	// all of a stream.
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return quickEventType(payload), nil
	}
	return decodeUsageEvent(payload)
}

// decodeUsageEvent reads the type and the usage object of one streamed event,
// skipping every other field — the answer's own text, which makes up nearly all
// of a stream.
func decodeUsageEvent(payload []byte) (eventType string, usage map[string]any) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return "", nil
	}
	var found map[string]any
	corrupt := func() (string, map[string]any) { return quickEventType(payload), nil }
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return corrupt()
		}
		key, _ := keyToken.(string)
		switch key {
		case "usage":
			var value map[string]any
			if err := decoder.Decode(&value); err != nil {
				return corrupt()
			}
			if len(value) > 0 {
				found = value
			}
		case "type":
			var value string
			if err := decoder.Decode(&value); err != nil {
				return corrupt()
			}
			eventType = value
		case "message", "response":
			// The one nesting level that carries usage: message_start and the
			// Responses terminal event.
			var wrapper struct {
				Usage map[string]any `json:"usage"`
			}
			if err := decoder.Decode(&wrapper); err != nil {
				return corrupt()
			}
			if len(wrapper.Usage) > 0 {
				found = wrapper.Usage
			}
		default:
			var skipped json.RawMessage
			if err := decoder.Decode(&skipped); err != nil {
				return corrupt()
			}
		}
	}
	return eventType, found
}

// quickEventType reads just the type of an event without decoding the rest.
func quickEventType(payload []byte) string {
	index := bytes.Index(payload, []byte(`"type"`))
	if index < 0 {
		return ""
	}
	rest := payload[index+len(`"type"`):]
	rest = bytes.TrimLeft(rest, " \t\r\n")
	if len(rest) < 2 || rest[0] != ':' {
		return ""
	}
	rest = bytes.TrimLeft(rest[1:], " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	end := bytes.IndexByte(rest[1:], '"')
	if end < 0 {
		return ""
	}
	return string(rest[1 : end+1])
}

func parseOpenAIUsageFromSSE(body []byte) TokenUsage {
	text := strings.TrimSpace(string(body))
	if text == "" || !strings.Contains(text, "data:") {
		return TokenUsage{}
	}
	usage := TokenUsage{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if chunkUsage := parseOpenAIUsageJSON([]byte(payload)); chunkUsage.InputTokens > 0 || chunkUsage.OutputTokens > 0 || chunkUsage.CacheTokens > 0 {
			usage = chunkUsage
		}
	}
	return usage
}

// ParseClaudeUsage extracts input/output/cache-read tokens from an Anthropic
// Messages API JSON response body or SSE stream body.
func ParseClaudeUsage(body []byte) TokenUsage {
	if usage := parseClaudeUsageJSON(body); usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheTokens > 0 {
		return usage
	}
	return parseClaudeUsageFromSSE(body)
}

// claudePassthroughUsage reads Anthropic's usage object without summing it, so
// a stream can keep the prompt figures from message_start and take only the
// running output count from message_delta. Summing per event would count the
// cache fields twice.
func claudePassthroughUsage(usage map[string]any) (promptTokens, outputTokens, cacheRead, cacheWrite int64) {
	if usage == nil {
		return 0, 0, 0, 0
	}
	return int64FromAny(usage["input_tokens"]),
		int64FromAny(usage["output_tokens"]),
		int64FromAny(usage["cache_read_input_tokens"]),
		int64FromAny(usage["cache_creation_input_tokens"])
}

// mergeClaudePassthroughUsage folds one more usage object into the running
// figures. Anthropic omits zero fields on message_delta (it reports
// output_tokens there and nothing else), and a field it does report is the
// latest total for that field, not an increment, so it replaces.
func mergeClaudePassthroughUsage(promptTokens, outputTokens, cacheRead, cacheWrite int64, usage map[string]any) (int64, int64, int64, int64) {
	if usage == nil {
		return promptTokens, outputTokens, cacheRead, cacheWrite
	}
	if _, ok := usage["input_tokens"]; ok {
		promptTokens = int64FromAny(usage["input_tokens"])
	}
	if _, ok := usage["output_tokens"]; ok {
		outputTokens = int64FromAny(usage["output_tokens"])
	}
	if _, ok := usage["cache_read_input_tokens"]; ok {
		cacheRead = int64FromAny(usage["cache_read_input_tokens"])
	}
	if _, ok := usage["cache_creation_input_tokens"]; ok {
		cacheWrite = int64FromAny(usage["cache_creation_input_tokens"])
	}
	return promptTokens, outputTokens, cacheRead, cacheWrite
}

func parseClaudeUsageMap(usage map[string]any) TokenUsage {
	if usage == nil {
		return TokenUsage{}
	}
	return normalizeClaudeUsageFields(
		int64FromAny(usage["input_tokens"]),
		int64FromAny(usage["output_tokens"]),
		int64FromAny(usage["cache_read_input_tokens"]),
		int64FromAny(usage["cache_creation_input_tokens"]),
	)
}

func parseClaudeUsageJSON(body []byte) TokenUsage {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return TokenUsage{}
	}
	usageValue, ok := payload["usage"]
	if !ok {
		return TokenUsage{}
	}
	usage, ok := usageValue.(map[string]any)
	if !ok {
		return TokenUsage{}
	}
	return parseClaudeUsageMap(usage)
}

func parseClaudeUsageFromSSE(body []byte) TokenUsage {
	text := strings.TrimSpace(string(body))
	if text == "" || !strings.Contains(text, "data:") {
		return TokenUsage{}
	}
	usage := TokenUsage{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		switch stringValue(event["type"]) {
		case "message_start":
			if message, ok := event["message"].(map[string]any); ok {
				if usageValue, ok := message["usage"].(map[string]any); ok {
					parsed := parseClaudeUsageMap(usageValue)
					// Keep later message_delta output/cache overrides when present.
					if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheTokens == 0 {
						usage = parsed
					} else {
						if parsed.InputTokens > 0 {
							usage.InputTokens = parsed.InputTokens
						}
						if parsed.CacheTokens > 0 {
							usage.CacheTokens = parsed.CacheTokens
						}
						if parsed.CacheWriteTokens > 0 {
							usage.CacheWriteTokens = parsed.CacheWriteTokens
						}
						if parsed.OutputTokens > usage.OutputTokens {
							usage.OutputTokens = parsed.OutputTokens
						}
					}
				}
			}
		case "message_delta":
			if usageValue, ok := event["usage"].(map[string]any); ok {
				parsed := parseClaudeUsageMap(usageValue)
				if parsed.InputTokens > 0 || parsed.CacheTokens > 0 {
					usage.InputTokens = parsed.InputTokens
					usage.CacheTokens = parsed.CacheTokens
					if parsed.CacheWriteTokens > 0 {
						usage.CacheWriteTokens = parsed.CacheWriteTokens
					}
				}
				if parsed.OutputTokens > 0 {
					usage.OutputTokens = parsed.OutputTokens
				}
			}
		}
	}
	return usage
}

// ParseResponsesUsage extracts token counts from an OpenAI Responses API body.
func ParseResponsesUsage(body []byte) TokenUsage {
	if usage := parseResponsesUsageJSON(body); usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheTokens > 0 {
		return usage
	}
	return parseResponsesUsageFromSSE(body)
}

func parseResponsesUsageJSON(body []byte) TokenUsage {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return TokenUsage{}
	}
	return responsesUsageFromPayload(payload)
}

func responsesUsageFromPayload(payload map[string]any) TokenUsage {
	usageValue, ok := payload["usage"].(map[string]any)
	if !ok {
		if response, ok := payload["response"].(map[string]any); ok {
			usageValue, ok = response["usage"].(map[string]any)
		}
	}
	if !ok {
		return TokenUsage{}
	}
	return responsesUsageFromUsageMap(usageValue)
}

// responsesUsageFromUsageMap reads one Responses usage object: the figures
// under input_tokens and output_tokens, the cache in input_tokens_details.
// Shared with the incremental stream fold so a whole body and one streamed
// event cannot drift apart.
// responsesUsageFromUsageMap reads one Responses usage object. Shared with the
// incremental stream fold so a JSON body and a streamed terminal event cannot
// drift apart.
func responsesUsageFromUsageMap(usageValue map[string]any) TokenUsage {
	inputTokens := int64FromAny(usageValue["input_tokens"])
	cacheTokens := int64(0)
	if details, ok := usageValue["input_tokens_details"].(map[string]any); ok {
		cacheTokens = int64FromAny(details["cached_tokens"])
	}
	return TokenUsage{
		InputTokens:  normalizeInclusiveInputTokens(inputTokens, cacheTokens, 0, 0),
		OutputTokens: int64FromAny(usageValue["output_tokens"]),
		CacheTokens:  cacheTokens,
	}
}

func parseResponsesUsageFromSSE(body []byte) TokenUsage {
	text := strings.TrimSpace(string(body))
	if text == "" || !strings.Contains(text, "data:") {
		return TokenUsage{}
	}
	usage := TokenUsage{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if chunkUsage := parseResponsesUsageJSON([]byte(payload)); chunkUsage.InputTokens > 0 || chunkUsage.OutputTokens > 0 || chunkUsage.CacheTokens > 0 {
			usage = chunkUsage
		}
	}
	return usage
}

// EstimateTokenUsage falls back to a rough body-length estimate when real usage
// is unavailable (e.g. streaming responses).
func EstimateTokenUsage(requestBody, responseBody []byte) TokenUsage {
	return TokenUsage{
		InputTokens:  int64(len(requestBody) / 4),
		OutputTokens: int64(len(responseBody) / 4),
	}
}

// usageStreamKind names the dialect a streamed body is parsed in. It replaces
// comparing parser function pointers: the call sites that pick a parser know
// which dialect it reads, so they say so and the reader never guesses.
type usageStreamKind int

const (
	// usageStreamUnfoldable is a stream no incremental fold covers (e.g. a
	// transcript body, or a response with no usage to report). The reader only
	// watches how the stream ends; its usage is never read back.
	usageStreamUnfoldable usageStreamKind = iota
	usageStreamOpenAI
	usageStreamResponses
	usageStreamClaude
)

func (k usageStreamKind) folder() func(*streamUsageReader, string, map[string]any) {
	switch k {
	case usageStreamOpenAI:
		return foldRunningTotalEvent
	case usageStreamResponses:
		return foldResponsesEvent
	case usageStreamClaude:
		return foldClaudeEvent
	default:
		return nil
	}
}

// streamUsageReader is both the incremental parser and the io.Writer that feeds
// it. It records the last running total that reports something: OpenAI sends
// usage on the final chunk, and an earlier one (when present) is incomplete.
// Anthropic is different — message_start carries the prompt figures and
// message_delta the running output count, and every reported field is a total,
// not an increment — so its raw fields are kept until the stream ends and only
// then reduced, which otherwise counts cache hits twice.
type streamUsageReader struct {
	streamEndReader
	kind    usageStreamKind
	fold    func(*streamUsageReader, string, map[string]any)
	usage   TokenUsage
	claude  claudeUsageTotals
	primed  bool   // the first payload was a whole JSON body, not one of its chunks
	pending []byte // an SSE line cut by a chunk boundary
}

// maxSSELineBytes bounds the partial line held between chunks.
const maxSSELineBytes = 1024 * 1024

func newStreamUsageReader(kind usageStreamKind) *streamUsageReader {
	return &streamUsageReader{kind: kind, fold: kind.folder()}
}

// maxFinalFallbackBytes bounds the body an unfoldable reader is handed back at
// the end. Passthrough streams hand it the capped log copy (256KB), so a longer
// answer must not be read as if it were the whole thing.
const maxFinalFallbackBytes = passThroughLogBufferMax

func (r *streamUsageReader) Write(p []byte) (int, error) {
	_, _ = r.streamEndReader.Write(p)
	r.consume(p)
	return len(p), nil
}

// cut is this stream's account of how it ended, for the closing frame.
func (r *streamUsageReader) cut(parseUsage func([]byte) TokenUsage, logTail []byte) streamCut {
	return r.streamEndReader.cut(r.final(parseUsage, logTail), r.streamEndReader.seen, logTail)
}

// final is the usage of the whole stream. A frame the upstream cut before its
// newline is still folded: it is all the client got.
//
// An unfoldable kind, and a foldable one that folded nothing (the upstream
// sent no usage), has no incremental total, so it falls back to the caller's
// whole-body parser — but only on a body as long as the stream
// (streamEndReader.seen counts every byte, body may be a capped copy). A
// longer stream parses to a truncated prefix that would under-count without
// saying so; it reports nothing instead, so the caller's own estimate applies.
func (r *streamUsageReader) final(parseUsage func([]byte) TokenUsage, body []byte) TokenUsage {
	if len(r.pending) > 0 {
		trailing := append([]byte(nil), r.pending...)
		r.pending = r.pending[:0]
		r.consume(append(trailing, byte(10)))
	}
	if r.fold != nil {
		var usage TokenUsage
		if r.claude.used {
			usage = r.claude.result()
		} else {
			usage = r.usage
		}
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheTokens > 0 {
			return usage
		}
		// 折叠一无所获（上游流里没带 usage，或只有被 JSON 整流掉的计数事件）：
		// 与不可折叠的流一样退回整段解析，别让上层退到 len/4 的假账。
	}
	if parseUsage == nil || int64(len(body)) < r.streamEndReader.seen || len(body) > maxFinalFallbackBytes {
		return TokenUsage{}
	}
	if r.fold == nil {
		// 不可折叠的流：非流式的 JSON 响应（如转录）照旧整段解析。
		return parseUsage(body)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] == '{' && json.Valid(trimmed)) {
		// 可折叠的流走到这里，说明它一行 data: 都没有、整段就是一个 JSON 文档：
		// 那是上游的错误体（配额耗尽的报错会引用一份 usage），不是流，解析它
		// 等于把没花的 token 记成花了。
		return TokenUsage{}
	}
	return parseUsage(body)
}

func isSSEDataLine(line []byte) bool {
	return len(line) >= 5 && bytes.HasPrefix(line, []byte("data:"))
}

func (r *streamUsageReader) event(payload []byte) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return
	}
	// The first payload of a JSON body is the whole document. Folding an error
	// body per event would report its embedded usage (an exhausted-quota error
	// quotes one) as if tokens had been spent.
	eventType, usage := usageEvent(payload)
	if !r.primed {
		r.primed = true
		if usage == nil && eventType == "" && payload[0] == '{' && json.Valid(payload) {
			return
		}
	}
	r.fold(r, eventType, usage)
}

// consume folds every SSE data line completed by p. A line cut by a chunk
// boundary is held until the rest of it arrives, so the facts that ride on
// chunking (can this payload be parsed?) are the same either way.
func (r *streamUsageReader) consume(p []byte) {
	if r.fold == nil {
		return
	}
	// A chunk that ends on a line boundary hands over everything it holds; a
	// cut line is what the next chunk completes.
	if len(r.pending) > 0 {
		head := r.pending
		r.pending = make([]byte, 0, len(head)+len(p))
		r.pending = append(r.pending, head...)
	}
	r.pending = append(r.pending, p...)
	r.pending = r.pending[:copy(r.pending, r.scan(r.pending))]
	r.trimPending()
}

// scan folds every line of data that ends in a newline and returns the tail
// that is still incomplete.
func (r *streamUsageReader) scan(data []byte) []byte {
	for {
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			return data
		}
		r.consider(bytes.TrimSuffix(data[:index], []byte("\r")))
		data = data[index+1:]
	}
}

func (r *streamUsageReader) trimPending() {
	if len(r.pending) > maxSSELineBytes {
		// Not a stream we can frame; stop growing rather than hold the tail of
		// a body that is not line-delimited.
		r.pending = r.pending[:0]
	}
}

// consider folds one SSE line, if it is a data line worth decoding. A line
// without a usage object — nearly every chunk of an answer — costs one
// substring search and nothing else.
func (r *streamUsageReader) consider(line []byte) {
	if len(line) == 0 || !ssLineStart(line[0]) {
		return
	}
	trimmed := bytes.TrimSpace(line)
	if !isSSEDataLine(trimmed) {
		return
	}
	if !bytes.Contains(line, usageField) {
		return
	}
	r.event(trimmed[len(dataPrefix):])
}

// ssLineStart reports whether a raw SSE line can still be a data line: one that
// starts with the 'd' of "data:", or with the indentation before it.
func ssLineStart(first byte) bool {
	return first == 'd' || first == ' ' || first == '	'
}

var (
	dataPrefix = []byte("data:")
	usageField = []byte(`"usage"`)
)

// foldRunningTotalEvent keeps the last event that reports usage. OpenAI sends
// it on the final chunk, with an empty choices array; Responses on its terminal
// event, which is "response.completed" or — a turn the vendor stopped at
// max_output_tokens — "response.incomplete". A figure an event carries
// otherwise (the input count on Responses' "in_progress" event) is not the
// final one.
func foldRunningTotalEvent(r *streamUsageReader, eventType string, usage map[string]any) {
	if usage == nil || (eventType != "" && !strings.HasSuffix(eventType, ".completed") && eventType != "response.incomplete" && eventType != "response.failed") {
		return
	}
	if parsed := openAIUsageFromMap(usage); parsed.InputTokens > 0 || parsed.OutputTokens > 0 || parsed.CacheTokens > 0 {
		r.usage = parsed
	}
}

// foldResponsesEvent folds a Responses stream's usage. Its terminal event is
// the only one that reports the final figures, and it can be response.completed
// or — a turn the vendor stopped at max_output_tokens — response.incomplete; a
// figure an earlier event carries is not final. The figures themselves use the
// Responses names (input_tokens, output_tokens), not Chat's.
func foldResponsesEvent(r *streamUsageReader, eventType string, usage map[string]any) {
	if usage == nil || (eventType != "" && !strings.HasSuffix(eventType, ".completed") && eventType != "response.incomplete" && eventType != "response.failed") {
		return
	}
	if parsed := responsesUsageFromUsageMap(usage); parsed.InputTokens > 0 || parsed.OutputTokens > 0 || parsed.CacheTokens > 0 {
		r.usage = parsed
	}
}

// claudeUsageTotals are the raw Anthropic fields, kept unreduced until the
// whole stream has been read.
type claudeUsageTotals struct {
	used                                  bool
	prompt, output, cacheRead, cacheWrite int64
}

func foldClaudeEvent(r *streamUsageReader, _ string, usage map[string]any) {
	if usage != nil {
		r.claude.used = true
		r.claude.prompt, r.claude.output, r.claude.cacheRead, r.claude.cacheWrite =
			mergeClaudePassthroughUsage(r.claude.prompt, r.claude.output, r.claude.cacheRead, r.claude.cacheWrite, usage)
	}
}

func (t claudeUsageTotals) result() TokenUsage {
	return normalizeClaudeUsageFields(t.prompt, t.output, t.cacheRead, t.cacheWrite)
}
