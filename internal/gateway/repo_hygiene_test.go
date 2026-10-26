package gateway

import (
	"os"
	"strings"
	"testing"
)

// audit_*_test.go 是审计时用来「复现缺陷」的一次性探针：它们断言的是缺陷本身（或者
// 一旦缺陷修好就会变红）。审计结论要么落成正式回归测试（去掉 audit_ 前缀），要么
// 写进 docs/known-issues.md，探针本身不进仓库。这里让残留的探针立刻显眼。
func TestNoAuditProbesLeftBehind(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "audit_") && strings.HasSuffix(name, "_test.go") {
			left = append(left, name)
		}
	}
	if len(left) > 0 {
		t.Fatalf("audit probes left in the package (turn each into a regression test or a known-issues entry): %s", strings.Join(left, ", "))
	}
}
