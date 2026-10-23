package gateway

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseClaudeUsageFromSSE(t *testing.T) {
	body := []byte(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":22707,"output_tokens":42,"cache_read_input_tokens":18432}}

`)
	usage := ParseClaudeUsage(body)
	// Claude input_tokens is exclusive of cache; gateway normalizes to inclusive total.
	if usage.InputTokens != 22707+18432 {
		t.Fatalf("unexpected input tokens: %d", usage.InputTokens)
	}
	if usage.OutputTokens != 42 {
		t.Fatalf("unexpected output tokens: %d", usage.OutputTokens)
	}
	if usage.CacheTokens != 18432 {
		t.Fatalf("unexpected cache tokens: %d", usage.CacheTokens)
	}
}

func TestNormalizeClaudeUsageFields(t *testing.T) {
	usage := normalizeClaudeUsageFields(678, 1133, 9218, 0)
	if usage.InputTokens != 678+9218 {
		t.Fatalf("expected inclusive input, got %d", usage.InputTokens)
	}
	if usage.CacheTokens != 9218 {
		t.Fatalf("unexpected cache: %d", usage.CacheTokens)
	}
	if got := claudeExclusiveInputTokens(usage, 0); got != 678 {
		t.Fatalf("expected exclusive input 678, got %d", got)
	}
}

func TestParseOpenAIUsageFromSSE(t *testing.T) {
	body := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":8,"prompt_cache_hit_tokens":96}}

data: [DONE]
`)
	usage := ParseOpenAIUsage(body)
	if usage.InputTokens != 120 {
		t.Fatalf("unexpected input tokens: %d", usage.InputTokens)
	}
	if usage.OutputTokens != 8 {
		t.Fatalf("unexpected output tokens: %d", usage.OutputTokens)
	}
	if usage.CacheTokens != 96 {
		t.Fatalf("unexpected cache tokens: %d", usage.CacheTokens)
	}
}

func TestParseOpenAIUsageFromJSONWithCachedTokensDetails(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":200,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":150}}}`)
	usage := ParseOpenAIUsage(body)
	if usage.InputTokens != 200 {
		t.Fatalf("expected inclusive input tokens: %d", usage.InputTokens)
	}
	if usage.CacheTokens != 150 {
		t.Fatalf("unexpected cache tokens: %d", usage.CacheTokens)
	}
}

func TestParseOpenAIUsageExclusivePromptTokens(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":96,"prompt_cache_miss_tokens":4}}`)
	usage := ParseOpenAIUsage(body)
	if usage.InputTokens != 100 {
		t.Fatalf("expected inclusive input from miss+cache, got %d", usage.InputTokens)
	}
	if usage.CacheTokens != 96 {
		t.Fatalf("unexpected cache tokens: %d", usage.CacheTokens)
	}
}

func TestParseOpenAIUsageExclusivePromptWithoutMiss(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":17900000,"completion_tokens":1,"prompt_cache_hit_tokens":188800000}}`)
	usage := ParseOpenAIUsage(body)
	if usage.InputTokens != 17900000+188800000 {
		t.Fatalf("expected prompt+cache inclusive input, got %d", usage.InputTokens)
	}
	if usage.CacheTokens != 188800000 {
		t.Fatalf("unexpected cache tokens: %d", usage.CacheTokens)
	}
}

func TestNormalizeInclusiveInputTokens(t *testing.T) {
	if got := normalizeInclusiveInputTokens(120, 96, 0, 0); got != 120 {
		t.Fatalf("expected inclusive prompt to stay %d, got %d", 120, got)
	}
	if got := normalizeInclusiveInputTokens(10, 96, 0, 4); got != 100 {
		t.Fatalf("expected miss+cache total 100, got %d", got)
	}
	if got := normalizeInclusiveInputTokens(678, 9218, 0, 0); got != 678+9218 {
		t.Fatalf("expected exclusive prompt+cache, got %d", got)
	}
}

