package gateway

// Reading Qoder's answer.
//
// Each of Qoder's SSE frames carries an envelope whose "body" is a standard
// OpenAI chunk, so most of the reply is already in the shape the gateway
// relays. Two things are not: the envelope's own failure codes, and the fact
// that Qoder sometimes writes a tool call as XML inside the answer text rather
// than as a tool call. The splitter below lifts those back out while the text
// streams, and refuses to pass one on that it cannot read — a half-written call
// sent as the answer is a call the client would then try to run.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type qoderChunkReader struct {
	scanner   *bufio.Scanner
	id        string
	created   int64
	model     string
	out       [][]byte
	pos       int
	done      bool
	began     bool
	text      qoderText
	toolIndex int
	nextCall  int
	usage     map[string]any
	// finishReason is what the turn ended on, remembered until the last frame:
	// Qoder sends its usage after the finish, in a chunk with no choices.
	finishReason string
}

func newQoderChunkReader(body io.Reader, model string) *qoderChunkReader {
	scanner := bufio.NewScanner(body)
	// One frame can carry a whole tool call's argument blob.
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	return &qoderChunkReader{
		scanner:   scanner,
		id:        "chatcmpl-" + randomHex(6),
		created:   time.Now().Unix(),
		model:     model,
		toolIndex: -1,
	}
}

