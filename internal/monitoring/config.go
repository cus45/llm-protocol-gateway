package monitoring

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

type RuleConfig struct {
	Key             string             `json:"key"`
	Enabled         bool               `json:"enabled"`
	Severity        Severity           `json:"severity"`
	Params          map[string]float64 `json:"params"`
	CooldownMinutes int                `json:"cooldownMinutes"`
}

type RuleDef struct {
	Key           string             `json:"key"`
	Name          string             `json:"name"`
	TargetType    string             `json:"targetType"`
	DefaultParams map[string]float64 `json:"defaultParams"`
	Interval      time.Duration      `json:"-"`           // 阶段 4 的 rules API 用 intervalMinutes 输出，不直接序列化纳秒
	Consecutive   bool               `json:"consecutive"` // true: Params["consecutiveFailures"] / consecutiveDays instead of a duration
}

var ruleDefs = []RuleDef{
	{Key: "oauth_expiring", Name: "凭证即将过期", TargetType: TargetProvider, DefaultParams: map[string]float64{"leadMinutes": 120}, Interval: time.Minute},
	{Key: "oauth_refresh_failed", Name: "凭证连续续期失败", TargetType: TargetProvider, DefaultParams: map[string]float64{"consecutiveFailures": 2}, Interval: time.Minute, Consecutive: true},
	{Key: "auth_invalid", Name: "凭证失效", TargetType: TargetProvider, DefaultParams: map[string]float64{"windowMinutes": 5, "minCount": 3, "minRatio": 0.5, "minSamples": 10}, Interval: time.Minute},
	{Key: "quota_low", Name: "额度不足", TargetType: TargetProvider, DefaultParams: map[string]float64{"maxRatio": 0.1}, Interval: 15 * time.Minute},
	{Key: "credit_expiring", Name: "积分即将到期", TargetType: TargetProvider, DefaultParams: map[string]float64{"leadHours": 72}, Interval: 24 * time.Hour},
	{Key: "key_budget_warn", Name: "密钥额度预警", TargetType: TargetAPIKey, DefaultParams: map[string]float64{"warnRatio": 0.8}, Interval: 5 * time.Minute},
	{Key: "key_budget_exhausted", Name: "密钥额度耗尽", TargetType: TargetAPIKey, DefaultParams: map[string]float64{"criticalRatio": 1.0}, Interval: 5 * time.Minute},
	{Key: "provider_down", Name: "Provider 成功率过低", TargetType: TargetProvider, DefaultParams: map[string]float64{"windowMinutes": 5, "minSamples": 20, "maxSuccessRatio": 0.8}, Interval: time.Minute},
	{Key: "server_error_spike", Name: "上游 5xx 激增", TargetType: TargetProvider, DefaultParams: map[string]float64{"windowMinutes": 5, "minSamples": 20, "maxErrorRatio": 0.1}, Interval: time.Minute},
	{Key: "rate_limit_spike", Name: "上游限流激增", TargetType: TargetProvider, DefaultParams: map[string]float64{"windowMinutes": 5, "minSamples": 20, "maxRatio": 0.2, "maxCount": 10}, Interval: time.Minute},
	{Key: "empty_response", Name: "空响应", TargetType: TargetProvider, DefaultParams: map[string]float64{"windowMinutes": 10, "minSamples": 10, "maxRatio": 0.05}, Interval: time.Minute},
	{Key: "ttft_regression", Name: "首包延迟回退", TargetType: TargetProvider, DefaultParams: map[string]float64{"windowMinutes": 15, "minSamples": 20, "floorMs": 5000, "factor": 2}, Interval: time.Minute},
	{Key: "model_unavailable", Name: "模型列表拉取失败", TargetType: TargetProvider, DefaultParams: map[string]float64{"consecutiveFailures": 3}, Interval: 15 * time.Minute, Consecutive: true},
	{Key: "probe_failed", Name: "轻探测失败", TargetType: TargetProvider, DefaultParams: map[string]float64{"consecutiveFailures": 2, "authSeverity": float64(SeverityP0.Rank())}, Interval: time.Minute, Consecutive: true},
	{Key: "checkin_failed", Name: "签到连续失败", TargetType: TargetProvider, DefaultParams: map[string]float64{"consecutiveDays": 2}, Interval: 24 * time.Hour, Consecutive: true},
	{Key: "multi_ip", Name: "API Key 多 IP 使用", TargetType: TargetAPIKey, DefaultParams: map[string]float64{"windowMinutes": 10, "minIPs": 5}, Interval: time.Minute},
	{Key: "concurrent_ip", Name: "API Key 并发 IP", TargetType: TargetAPIKey, DefaultParams: map[string]float64{"minIPs": 4}, Interval: time.Minute},
	{Key: "collector_stale", Name: "监控采集器停滞", TargetType: TargetGateway, DefaultParams: map[string]float64{"maxAgeMinutes": 5}, Interval: time.Minute},
}

