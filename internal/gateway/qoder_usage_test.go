package gateway

import (
	"encoding/json"
	"testing"
	"time"
)

func TestQoderParseQuotaReadsTheCreditPools(t *testing.T) {
	envelope := qoderUsageEnvelope{DisplayMode: "qoder", QoderUsage: json.RawMessage(`{
		"userType":"personal_professional_trial",
		"expiresAt":"2026-11-02T16:00:00Z",
		"userQuota":{"name":"积分","unit":"credits","total":300,"used":300},
		"addOnQuota":{"name":"Add-on credits","unit":"credits","cap":400,"remaining":100}
	}`)}
	quota := qoderParseQuota(envelope)
	if quota.Error != "" {
		t.Fatalf("unexpected error: %s", quota.Error)
	}
	if quota.Plan != "personal_professional_trial" {
		t.Fatalf("plan = %q", quota.Plan)
	}
	if len(quota.Windows) != 2 {
		t.Fatalf("windows = %+v", quota.Windows)
	}
	// The panel shows a percentage plus the raw numbers, so both have to come
	// out: a pool read as "used up" with no figures behind it reads as a bug.
	if quota.Windows[0].Used != 100 || quota.Windows[0].Amount != 300 || quota.Windows[0].Limit != 300 {
		t.Fatalf("first pool wrong: %+v", quota.Windows[0])
	}
	if quota.Windows[0].Name != "积分" || quota.Windows[0].Unit != "credits" {
		t.Fatalf("pool naming wrong: %+v", quota.Windows[0])
	}
	// A pool that only says what is left is read from the cap.
	if quota.Windows[1].Used != 75 || quota.Windows[1].Amount != 300 || quota.Windows[1].Limit != 400 {
		t.Fatalf("add-on pool wrong: %+v", quota.Windows[1])
	}
	if quota.Until == "" {
		t.Fatal("the plan's own expiry should be reported")
	}
}

// Qoder names these fields in both cases, and an enterprise account answers
// with a display mode and no personal quota at all.
func TestQoderParseQuotaShapes(t *testing.T) {
	cases := []struct {
		name      string
		envelope  qoderUsageEnvelope
		wantPlan  string
		wantPools int
		wantError string
	}{
		{"snake case", qoderUsageEnvelope{DisplayMode: "qoder", QoderUsage: json.RawMessage(`{"user_type":"team","user_quota":{"unit":"credits","total":100,"used":20}}`)}, "team", 1, ""},
		{"enterprise", qoderUsageEnvelope{DisplayMode: "enterprise"}, "Enterprise", 0, ""},
		{"no quota data", qoderUsageEnvelope{DisplayMode: "qoder"}, "", 0, "没有返回额度数据"},
		{"an empty pool is not shown", qoderUsageEnvelope{DisplayMode: "qoder", QoderUsage: json.RawMessage(`{"userQuota":{"total":0,"used":0}}`)}, "", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quota := qoderParseQuota(tc.envelope)
			if quota.Plan != tc.wantPlan {
				t.Fatalf("plan = %q, want %q", quota.Plan, tc.wantPlan)
			}
			if len(quota.Windows) != tc.wantPools {
				t.Fatalf("windows = %+v", quota.Windows)
			}
			if tc.wantError != "" && quota.Error == "" {
				t.Fatalf("expected the error %q to be reported", tc.wantError)
			}
		})
	}
}

func TestQoderQuotaPreservesPoolResetTimes(t *testing.T) {
	for _, field := range []string{"resetsAt", "reset_at"} {
		t.Run(field, func(t *testing.T) {
			raw := `{"userQuota":{"total":100,"used":20,"` + field + `":"2026-11-02T16:00:00Z"}}`
			quota := qoderParseQuota(qoderUsageEnvelope{DisplayMode: "qoder", QoderUsage: json.RawMessage(raw)})
			if len(quota.Windows) != 1 || quota.Windows[0].ResetsAt != "2026-11-02T16:00:00Z" {
				t.Fatalf("reset time lost: %+v", quota.Windows)
			}
		})
	}
}

func TestQoderWhenAcceptsEveryShapeItHasUsed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"rfc3339 string", `"2026-11-02T16:00:00Z"`, true},
		{"seconds", `1793664000`, true},
		{"milliseconds", `1793664000000`, true},
		{"seconds as text", `"1793664000"`, true},
		{"absent", `null`, false},
		{"zero", `0`, false},
		{"prose", `"soon"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := qoderWhen(json.RawMessage(tc.raw))
			if (got != "") != tc.want {
				t.Fatalf("qoderWhen(%s) = %q, want a value: %v", tc.raw, got, tc.want)
			}
			if got != "" {
				if _, err := time.Parse(time.RFC3339, got); err != nil {
					t.Fatalf("qoderWhen(%s) = %q, which is not RFC3339", tc.raw, got)
				}
			}
		})
	}
}