func (r *qoderChunkReader) Read(p []byte) (int, error) {
	for {
		if len(r.out) > 0 {
			n := copy(p, r.out[0][r.pos:])
			r.pos += n
			if r.pos >= len(r.out[0]) {
				r.out, r.pos = r.out[1:], 0
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		if r.done {
			return 0, io.EOF
		}
		if err := r.scanner.Err(); err != nil {
			r.emitError(fmt.Sprintf("Qoder：读取响应流失败：%v", err))
			continue
		}
		if !r.scanner.Scan() {
			r.finish()
			continue
		}
		r.consume(strings.TrimSpace(r.scanner.Text()))
	}
}

func (r *qoderChunkReader) Close() error { return nil }

// consume takes one SSE line. Qoder frames its replies as plain "data:" lines,
// and a line that is not one — an event name, a comment, a keep-alive — carries
// nothing an OpenAI client needs.
func (r *qoderChunkReader) consume(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || !json.Valid([]byte(payload)) {
		return
	}
	var envelope struct {
		StatusCodeValue int             `json:"statusCodeValue"`
		Code            json.Number     `json:"code"`
		Message         string          `json:"message"`
		Body            json.RawMessage `json:"body"`
	}
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		return
	}
	// Qoder sends the chunk as a JSON *string* holding the chunk's own JSON, so
	// the field has to be unwrapped before it can be read; an envelope that
	// sends it as an object is taken as it stands.
	body := qoderEnvelopeBody(envelope.Body)
	if envelope.StatusCodeValue != 0 && envelope.StatusCodeValue != http.StatusOK {
		data := body
		if len(data) == 0 {
			data = []byte(payload)
		}
		status, message, wait := qoderFailure(envelope.StatusCodeValue, data)
		// The queue verdict lives on the envelope, not always in its body: a
		// body that is only the queue state names no code, so qoderFailure
		// read from it alone answers a plain 403 and the account looks broken.
		// Any frame whose envelope says 10605 is the model being busy, never
		// the account — ask again with the whole frame and keep the wait.
		if qoderEnvelopeQueued(envelope.Code, data) && wait == 0 {
			if s2, m2, w2 := qoderFailure(envelope.StatusCodeValue, []byte(payload)); w2 > 0 {
				status, message, wait = s2, m2, w2
			}
		}
		r.emitErrorQueued(fmt.Sprintf("%s（HTTP %d）", message, status), wait)
		return
	}
	if strings.TrimSpace(string(body)) == "[DONE]" {
		r.finish()
		return
	}
	if len(body) == 0 || !json.Valid(body) {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(body, &chunk) != nil {
		return
	}
	if len(chunk.Choices) == 0 {
		if chunk.Usage != nil {
			r.usage = chunk.Usage
		}
		return
	}
	delta := chunk.Choices[0].Delta
	if chunk.Usage != nil {
		r.usage = chunk.Usage
	}
	if delta.ReasoningContent != "" {
		r.emit(map[string]any{"reasoning_content": delta.ReasoningContent})
	}
	if delta.Content != "" {
		for _, fragment := range r.text.feed(delta.Content) {
			if !r.emitFragment(fragment) {
				return
			}
		}
	}
	// Qoder also serves tool calls the OpenAI way. The id it sends is the one a
	// result has to be answered with, so it is kept rather than replaced.
	for _, call := range delta.ToolCalls {
		index := r.toolIndex
		if call.Index != nil {
			index = *call.Index
		}
		if index != r.toolIndex {
			r.flushText()
			r.toolIndex = index
			id := strings.TrimSpace(call.ID)
			if id == "" {
				id = "call_" + randomHex(16)
			}
			r.emit(map[string]any{"tool_calls": []any{map[string]any{
				"index": r.nextCall, "id": id, "type": "function",
				"function": map[string]any{"name": call.Function.Name, "arguments": ""},
			}}})
			r.nextCall++
			r.text.sawTool = true
		}
		if call.Function.Arguments != "" {
			r.emit(map[string]any{"tool_calls": []any{map[string]any{
				"index": r.nextCall - 1,
				"function": map[string]any{
					"arguments": call.Function.Arguments,
				},
			}}})
		}
	}
	if chunk.Choices[0].FinishReason != "" {
		// The finish reason does not end the reading: Qoder sends its usage in
		// a chunk of its own afterwards, with no choices at all. Stopping here
		// is what made every Qoder request count as 0 tokens.
		r.finishReason = chunk.Choices[0].FinishReason
	}
}

// emitFragment passes on one piece of the split text: a lifted tool call, the
// text itself, or a call that cannot be read. It reports whether the turn may
// go on: an error ends it without a finish, so a client can try again.
func (r *qoderChunkReader) emitFragment(fragment qoderFragment) bool {
	switch {
	case fragment.err != "":
		r.emitError(fragment.err)
		return false
	case fragment.call != nil:
		r.emit(map[string]any{"tool_calls": []any{map[string]any{
			"index": r.nextCall, "id": "call_" + randomHex(16), "type": "function",
			"function": map[string]any{"name": fragment.call.name, "arguments": ""},
		}}})
		r.emit(map[string]any{"tool_calls": []any{map[string]any{
			"index":    r.nextCall,
			"function": map[string]any{"arguments": fragment.call.arguments},
		}}})
		r.nextCall++
		return true
	case fragment.text != "":
		r.emit(map[string]any{"content": fragment.text})
	}
	return true
}

// emit appends one OpenAI chunk carrying this delta.
func (r *qoderChunkReader) emit(delta map[string]any) {
	if !r.began {
		r.began = true
		r.writeChunk(map[string]any{"role": "assistant", "content": ""}, "", nil)
	}
	r.writeChunk(delta, "", nil)
}

func (r *qoderChunkReader) writeChunk(delta map[string]any, finishReason string, usage map[string]any) {
	if delta == nil {
		delta = map[string]any{}
	}
	chunk := map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nilIfEmpty(finishReason)}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	r.out = append(r.out, []byte("data: "+string(raw)+"\n\n"))
}

// flushText gives out whatever the splitter was still holding: a call that the
// stream ended inside is resolved here, not left unsaid.
func (r *qoderChunkReader) flushText() {
	for _, fragment := range r.text.flush() {
		if !r.emitFragment(fragment) {
			return
		}
	}
}

// finish closes the stream: the turn's last chunk, with its finish reason and
// the usage Qoder reported, then the sentinel.
func (r *qoderChunkReader) finish() {
	if r.done {
		return
	}
	r.flushText()
	if r.done {
		return
	}
	finish := "stop"
	switch {
	case r.text.sawTool || r.toolIndex >= 0:
		finish = "tool_calls"
	case r.finishReason == "length":
		// A reply cut off by the token budget is not a finished one: the agent
		// has to see that it stopped early.
		finish = "length"
	}
	r.began = true
	r.writeChunk(map[string]any{}, finish, r.usage)
	r.out = append(r.out, []byte("data: [DONE]\n\n"))
	r.done = true
}

func (r *qoderChunkReader) emitError(message string) {
	if r.done {
		return
	}
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "api_error"}})
	r.out = append(r.out, []byte("data: "+string(payload)+"\n\n"))
	r.done = true
}