// A long stream (past the 256KB log cap) must still report the usage it
// carries at the end: the figures come from every forwarded chunk, not from
// the capped copy kept for logs.
func TestStreamUsageReaderReportsUsageBeyondLogCap(t *testing.T) {
	prime := []byte(`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90}}}` + "\n\n")
	delta := []byte(`event: message_delta` + "\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n")
	chunk := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n")
	reader := newStreamUsageReader(usageStreamClaude)
	if _, err := reader.Write(prime); err != nil {
		t.Fatal(err)
	}
	reader.Write(bytes.Repeat(chunk, passThroughLogBufferMax/len(chunk)+16))
	reader.Write(delta)
	usage := reader.final(ParseClaudeUsage, []byte("truncated"))
	if usage.InputTokens != 100 || usage.CacheTokens != 90 || usage.OutputTokens != 42 {
		t.Fatalf("usage beyond the log cap lost: %+v", usage)
	}
}

// The same guarantee for an OpenAI stream whose usage chunk follows a long
// content run.
func TestStreamUsageReaderOpenAILateUsage(t *testing.T) {
	reader := newStreamUsageReader(usageStreamOpenAI)
	chunk := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	reader.Write(bytes.Repeat(chunk, passThroughLogBufferMax/len(chunk)+16))
	reader.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":3}}\n\n"))
	reader.Write([]byte("data: [DONE]\n\n"))
	usage := reader.final(ParseOpenAIUsage, nil)
	if usage.InputTokens != 11 || usage.OutputTokens != 7 || usage.CacheTokens != 3 {
		t.Fatalf("late usage chunk lost: %+v", usage)
	}
}

// The reader rides on the copy, so the figures must not depend on how the
// stream happened to be chunked by the transport.
func TestStreamUsageReaderIgnoresChunkBoundaries(t *testing.T) {
	ops := []byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":7}}}` + "\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\n\n")
	want := newStreamUsageReader(usageStreamClaude)
	want.Write(ops)
	split := newStreamUsageReader(usageStreamClaude)
	for _, b := range ops {
		split.Write([]byte{b})
	}
	if got, expected := split.final(ParseClaudeUsage, nil), want.final(ParseClaudeUsage, nil); got != expected || expected.InputTokens != 12 || expected.OutputTokens != 9 {
		t.Fatalf("split write changed usage: got %+v want %+v", got, expected)
	}
}

// An error body is not a stream, even though it may quote a usage object: an
// exhausted-quota message does, and billing it as spent tokens would be wrong.
func TestStreamUsageReaderIgnoresWholeJSONBody(t *testing.T) {
	reader := newStreamUsageReader(usageStreamOpenAI)
	body := []byte(`{"error":{"message":"quota"},"usage":{"prompt_tokens":99,"completion_tokens":99}}`)
	reader.Write(body)
	if usage := reader.final(ParseOpenAIUsage, body); usage.OutputTokens == 99 {
		t.Fatalf("error body counted as usage: %+v", usage)
	}
}

// A data frame the upstream cut before its newline is all the client got, so
// the usage it reports is folded like any other: the cut itself is what the
// interruption is for, not a reason to drop the figures.
func TestStreamUsageReaderFoldsTrailingFrameWithoutNewline(t *testing.T) {
	reader := newStreamUsageReader(usageStreamOpenAI)
	reader.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6}}"))
	if got := reader.final(ParseOpenAIUsage, nil); got.InputTokens != 4 || got.OutputTokens != 6 {
		t.Fatalf("trailing frame lost: %+v", got)
	}
}