func RuleDefinition(key string) (RuleDef, bool) {
	for _, def := range ruleDefs {
		if def.Key == key {
			return def, true
		}
	}
	return RuleDef{}, false
}

func DefaultRules() []RuleConfig {
	out := make([]RuleConfig, 0, len(ruleDefs))
	for _, def := range ruleDefs {
		out = append(out, RuleConfig{
			Key:             def.Key,
			Enabled:         true,
			Severity:        defaultSeverity(def.Key),
			Params:          cloneParams(def.DefaultParams),
			CooldownMinutes: 60,
		})
	}
	return out
}

func defaultSeverity(key string) Severity {
	switch key {
	case "oauth_expiring", "oauth_refresh_failed", "auth_invalid", "multi_ip", "concurrent_ip":
		return SeverityP0
	case "key_budget_warn", "credit_expiring", "ttft_regression", "model_unavailable", "collector_stale", "checkin_failed":
		return SeverityP2
	default:
		return SeverityP1
	}
}

func cloneParams(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func ValidateRules(rules []RuleConfig) error {
	seen := map[string]bool{}
	for _, rule := range rules {
		def, ok := RuleDefinition(rule.Key)
		if !ok {
			return fmt.Errorf("unknown rule %q", rule.Key)
		}
		if seen[rule.Key] {
			return fmt.Errorf("duplicate rule %q", rule.Key)
		}
		seen[rule.Key] = true
		if rule.Severity != SeverityP0 && rule.Severity != SeverityP1 && rule.Severity != SeverityP2 {
			return fmt.Errorf("rule %s: severity must be p0/p1/p2", rule.Key)
		}
		if rule.CooldownMinutes < 5 || rule.CooldownMinutes > 1440 {
			return fmt.Errorf("rule %s: cooldownMinutes must be 5..1440", rule.Key)
		}
		for key, value := range rule.Params {
			if _, known := def.DefaultParams[key]; !known {
				return fmt.Errorf("rule %s: unknown param %s", rule.Key, key)
			}
			if key == "windowMinutes" && value != 5 && value != 10 && value != 15 {
				return fmt.Errorf("rule %s: windowMinutes must be 5/10/15", rule.Key)
			}
			if value <= 0 {
				return fmt.Errorf("rule %s: param %s must be > 0", rule.Key, key)
			}
			if (key == "minSamples") && (value < 1 || value > 10000) {
				return fmt.Errorf("rule %s: minSamples must be 1..10000", rule.Key)
			}
			if (key == "minRatio" || key == "maxRatio" || key == "maxSuccessRatio" ||
				key == "maxErrorRatio" || key == "warnRatio" || key == "criticalRatio") && value > 1 {
				return fmt.Errorf("rule %s: ratio %s must be <= 1", rule.Key, key)
			}
		}
	}
	return nil
}

func NormalizeRules(stored []RuleConfig) []RuleConfig {
	byKey := make(map[string]RuleConfig, len(stored))
	for _, r := range stored {
		byKey[r.Key] = r
	}
	out := DefaultRules()
	for i := range out {
		stored, ok := byKey[out[i].Key]
		if !ok {
			continue
		}
		out[i].Enabled = stored.Enabled
		if stored.Severity == SeverityP0 || stored.Severity == SeverityP1 || stored.Severity == SeverityP2 {
			out[i].Severity = stored.Severity
		}
		if stored.CooldownMinutes >= 5 && stored.CooldownMinutes <= 1440 {
			out[i].CooldownMinutes = stored.CooldownMinutes
		}
		for key, value := range stored.Params {
			if _, known := out[i].Params[key]; known && value > 0 {
				out[i].Params[key] = value
			}
		}
	}
	return out
}

func MarshalRules(rules []RuleConfig) (string, error) {
	b, err := json.Marshal(rules)
	return string(b), err
}

func UnmarshalRules(raw string) []RuleConfig {
	rules, err := ParseRules(raw)
	if err != nil {
		return DefaultRules()
	}
	return rules
}

// ParseRules reports malformed stored JSON; hot reload keeps the previous
// rules instead of silently falling back to defaults.
func ParseRules(raw string) ([]RuleConfig, error) {
	if raw == "" {
		return DefaultRules(), nil
	}
	var stored []RuleConfig
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, err
	}
	return NormalizeRules(stored), nil
}

func SortedRuleKeys(rules []RuleConfig) []string {
	keys := make([]string, 0, len(rules))
	for _, r := range rules {
		keys = append(keys, r.Key)
	}
	sort.Strings(keys)
	return keys
}

type DispatchConfig struct {
	DigestMinutes  int    `json:"digestMinutes"`
	QuietEnabled   bool   `json:"quietEnabled"`
	QuietStart     string `json:"quietStart"`
	QuietEnd       string `json:"quietEnd"`
	RecoveryNotify bool   `json:"recoveryNotify"`
	Timezone       string `json:"timezone"`
	BaseURL        string `json:"baseUrl"`
}