// emitErrorQueued is emitError for a refusal the vendor framed as "the model is
// busy" (Qoder's 10605). The type carries that verdict to the failover loop,
// which must move on to the next account without counting this one as failed:
// a queued model answers every account the same way, so cooling this account
// down would only send the next request into the same queue with fewer places
// left to try.
func (r *qoderChunkReader) emitErrorQueued(message string, wait time.Duration) {
	if r.done {
		return
	}
	errType := "api_error"
	if wait > 0 {
		errType = upstreamBusyErrorType
	}
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": errType}})
	r.out = append(r.out, []byte("data: "+string(payload)+"\n\n"))
	r.done = true
}

// qoderEnvelopeQueued reports whether a failure frame names the model as busy.
// Qoder puts the code on the envelope and sometimes leaves only the queue state
// inside body, so both places are read.
func qoderEnvelopeQueued(code json.Number, body []byte) bool {
	if strings.TrimSpace(code.String()) == "10605" {
		return true
	}
	var state struct {
		IsQueued          bool  `json:"isQueued"`
		ServiceAvailable  *bool `json:"serviceAvailable"`
		RetryAfterSeconds int   `json:"retryAfterSeconds"`
	}
	if json.Unmarshal(body, &state) != nil {
		return false
	}
	return state.IsQueued || state.RetryAfterSeconds > 0 || (state.ServiceAvailable != nil && !*state.ServiceAvailable)
}

// qoderEnvelopeBody unwraps the chunk a Qoder frame carries: Qoder sends it as
// a JSON string holding the chunk's own JSON.
func qoderEnvelopeBody(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return []byte(asString)
	}
	return raw
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// qoderText is the streaming tool-call splitter. It holds text back until a
// whole call block arrives, then emits it as a call. A block on its way to being
// a call that cannot be read, or that the stream ends inside, is an error and
// never the answer; a block whose JSON is whole when the stream simply stops is
// still the call. A tag inside code or prose stays text.
type qoderText struct {
	textBuf string
	callBuf string
	inCall  bool
	sawTool bool
	sent    string
}

type qoderFragment struct {
	text string
	call *qoderLiftedCall
	err  string
}

type qoderLiftedCall struct {
	name      string
	arguments string
}

const (
	qoderCallUnreadable = "Qoder：模型把工具调用写成了文本，但读不出来；不把它作为回答发出"
	qoderCallCutShort   = "Qoder：模型把工具调用写成了文本，还没写完流就结束了；不把它作为回答发出"
)

func (a *qoderText) feed(input string) []qoderFragment {
	var out []qoderFragment
	for input != "" {
		if !a.inCall {
			combined := a.textBuf + input
			a.textBuf = ""
			index := strings.Index(combined, qoderCallOpen)
			if index < 0 {
				keep := qoderCallSuffix(combined)
				if keep < len(combined) {
					give := combined[:len(combined)-keep]
					a.text(give)
					out = append(out, qoderFragment{text: give})
					a.textBuf = combined[len(combined)-keep:]
				} else {
					a.textBuf = combined
				}
				break
			}
			if index > 0 {
				a.text(combined[:index])
				out = append(out, qoderFragment{text: combined[:index]})
			}
			input = combined[index+len(qoderCallOpen):]
			a.inCall = true
			continue
		}
		combined := a.callBuf + input
		a.callBuf = ""
		end := strings.Index(combined, qoderCallClose)
		if end < 0 {
			a.callBuf = combined
			break
		}
		body := combined[:end]
		a.inCall = false
		if call, ok := qoderParseCall(body); ok {
			a.sawTool = true
			out = append(out, qoderFragment{call: &call})
		} else if qoderCallish(body) && !qoderQuoted(a.sent) {
			return append(out, qoderFragment{err: qoderCallUnreadable})
		} else {
			literal := qoderCallOpen + body + qoderCallClose
			a.text(literal)
			out = append(out, qoderFragment{text: literal})
		}
		input = combined[end+len(qoderCallClose):]
	}
	return out
}

