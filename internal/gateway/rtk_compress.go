package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// RTK (rtk-ai.app) rewrites/compresses shell output before an agent reads it.
// The gateway cannot install per-agent hooks, but it can apply the same rtk
// pipe filters to tool_result content it is about to forward. This is opt-in
// per provider because the compression is lossy.
const (
	rtkMinCompressBytes = 800
	rtkMaxCompressBytes = 2 << 20
	rtkCommandTimeout   = 6 * time.Second
	rtkCacheMaxEntries  = 256
	rtkCacheMaxBytes    = 16 << 20
	rtkMaxConcurrent    = 4
	rtkAvailabilityTTL  = 30 * time.Second
)

type rtkCompressionStats struct {
	Commands         int64
	OriginalBytes    int64
	CompressedBytes  int64
	OriginalTokens   int64
	CompressedTokens int64
}

func (s *rtkCompressionStats) addSavings(original, compressed string) {
	s.OriginalBytes += int64(len(original))
	s.CompressedBytes += int64(len(compressed))
	s.OriginalTokens += estimateRTKTokens(original)
	s.CompressedTokens += estimateRTKTokens(compressed)
}

type rtkCompressor struct {
	mu        sync.Mutex
	path      string
	version   string
	checkedAt time.Time
	cache     map[[32]byte]string
	cacheKeys [][32]byte
	cacheSize int
	sem       chan struct{}
	runner    func(context.Context, string, string, []byte) ([]byte, error)
}

func newRTKCompressor() *rtkCompressor {
	return &rtkCompressor{
		cache:  make(map[[32]byte]string),
		sem:    make(chan struct{}, rtkMaxConcurrent),
		runner: runRTKCLI,
	}
}

// status resolves the rtk binary lazily. The first call after a miss re-checks
// after a short TTL so installing rtk does not require a gateway restart.
func (c *rtkCompressor) status() (bool, string) {
	if c == nil {
		return false, ""
	}
	c.ensure()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path != "", c.version
}

func (c *rtkCompressor) ensure() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.path != "" || (!c.checkedAt.IsZero() && time.Since(c.checkedAt) < rtkAvailabilityTTL) {
		c.mu.Unlock()
		return
	}
	c.checkedAt = time.Now()
	c.mu.Unlock()

	rtkPath, version := resolveRTKBinary()

	c.mu.Lock()
	c.path = rtkPath
	c.version = version
	c.mu.Unlock()
}

// resolveRTKBinary probes the explicit path first, then PATH, then the usual
// per-user install locations. The fallback matters on hosts where the gateway
// was started before rtk was installed: an existing process keeps its original
// PATH, so LookPath alone would never see the new shim without a restart.
func resolveRTKBinary() (string, string) {
	for _, candidate := range rtkCandidatePaths() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := exec.CommandContext(ctx, candidate, "--version").Output()
		cancel()
		if err == nil {
			return candidate, strings.TrimSpace(string(out))
		}
	}
	return "", ""
}

func rtkCandidatePaths() []string {
	if explicit := strings.TrimSpace(os.Getenv("GATEWAY_RTK_PATH")); explicit != "" {
		return []string{explicit}
	}
	var candidates []string
	seen := map[string]bool{}
	add := func(candidate string) {
		if candidate == "" || seen[candidate] {
			return
		}
		seen[candidate] = true
		candidates = append(candidates, candidate)
	}
	if found, err := exec.LookPath("rtk"); err == nil {
		add(found)
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		for _, dir := range []string{
			joinNonEmpty(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Links"),
			joinNonEmpty(os.Getenv("USERPROFILE"), "scoop", "shims"),
			joinNonEmpty(home, ".cargo", "bin"),
			joinNonEmpty(os.Getenv("LOCALAPPDATA"), "Programs", "rtk"),
			joinNonEmpty(os.Getenv("ProgramFiles"), "rtk"),
			joinNonEmpty(os.Getenv("ProgramData"), "chocolatey", "bin"),
		} {
			if dir != "" {
				add(filepath.Join(dir, "rtk.exe"))
			}
		}
		return candidates
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		add(filepath.Join(dir, "rtk"))
	}
	if home != "" {
		add(filepath.Join(home, ".local", "bin", "rtk"))
		add(filepath.Join(home, ".cargo", "bin", "rtk"))
	}
	return candidates
}

// joinNonEmpty keeps filepath.Join from producing a cwd-relative path when the
// base environment variable is unset.
func joinNonEmpty(base string, elems ...string) string {
	if strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, elems...)...)
}