func DefaultDispatchConfig() DispatchConfig {
	return DispatchConfig{
		DigestMinutes: 15, QuietEnabled: true, QuietStart: "23:00", QuietEnd: "08:00",
		RecoveryNotify: true, Timezone: "Asia/Shanghai",
	}
}

func (c DispatchConfig) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		loc, _ = time.LoadLocation("Asia/Shanghai")
	}
	return loc
}

type ProbeConfig struct {
	EventDriven struct {
		Enabled             bool `json:"enabled"`
		IdleMinutes         int  `json:"idleMinutes"`
		MaxPerDayPerAccount int  `json:"maxPerDayPerAccount"`
		MinIntervalMinutes  int  `json:"minIntervalMinutes"`
	} `json:"eventDriven"`
	Daily struct {
		Enabled     bool   `json:"enabled"`
		WindowStart string `json:"windowStart"`
		WindowEnd   string `json:"windowEnd"`
	} `json:"daily"`
	QuotaStaleMinutes int `json:"quotaStaleMinutes"`
}

func DefaultProbeConfig() ProbeConfig {
	cfg := ProbeConfig{QuotaStaleMinutes: 30}
	cfg.EventDriven.Enabled = true
	cfg.EventDriven.IdleMinutes = 30
	cfg.EventDriven.MaxPerDayPerAccount = 4
	cfg.EventDriven.MinIntervalMinutes = 360
	cfg.Daily.Enabled = true
	cfg.Daily.WindowStart = "04:00"
	cfg.Daily.WindowEnd = "06:00"
	return cfg
}

type MigrationMarker struct {
	Checkins         bool  `json:"checkins"`
	Alerts           bool  `json:"alerts"`
	Telegram         bool  `json:"telegram"`
	LegacyMaxAlertID int64 `json:"legacyMaxAlertId"`
}

// Settings is the slice of the store the monitoring config needs. Both the
// store and test fakes implement it.
type Settings interface {
	Setting(key string) string
	SetSetting(key, value string) error
}

type Config struct {
	settings Settings
}

func NewConfig(settings Settings) *Config { return &Config{settings: settings} }

func (c *Config) Rules() []RuleConfig { return UnmarshalRules(c.RawRules()) }

// RawRules returns the stored JSON verbatim. Reads normalize away invalid values,
// so tests that assert "a rejected PUT changed nothing" must compare this.
func (c *Config) RawRules() string { return c.settings.Setting("monitor_rules") }

func (c *Config) SaveRules(rules []RuleConfig) error {
	if err := ValidateRules(rules); err != nil {
		return err
	}
	raw, err := MarshalRules(rules)
	if err != nil {
		return err
	}
	return c.settings.SetSetting("monitor_rules", string(raw))
}

func (c *Config) Dispatch() DispatchConfig {
	cfg := DefaultDispatchConfig()
	if raw := c.settings.Setting("monitor_dispatch"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return DefaultDispatchConfig()
		}
	}
	if cfg.DigestMinutes <= 0 {
		cfg.DigestMinutes = 15
	}
	if cfg.Timezone == "" {
		cfg.Timezone = "Asia/Shanghai"
	}
	return cfg
}

func (c *Config) SaveDispatch(cfg DispatchConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return c.settings.SetSetting("monitor_dispatch", string(raw))
}

func (c *Config) Channels() []Channel {
	raw := c.settings.Setting("monitor_channels")
	if raw == "" {
		return nil
	}
	var channels []Channel
	if err := json.Unmarshal([]byte(raw), &channels); err != nil {
		return nil
	}
	return channels
}

func (c *Config) SaveChannels(channels []Channel) error {
	raw, err := json.Marshal(channels)
	if err != nil {
		return err
	}
	return c.settings.SetSetting("monitor_channels", string(raw))
}

// Probe reads the stored probe schedule.
//
// 已撤下的字段（旧版每周 CLI 深检的 "weeklyCli"）在库里可能还留着键：json 解析
// 会忽略未知字段，所以旧库照常读得出；下一次 SaveProbe 写回时该键自然消失，这是
// 期望行为（该深检已整个移除，不保留兼容写入）。
func (c *Config) Probe() ProbeConfig {
	cfg := DefaultProbeConfig()
	if raw := c.settings.Setting("monitor_probe"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return DefaultProbeConfig()
		}
	}
	return cfg
}

func (c *Config) SaveProbe(cfg ProbeConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return c.settings.SetSetting("monitor_probe", string(raw))
}

func (c *Config) MigrationMarker() MigrationMarker {
	var marker MigrationMarker
	if raw := c.settings.Setting("monitor_migration_v1"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &marker)
	}
	return marker
}

func (c *Config) SaveMigrationMarker(marker MigrationMarker) error {
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return c.settings.SetSetting("monitor_migration_v1", string(raw))
}
