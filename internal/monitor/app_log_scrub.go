package monitor

import "regexp"

// App-log entries carry upstream error text, OAuth refresh failures and tool
// output — all of which have been known to echo a credential ("Incorrect API
// key provided: sk-…", "refresh_token=…"). Scrubbing happens here, at the one
// place every entry passes through, rather than at each of the callers: a new
// log line cannot forget it.
var appLogSecretPattern = regexp.MustCompile(
	`\b(sk-[A-Za-z0-9_\-]{12,}` +
		`|sk-ant-[A-Za-z0-9_\-]{12,}` +
		`|sk-proj-[A-Za-z0-9_\-]{12,}` +
		`|ghp_[A-Za-z0-9]{20,}` +
		`|gho_[A-Za-z0-9]{20,}` +
		`|github_pat_[A-Za-z0-9_]{20,}` +
		`|AKIA[0-9A-Z]{16}` +
		`|AIza[0-9A-Za-z_\-]{30,}` +
		`|xox[baprs]-[0-9A-Za-z\-]{10,}` +
		`|glpat-[0-9A-Za-z_\-]{16,}` +
		// 厂商私有的不透明凭据（Google OAuth refresh token、xAI API token）。
		`|1//0[0-9A-Za-z_\-]{20,}` +
		`|pac_[0-9A-Za-z_\-]{20,})`)

// appLogJWTPattern matches a JSON Web Token wherever it appears in free text.
// Access tokens for the subscription providers are JWTs, and an expiry or a
// refresh failure can name one.
var appLogJWTPattern = regexp.MustCompile(
	`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)

const appLogScrubbed = "[redacted]"

// scrubAppLogText replaces known credential shapes in one free-text field. It
// keeps the field's shape, so an error message is still readable.
func scrubAppLogText(text string) string {
	if text == "" {
		return text
	}
	text = appLogSecretPattern.ReplaceAllString(text, appLogScrubbed)
	return appLogJWTPattern.ReplaceAllString(text, appLogScrubbed)
}