func (c *rtkCompressor) compressRequest(req map[string]any, protocol domain.Protocol) (map[string]any, rtkCompressionStats) {
	if c == nil || req == nil {
		return req, rtkCompressionStats{}
	}
	if available, _ := c.status(); !available {
		return req, rtkCompressionStats{}
	}
	switch protocol {
	case domain.ProtocolClaude:
		return c.compressClaudeRequest(req)
	case domain.ProtocolOpenAIChat:
		return c.compressOpenAIChatRequest(req)
	case domain.ProtocolOpenAIResponses:
		return c.compressOpenAIResponsesRequest(req)
	default:
		return req, rtkCompressionStats{}
	}
}

func (c *rtkCompressor) compressClaudeRequest(req map[string]any) (map[string]any, rtkCompressionStats) {
	stats := rtkCompressionStats{}
	messages, ok := req["messages"].([]any)
	if !ok || len(messages) == 0 {
		return req, stats
	}
	commands := claudeToolCommands(messages)
	if len(commands) == 0 {
		return req, stats
	}

	replaced := make([]any, len(messages))
	copy(replaced, messages)
	changed := false
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		newContent := make([]any, len(content))
		copy(newContent, content)
		contentChanged := false
		for blockIndex, blockRaw := range content {
			block, ok := blockRaw.(map[string]any)
			if !ok || stringValue(block["type"]) != "tool_result" {
				continue
			}
			// A failed tool result is exactly where the model needs the raw
			// error text; never compress it.
			if isErrorResult, _ := block["is_error"].(bool); isErrorResult {
				continue
			}
			command, ok := commands[stringValue(block["tool_use_id"])]
			if !ok {
				continue
			}
			filter, ok := rtkFilterForCommand(command)
			if !ok {
				continue
			}
			if newBlock := c.compressClaudeToolResultBlock(block, filter, &stats); newBlock != nil {
				newContent[blockIndex] = newBlock
				contentChanged = true
			}
		}
		if contentChanged {
			newMessage := cloneMap(message)
			newMessage["content"] = newContent
			replaced[index] = newMessage
			changed = true
		}
	}
	if !changed {
		return req, stats
	}
	out := cloneMap(req)
	out["messages"] = replaced
	return out, stats
}

func claudeToolCommands(messages []any) map[string]string {
	out := make(map[string]string)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, blockRaw := range content {
			block, ok := blockRaw.(map[string]any)
			if !ok || stringValue(block["type"]) != "tool_use" {
				continue
			}
			if !isShellToolName(stringValue(block["name"])) {
				continue
			}
			id := strings.TrimSpace(stringValue(block["id"]))
			input, _ := block["input"].(map[string]any)
			command := strings.TrimSpace(stringValue(input["command"]))
			if command == "" {
				command = strings.TrimSpace(stringValue(input["cmd"]))
			}
			if id != "" && command != "" {
				out[id] = command
			}
		}
	}
	return out
}

func (c *rtkCompressor) compressClaudeToolResultBlock(block map[string]any, filter string, stats *rtkCompressionStats) map[string]any {
	switch content := block["content"].(type) {
	case string:
		compressed, ok := c.compressText(filter, content)
		if !ok {
			return nil
		}
		stats.Commands++
		stats.addSavings(content, compressed)
		out := cloneMap(block)
		out["content"] = compressed
		return out
	case []any:
		newBlocks := make([]any, len(content))
		copy(newBlocks, content)
		changed := false
		for index, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok || stringValue(part["type"]) != "text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			compressed, ok := c.compressText(filter, text)
			if !ok {
				continue
			}
			stats.addSavings(text, compressed)
			newPart := cloneMap(part)
			newPart["text"] = compressed
			newBlocks[index] = newPart
			changed = true
		}
		if !changed {
			return nil
		}
		stats.Commands++
		out := cloneMap(block)
		out["content"] = newBlocks
		return out
	}
	return nil
}

