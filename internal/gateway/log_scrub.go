package gateway

import (
	"encoding/json"
	"regexp"
	"strings"
)

// 请求正文落库前的敏感字段脱敏。
//
// 请求日志（失败请求默认记录正文、成功请求可开关记录）会把正文写进 SQLite，
// 随后经 /__app/requests 展示给前端。客户端偶尔会把凭据塞进正文：Claude Code
// 的 MCP server 配置带 env/API key、Responses 的 metadata、自定义字段等。
// 这些字段一旦落库就会经由管理界面泄露，因此在落库前按字段名抹掉，并按常见
// 密钥前缀扫描字符串值兜底。

// sensitiveRequestKeys are payload keys whose values are replaced wholesale.
var sensitiveRequestKeys = map[string]struct{}{
	"api_key":             {},
	"apikey":              {},
	"api-key":             {},
	"x-api-key":           {},
	"authorization":       {},
	"proxy-authorization": {},
	"access_token":        {},
	"refresh_token":       {},
	"id_token":            {},
	"auth_token":          {},
	"bearer_token":        {},
	"secret":              {},
	"client_secret":       {},
	"password":            {},
	"passwd":              {},
	"private_key":         {},
	"credential":          {},
	"credentials":         {},
	"cookie":              {},
	"set-cookie":          {},
}

// sensitiveKeyMatches reports whether key (any case, -/_ alike) names a secret.
func sensitiveKeyMatches(key string) bool {
	// 先按原大小写拆驼峰（refreshToken → refresh_token），再统一小写与分隔符。
	normalized := camelToSnake(strings.TrimSpace(key))
	normalized = strings.NewReplacer("-", "_", ".", "_").Replace(normalized)
	if _, ok := sensitiveRequestKeys[normalized]; ok {
		return true
	}
	return strings.HasSuffix(normalized, "_api_key") || strings.HasSuffix(normalized, "_token") ||
		strings.HasSuffix(normalized, "_secret") || strings.HasSuffix(normalized, "_password")
}

// camelToSnake lowercases s and inserts _ before an upper-case letter that
// follows a lower-case one or a digit.
func camelToSnake(s string) string {
	var builder strings.Builder
	builder.Grow(len(s) + 4)
	var previous rune
	for i, current := range s {
		if i > 0 && current >= 'A' && current <= 'Z' && (previous >= 'a' && previous <= 'z' || previous >= '0' && previous <= '9') {
			builder.WriteByte('_')
		}
		builder.WriteRune(current)
		previous = current
	}
	return strings.ToLower(builder.String())
}

// secretValuePattern matches well-known credential prefixes inside free text,
// so a key pasted into a prompt ("use sk-abc… to call it") is scrubbed too.
var secretValuePattern = regexp.MustCompile(
	`\b(sk-[A-Za-z0-9_\-]{12,}` +
		`|sk-ant-[A-Za-z0-9_\-]{12,}` +
		`|sk-proj-[A-Za-z0-9_\-]{12,}` +
		`|ghp_[A-Za-z0-9]{20,}` +
		`|gho_[A-Za-z0-9]{20,}` +
		`|github_pat_[A-Za-z0-9_]{20,}` +
		`|AKIA[0-9A-Z]{16}` +
		`|AIza[0-9A-Za-z_\-]{30,}` +
		`|xox[baprs]-[0-9A-Za-z\-]{10,}` +
		`|glpat-[0-9A-Za-z_\-]{16,})`)

const scrubbedValue = "[redacted]"

// scrubSensitiveRequestBody returns body with credential fields and known
// secret patterns replaced. Bodies that aren't JSON are scrubbed as text and
// returned as-is when nothing matched, so the log keeps its exact bytes.
func scrubSensitiveRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		scrubbed := secretValuePattern.ReplaceAllString(string(body), scrubbedValue)
		if scrubbed == string(body) {
			return body
		}
		return []byte(scrubbed)
	}
	changed := false
	scrubbed := scrubSensitiveValue(parsed, false, &changed)
	if !changed {
		return body
	}
	rewritten, err := json.Marshal(scrubbed)
	if err != nil {
		return body
	}
	return rewritten
}

// scrubSensitiveValue walks parsed JSON. Values under a sensitive key are
// replaced; everything else has its strings pattern-scrubbed. insideSensitive
// carries the parent decision down the subtree.
func scrubSensitiveValue(value any, insideSensitive bool, changed *bool) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			sensitive := insideSensitive || sensitiveKeyMatches(key)
			if sensitive {
				typed[key] = scrubbedValue
				*changed = true
				continue
			}
			typed[key] = scrubSensitiveValue(child, false, changed)
		}
		return typed
	case []any:
		for i, child := range typed {
			if insideSensitive {
				typed[i] = scrubbedValue
				*changed = true
				continue
			}
			typed[i] = scrubSensitiveValue(child, false, changed)
		}
		return typed
	case string:
		if insideSensitive {
			*changed = true
			return scrubbedValue
		}
		if secretValuePattern.MatchString(typed) {
			*changed = true
			return secretValuePattern.ReplaceAllString(typed, scrubbedValue)
		}
		return typed
	default:
		if insideSensitive {
			*changed = true
			return scrubbedValue
		}
		return value
	}
}