// text remembers what has been given out: whether the next block is inside code
// is told from what came before it.
func (a *qoderText) text(given string) { a.sent += given }

func (a *qoderText) flush() []qoderFragment {
	if a.inCall {
		body := a.callBuf
		a.inCall, a.callBuf = false, ""
		if qoderCallish(body) && !qoderQuoted(a.sent) {
			if call, ok := qoderParseCall(body); ok {
				a.sawTool = true
				return []qoderFragment{{call: &call}}
			}
			return []qoderFragment{{err: qoderCallCutShort}}
		}
		literal := qoderCallOpen + body
		a.text(literal)
		if literal != "" {
			return []qoderFragment{{text: literal}}
		}
	}
	if a.textBuf != "" {
		given := a.textBuf
		a.textBuf = ""
		a.text(given)
		return []qoderFragment{{text: given}}
	}
	return nil
}

// qoderCallSuffix is the length of the tail that could still become a call
// marker — a frame that ends mid-tag must not leak the partial tag as text.
func qoderCallSuffix(value string) int {
	for size := len(qoderCallOpen) - 1; size > 0; size-- {
		if strings.HasSuffix(value, qoderCallOpen[:size]) {
			return size
		}
	}
	return 0
}

// qoderCallish says a block was on its way to being a call: its JSON began, or
// the XML function form appeared.
func qoderCallish(value string) bool {
	return strings.HasPrefix(strings.TrimLeft(value, " \t\r\n"), "{") || strings.Contains(value, "\x3cfunction=")
}

// qoderQuoted says the text ends inside a code fence or an inline code span: a
// tool-call block there is prose about one, not a call.
func qoderQuoted(text string) bool {
	if strings.Count(text, "```")%2 == 1 {
		return true
	}
	line := text[strings.LastIndex(text, "\n")+1:]
	return strings.Count(strings.ReplaceAll(line, "```", ""), "`")%2 == 1
}

// qoderParseCall reads one tool-call block: a JSON {name, arguments}, or the
// XML function/parameter form.
func qoderParseCall(value string) (qoderLiftedCall, bool) {
	var framed struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(value)), &framed) == nil && strings.TrimSpace(framed.Name) != "" {
		var arguments map[string]json.RawMessage
		if json.Unmarshal(framed.Arguments, &arguments) == nil && arguments != nil {
			return qoderLiftedCall{name: strings.TrimSpace(framed.Name), arguments: string(framed.Arguments)}, true
		}
	}
	match := qoderFuncRe.FindStringSubmatch(value)
	if len(match) != 3 {
		return qoderLiftedCall{}, false
	}
	name := strings.TrimSpace(match[1])
	if name == "" {
		return qoderLiftedCall{}, false
	}
	arguments := map[string]any{}
	for _, parameter := range qoderParamRe.FindAllStringSubmatch(match[2], -1) {
		key := strings.TrimSpace(parameter[1])
		if key == "" {
			continue
		}
		raw := strings.TrimSpace(parameter[2])
		var decoded any
		if json.Unmarshal([]byte(raw), &decoded) != nil {
			decoded = raw
		}
		arguments[key] = decoded
	}
	payload, err := json.Marshal(arguments)
	if err != nil {
		return qoderLiftedCall{}, false
	}
	return qoderLiftedCall{name: name, arguments: string(payload)}, true
}