func (c *rtkCompressor) compressOpenAIChatRequest(req map[string]any) (map[string]any, rtkCompressionStats) {
	stats := rtkCompressionStats{}
	messages, ok := req["messages"].([]any)
	if !ok || len(messages) == 0 {
		return req, stats
	}
	commands := openAIChatToolCommands(messages)
	if len(commands) == 0 {
		return req, stats
	}

	replaced := make([]any, len(messages))
	copy(replaced, messages)
	changed := false
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || stringValue(message["role"]) != "tool" {
			continue
		}
		command, ok := commands[strings.TrimSpace(stringValue(message["tool_call_id"]))]
		if !ok {
			continue
		}
		filter, ok := rtkFilterForCommand(command)
		if !ok {
			continue
		}
		if newMessage := c.compressOpenAIChatToolMessage(message, filter, &stats); newMessage != nil {
			replaced[index] = newMessage
			changed = true
		}
	}
	if !changed {
		return req, stats
	}
	out := cloneMap(req)
	out["messages"] = replaced
	return out, stats
}

func openAIChatToolCommands(messages []any) map[string]string {
	out := make(map[string]string)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		calls, ok := message["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, callRaw := range calls {
			call, ok := callRaw.(map[string]any)
			if !ok {
				continue
			}
			function, _ := call["function"].(map[string]any)
			if !isShellToolName(stringValue(function["name"])) {
				continue
			}
			id := strings.TrimSpace(stringValue(call["id"]))
			command := commandFromToolArguments(stringValue(function["arguments"]))
			if id != "" && command != "" {
				out[id] = command
			}
		}
	}
	return out
}

func (c *rtkCompressor) compressOpenAIChatToolMessage(message map[string]any, filter string, stats *rtkCompressionStats) map[string]any {
	switch content := message["content"].(type) {
	case string:
		compressed, ok := c.compressText(filter, content)
		if !ok {
			return nil
		}
		stats.Commands++
		stats.addSavings(content, compressed)
		out := cloneMap(message)
		out["content"] = compressed
		return out
	case []any:
		newParts := make([]any, len(content))
		copy(newParts, content)
		changed := false
		for index, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			partType := stringValue(part["type"])
			if partType != "text" && partType != "input_text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			compressed, ok := c.compressText(filter, text)
			if !ok {
				continue
			}
			stats.addSavings(text, compressed)
			newPart := cloneMap(part)
			newPart["text"] = compressed
			newParts[index] = newPart
			changed = true
		}
		if !changed {
			return nil
		}
		stats.Commands++
		out := cloneMap(message)
		out["content"] = newParts
		return out
	}
	return nil
}

func (c *rtkCompressor) compressOpenAIResponsesRequest(req map[string]any) (map[string]any, rtkCompressionStats) {
	stats := rtkCompressionStats{}
	input, ok := req["input"].([]any)
	if !ok || len(input) == 0 {
		return req, stats
	}
	commands := responsesToolCommands(input)
	if len(commands) == 0 {
		return req, stats
	}

	replaced := make([]any, len(input))
	copy(replaced, input)
	changed := false
	for index, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || stringValue(item["type"]) != "function_call_output" {
			continue
		}
		command, ok := commands[strings.TrimSpace(stringValue(item["call_id"]))]
		if !ok {
			continue
		}
		filter, ok := rtkFilterForCommand(command)
		if !ok {
			continue
		}
		if newItem := c.compressResponsesToolOutput(item, filter, &stats); newItem != nil {
			replaced[index] = newItem
			changed = true
		}
	}
	if !changed {
		return req, stats
	}
	out := cloneMap(req)
	out["input"] = replaced
	return out, stats
}