// An unfoldable stream never reads its usage back from the capped log copy: a
// body shorter than what the reader saw is a truncated prefix, and parsing it
// would under-count without saying so.
func TestStreamUsageReaderUnfoldableDoesNotParseTruncatedPrefix(t *testing.T) {
	reader := newStreamUsageReader(usageStreamUnfoldable)
	full := []byte(`{"usage":{"input_tokens":4,"output_tokens":6}}`)
	prefix := append(append([]byte(nil), full...), bytes.Repeat([]byte(" "), 64)...)
	reader.Write(prefix)
	if got := reader.final(ParseResponsesUsage, full); got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("truncated prefix counted as usage: %+v", got)
	}
	// A body as long as the stream (the non-stream path) still parses.
	whole := newStreamUsageReader(usageStreamUnfoldable)
	whole.Write(full)
	if got := whole.final(ParseResponsesUsage, full); got.InputTokens != 4 || got.OutputTokens != 6 {
		t.Fatalf("whole body not parsed: %+v", got)
	}
}

// An unfoldable stream with no whole body to parse reports nothing; its
// caller estimates instead.
func TestStreamUsageReaderUnfoldableWithoutParser(t *testing.T) {
	reader := newStreamUsageReader(usageStreamUnfoldable)
	reader.Write([]byte("some forwarded bytes"))
	if got := reader.final(nil, nil); got != (TokenUsage{}) {
		t.Fatalf("no parser must report nothing, got %+v", got)
	}
}

// A long stream's remaining bytes must stop piling up in the line buffer: the
// buffer only ever holds the unfinished tail.
func TestStreamUsageReaderPendingStaysSmall(t *testing.T) {
	reader := newStreamUsageReader(usageStreamOpenAI)
	chunk := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	for index := 0; index < 4000; index++ {
		reader.Write(chunk)
	}
	if len(reader.pending) != 0 {
		t.Fatalf("completed lines must leave no residue, held %d bytes", len(reader.pending))
	}
	reader.Write([]byte("data: {\"partial\":"))
	if len(reader.pending) != len("data: {\"partial\":") {
		t.Fatalf("only the unfinished tail should be held, got %d", len(reader.pending))
	}
}

// Each protocol's closing frame is recognised from the streamed bytes alone.
func TestStreamEndReaderSeesClosingFrames(t *testing.T) {
	cases := []struct {
		name  string
		want  bool
		parts []string
	}{
		{name: "chat done", want: true, parts: []string{"data: {\"choices\":[]}\n\n", "data: [DONE]\n\n"}},
		{name: "chat done split", want: true, parts: []string{"data: [DO", "NE]\n\n"}},
		{name: "chat cut", want: false, parts: []string{"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"}},
		{name: "chat cut mid frame", want: false, parts: []string{"data: {\"choices\":[]}\n\n", "data: {\"choi"}},
		{name: "chat quoting done", want: false, parts: []string{"data: {\"choices\":[],\"text\":\"[DONE]\",\"pad\":\"" + strings.Repeat("p", 400) + "\"}\n\n"}},
		{name: "responses completed", want: true, parts: []string{"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"}},
		{name: "claude stop", want: true, parts: []string{"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}},
	}
	for _, testCase := range cases {
		reader := &streamEndReader{}
		for _, part := range testCase.parts {
			reader.Write([]byte(part))
		}
		if got := reader.closed(); got != testCase.want {
			t.Fatalf("%s: closed()=%v, want %v", testCase.name, got, testCase.want)
		}
	}
}

// The Claude cut turn is folded per line, across chunk boundaries, so a stream
// closed from its own bytes stops exactly the blocks it left open.
func TestStreamEndReaderFoldsClaudeBlocksAcrossChunks(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2}\n\n"
	reader := &streamEndReader{}
	for _, b := range []byte(stream) {
		reader.Write([]byte{b})
	}
	cut := reader.cut(TokenUsage{}, int64(len(stream)), nil)
	if !cut.messageStarted {
		t.Fatal("message_start was not seen")
	}
	if len(cut.openBlocks) != 1 || cut.openBlocks[0] != 2 {
		t.Fatalf("open blocks %v, want [2]", cut.openBlocks)
	}
}