func responsesToolCommands(input []any) map[string]string {
	out := make(map[string]string)
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || stringValue(item["type"]) != "function_call" {
			continue
		}
		if !isShellToolName(stringValue(item["name"])) {
			continue
		}
		callID := strings.TrimSpace(stringValue(item["call_id"]))
		if callID == "" {
			callID = strings.TrimSpace(stringValue(item["id"]))
		}
		command := commandFromToolArguments(stringValue(item["arguments"]))
		if callID != "" && command != "" {
			out[callID] = command
		}
	}
	return out
}

func (c *rtkCompressor) compressResponsesToolOutput(item map[string]any, filter string, stats *rtkCompressionStats) map[string]any {
	switch output := item["output"].(type) {
	case string:
		compressed, ok := c.compressText(filter, output)
		if !ok {
			return nil
		}
		stats.Commands++
		stats.addSavings(output, compressed)
		out := cloneMap(item)
		out["output"] = compressed
		return out
	case []any:
		newParts := make([]any, len(output))
		copy(newParts, output)
		changed := false
		for index, raw := range output {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			partType := stringValue(part["type"])
			if partType != "input_text" && partType != "output_text" && partType != "text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			compressed, ok := c.compressText(filter, text)
			if !ok {
				continue
			}
			stats.addSavings(text, compressed)
			newPart := cloneMap(part)
			newPart["text"] = compressed
			newParts[index] = newPart
			changed = true
		}
		if !changed {
			return nil
		}
		stats.Commands++
		out := cloneMap(item)
		out["output"] = newParts
		return out
	}
	return nil
}

func commandFromToolArguments(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
		return ""
	}
	for _, key := range []string{"command", "cmd", "script"} {
		if command := strings.TrimSpace(stringValue(arguments[key])); command != "" {
			return command
		}
	}
	return ""
}

func isShellToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash", "shell", "sh", "exec_command", "run_command", "execute_command":
		return true
	default:
		return false
	}
}

func (c *rtkCompressor) compressText(filter, text string) (string, bool) {
	if len(text) < rtkMinCompressBytes || len(text) > rtkMaxCompressBytes || !utf8.ValidString(text) {
		return "", false
	}
	c.ensure()
	c.mu.Lock()
	rtkPath := c.path
	c.mu.Unlock()
	if rtkPath == "" {
		return "", false
	}

	key := sha256.Sum256([]byte(filter + "\x00" + text))
	c.mu.Lock()
	if cached, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return cached, true
	}
	c.mu.Unlock()

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	default:
		// Under load, skipping compression is safer than queueing behind a
		// process spawn and inflating request latency.
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), rtkCommandTimeout)
	defer cancel()
	out, err := c.runner(ctx, rtkPath, filter, []byte(text))
	if err != nil || len(out) == 0 || len(out) >= len(text) || !utf8.Valid(out) {
		return "", false
	}
	compressed := string(out)
	c.mu.Lock()
	c.cachePutLocked(key, compressed)
	c.mu.Unlock()
	return compressed, true
}

func (c *rtkCompressor) cachePutLocked(key [32]byte, value string) {
	if len(value) > rtkCacheMaxBytes {
		return
	}
	if _, exists := c.cache[key]; exists {
		return
	}
	for len(c.cacheKeys) > 0 && (len(c.cacheKeys) >= rtkCacheMaxEntries || c.cacheSize+len(value) > rtkCacheMaxBytes) {
		oldest := c.cacheKeys[0]
		c.cacheKeys = c.cacheKeys[1:]
		if oldValue, ok := c.cache[oldest]; ok {
			c.cacheSize -= len(oldValue)
			delete(c.cache, oldest)
		}
	}
	c.cache[key] = value
	c.cacheKeys = append(c.cacheKeys, key)
	c.cacheSize += len(value)
}

func runRTKCLI(ctx context.Context, rtkPath, filter string, input []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, rtkPath, "pipe", "-f", filter)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("rtk pipe %s: %w: %s", filter, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// rtkFilterForCommand maps a shell command to one of rtk pipe's filters. Only
// stdout-preserving shapes are accepted: agents commonly prefix commands with
// `cd <dir> &&`, suppress stderr with 2>/dev/null, or truncate with
// `| head`/`| tail`, none of which change the report shape. Anything else is
// left untouched because the filter could pick the wrong shape.
func rtkFilterForCommand(raw string) (string, bool) {
	segments, operators, ok := splitShellStages(strings.TrimSpace(raw))
	if !ok || len(segments) == 0 || len(operators) != len(segments)-1 {
		return "", false
	}
	for len(operators) > 0 && (operators[0] == "&&" || operators[0] == ";") && isCDStage(segments[0]) {
		segments = segments[1:]
		operators = operators[1:]
	}
	if len(segments) == 0 || len(operators) != len(segments)-1 {
		return "", false
	}
	for index := 1; index < len(segments); index++ {
		if operators[index-1] != "|" || !isTruncationStage(segments[index]) {
			return "", false
		}
	}
	return rtkFilterForStage(segments[0])
}

// splitShellStages breaks a command at unquoted operators. Quoted text is
// opaque, so a regex alternation like "a|b" is not mistaken for a pipe.
// 2>/dev/null is dropped because it only affects stderr.
func splitShellStages(raw string) ([]string, []string, bool) {
	var segments, operators []string
	var buf strings.Builder
	quote := byte(0)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(raw) {
				buf.WriteByte(c)
				i++
				buf.WriteByte(raw[i])
				continue
			}
			buf.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			buf.WriteByte(c)
		case '\\':
			if i+1 >= len(raw) {
				return nil, nil, false
			}
			buf.WriteByte(c)
			i++
			buf.WriteByte(raw[i])
		case ' ', '\t':
			buf.WriteByte(c)
		case '\n', '\r', '$', '`', '{', '}', '(', ')':
			return nil, nil, false
		case '&', '|', ';':
			op := string(c)
			if i+1 < len(raw) {
				switch {
				case c == '&' && raw[i+1] == '&':
					op = "&&"
					i++
				case c == '|' && raw[i+1] == '|':
					return nil, nil, false
				case c == '&' && raw[i+1] == '>':
					return nil, nil, false
				case c == '|' && raw[i+1] == '&':
					return nil, nil, false
				case c == ';' && raw[i+1] == ';':
					return nil, nil, false
				}
			}
			if strings.TrimSpace(buf.String()) == "" {
				return nil, nil, false
			}
			segments = append(segments, strings.TrimSpace(buf.String()))
			buf.Reset()
			operators = append(operators, op)
		case '<':
			return nil, nil, false
		case '>':
			next := i + 1
			if next < len(raw) && raw[next] == '>' {
				next++
			}
			for next < len(raw) && (raw[next] == ' ' || raw[next] == '\t') {
				next++
			}
			const devNull = "/dev/null"
			if !strings.HasPrefix(raw[next:], devNull) || !isShellWordEnd(raw, next+len(devNull)) || !trimStderrFD(&buf) {
				return nil, nil, false
			}
			i = next + len(devNull) - 1
		default:
			buf.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, nil, false
	}
	if strings.TrimSpace(buf.String()) != "" {
		segments = append(segments, strings.TrimSpace(buf.String()))
	}
	return segments, operators, true
}

func isShellWordEnd(raw string, index int) bool {
	if index >= len(raw) {
		return true
	}
	switch raw[index] {
	case ' ', '\t', '|', '&', ';', '>', '<':
		return true
	}
	return false
}

// trimStderrFD removes a trailing fd 2 token before a suppressed redirect. It
// refuses tokens such as foo2 or "x2" where the 2 is not a standalone fd.
func trimStderrFD(buf *strings.Builder) bool {
	text := strings.TrimRight(buf.String(), " \t")
	if !strings.HasSuffix(text, "2") {
		return false
	}
	prefix := text[:len(text)-1]
	if prefix != "" && !strings.ContainsRune(" \t", rune(prefix[len(prefix)-1])) {
		return false
	}
	buf.Reset()
	buf.WriteString(strings.TrimRight(prefix, " \t"))
	return true
}

func isCDStage(segment string) bool {
	fields := stripCommandPrefixes(strings.Fields(segment))
	return len(fields) > 0 && fields[0] == "cd"
}

func isTruncationStage(segment string) bool {
	fields := stripCommandPrefixes(strings.Fields(segment))
	if len(fields) == 0 {
		return false
	}
	head := strings.ToLower(path.Base(strings.ReplaceAll(fields[0], `\`, "/")))
	return head == "head" || head == "tail"
}

func rtkFilterForStage(command string) (string, bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", false
	}
	fields := strings.Fields(command)
	fields = stripCommandPrefixes(fields)
	if len(fields) == 0 {
		return "", false
	}
	head := strings.ToLower(path.Base(strings.ReplaceAll(fields[0], `\`, "/")))
	args := fields[1:]

	switch head {
	case "git":
		if len(args) == 0 {
			return "", false
		}
		switch args[0] {
		case "status":
			return "git-status", true
		case "diff":
			return "git-diff", true
		case "log":
			return "git-log", true
		}
	case "rg":
		return "rg", true
	case "grep":
		return "grep", true
	case "find":
		return "find", true
	case "fd":
		return "fd", true
	case "go":
		if len(args) > 0 {
			switch args[0] {
			case "test":
				return "go-test", true
			case "build":
				return "go-build", true
			}
		}
	case "cargo":
		if len(args) > 0 && args[0] == "test" {
			return "cargo-test", true
		}
	case "pytest", "py.test":
		return "pytest", true
	case "ctest":
		return "ctest", true
	case "tsc":
		return "tsc", true
	case "vitest":
		return "vitest", true
	case "mypy":
		return "mypy", true
	case "ruff":
		if len(args) > 0 {
			switch args[0] {
			case "check":
				return "ruff-check", true
			case "format":
				return "ruff-format", true
			}
		}
	case "prettier":
		return "prettier", true
	case "phpunit":
		return "phpunit", true
	case "pest":
		return "pest", true
	case "paratest":
		return "paratest", true
	case "phpstan":
		return "phpstan", true
	case "pint":
		return "pint", true
	case "sqlfluff":
		if len(args) > 0 && args[0] == "lint" {
			return "sqlfluff-lint", true
		}
	case "ecs":
		return "ecs", true
	case "python", "python3", "py":
		for index := 0; index+1 < len(args); index++ {
			if args[index] == "-m" && args[index+1] == "pytest" {
				return "pytest", true
			}
		}
	case "npx", "pnpm", "yarn", "bun":
		rest := args
		for len(rest) > 0 && (rest[0] == "exec" || rest[0] == "x" || rest[0] == "dlx") {
			rest = rest[1:]
		}
		if len(rest) > 0 {
			switch strings.ToLower(path.Base(rest[0])) {
			case "vitest":
				return "vitest", true
			case "tsc":
				return "tsc", true
			case "pytest":
				return "pytest", true
			}
		}
	}
	return "", false
}

func stripCommandPrefixes(fields []string) []string {
	for len(fields) > 0 {
		switch fields[0] {
		case "sudo", "command", "time", "noglob", "exec":
			fields = fields[1:]
		case "env":
			fields = fields[1:]
			for len(fields) > 0 && isEnvAssignment(fields[0]) {
				fields = fields[1:]
			}
		default:
			if isEnvAssignment(fields[0]) {
				fields = fields[1:]
				continue
			}
			return fields
		}
	}
	return fields
}

func isEnvAssignment(value string) bool {
	index := strings.IndexByte(value, '=')
	if index <= 0 {
		return false
	}
	for _, r := range value[:index] {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func estimateRTKTokens(text string) int64 {
	if text == "" {
		return 0
	}
	ascii, nonASCII := 0, 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
	}
	tokens := int64(ascii/4 + nonASCII)
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}
