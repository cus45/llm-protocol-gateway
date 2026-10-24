package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// workBuddyCheckinAnswers points WorkBuddy's China endpoint at a test server and
// answers each of its account paths with the JSON in answers, keyed by path
// suffix. A value starting with "status:" answers with a bare status code.
func workBuddyCheckinAnswers(t *testing.T, answers map[string]string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for suffix, body := range answers {
			if !strings.HasSuffix(r.URL.Path, suffix) {
				continue
			}
			if strings.HasPrefix(body, "status:") {
				w.WriteHeader(atoiOr(strings.TrimPrefix(body, "status:"), 500))
				w.Write([]byte("<html>gateway</html>"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		t.Errorf("unexpected WorkBuddy call to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	old := workBuddyCNEndpoint
	workBuddyCNEndpoint = server.URL
	t.Cleanup(func() { workBuddyCNEndpoint = old })
	return server.URL
}

func atoiOr(text string, fallback int) int {
	value := 0
	for _, char := range text {
		if char < '0' || char > '9' {
			return fallback
		}
		value = value*10 + int(char-'0')
	}
	if value == 0 {
		return fallback
	}
	return value
}

func workBuddyCheckinTestCredential() domain.WorkBuddyCredential {
	return domain.WorkBuddyCredential{AccessToken: "tok", UserID: "u-1", AccountLabel: "tester"}
}

func runWorkBuddyCheckin(t *testing.T, site workBuddySite) checkinResult {
	t.Helper()
	result, _ := workBuddyCheckin(context.Background(), site, workBuddyCheckinTestCredential(), nil)
	return result
}

func TestWorkBuddyCheckinClaimsWhenTheEventIsOpen(t *testing.T) {
	workBuddyCheckinAnswers(t, map[string]string{
		workBuddyCheckinStatusPath: `{"code":0,"data":{"active":true,"today_checked_in":false,"streak_days":14,"daily_credit":100}}`,
		workBuddyCheckinClaimPath:  `{"code":0,"data":{"credit":"100","streak_days":"15","message":"签到成功"}}`,
	})
	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	result := runWorkBuddyCheckin(t, site)
	if result.Outcome != domain.CheckinClaimed {
		t.Fatalf("outcome = %q (%s)", result.Outcome, result.Message)
	}
	if result.Credit != 100 || result.Streak != 15 {
		t.Fatalf("credit/streak = %v/%v", result.Credit, result.Streak)
	}
	if result.AccountKey != domain.AuthTypeWorkBuddy+"|u-1" {
		t.Fatalf("account key = %q", result.AccountKey)
	}
}

// Pressing 签到 in the app counts: the gateway must report the day as done and
// show the vendor's streak, not claim credits a second time.
func TestWorkBuddyCheckinReportsWhatTheAppAlreadyPressed(t *testing.T) {
	workBuddyCheckinAnswers(t, map[string]string{
		workBuddyCheckinStatusPath: `{"code":0,"data":{"active":true,"today_checked_in":true,"streak_days":9,"today_credit":"60"}}`,
	})
	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	result := runWorkBuddyCheckin(t, site)
	if result.Outcome != domain.CheckinDone || result.Credit != 60 || result.Streak != 9 {
		t.Fatalf("result = %+v", result)
	}
}

func TestWorkBuddyCheckinAnswers(t *testing.T) {
	cases := []struct {
		name    string
		answers map[string]string
		want    string
	}{
		{"no event running", map[string]string{workBuddyCheckinStatusPath: `{"code":0,"data":{"active":false}}`}, domain.CheckinInactive},
		{"the account can't take part", map[string]string{workBuddyCheckinStatusPath: `{"code":1002,"msg":"该账号不参与本次活动"}`}, domain.CheckinIneligible},
		{"the event ended", map[string]string{workBuddyCheckinStatusPath: `{"code":1003,"msg":"活动已结束"}`}, domain.CheckinInactive},
		{"an already-claimed day is done", map[string]string{
			workBuddyCheckinStatusPath: `{"code":0,"data":{"active":true,"today_checked_in":false,"streak_days":3}}`,
			workBuddyCheckinClaimPath:  `{"code":1001,"msg":"今日已签到"}`,
		}, domain.CheckinDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workBuddyCheckinAnswers(t, tc.answers)
			site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
			result := runWorkBuddyCheckin(t, site)
			if result.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (%s)", result.Outcome, tc.want, result.Message)
			}
		})
	}
}

// The claim's streak wins over the status's: the status was read a moment before
// the credits landed, and 15 is the number WorkBuddy now counts.
func TestWorkBuddyCheckinKeepsTheClaimStreak(t *testing.T) {
	workBuddyCheckinAnswers(t, map[string]string{
		workBuddyCheckinStatusPath: `{"code":0,"data":{"active":true,"today_checked_in":false,"streak_days":14}}`,
		workBuddyCheckinClaimPath:  `{"code":1001,"msg":"今日已签到"}`,
	})
	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	result := runWorkBuddyCheckin(t, site)
	if result.Outcome != domain.CheckinDone || result.Streak != 14 {
		t.Fatalf("result = %+v", result)
	}
}

// WorkBuddy answers the check-in alone with a bare 401 for some accounts: its
// own app signs that one call with a device token from its security SDK. When the
// credits still read, that is the day's answer, not a failure to retry for hours.
func TestWorkBuddyCheckin401IsTheAppWhenCreditsRead(t *testing.T) {
	workBuddyCheckinAnswers(t, map[string]string{
		workBuddyCheckinStatusPath:   "status:401",
		workBuddyResourceSummaryPath: `{"code":0,"data":{"IsPaidUser":true,"Packages":[]}}`,
	})
	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	result := runWorkBuddyCheckin(t, site)
	if result.Outcome != domain.CheckinOwnApp {
		t.Fatalf("outcome = %q, want %q (%s)", result.Outcome, domain.CheckinOwnApp, result.Message)
	}
	if !strings.Contains(result.Message, "WorkBuddy") {
		t.Fatalf("the message should name where to check in: %q", result.Message)
	}
}

// The same 401 with unreadable credits is the sign-in itself lapsing, which the
// user can only fix by signing in again.
func TestWorkBuddyCheckin401IsASignInWhenCreditsFail(t *testing.T) {
	workBuddyCheckinAnswers(t, map[string]string{
		workBuddyCheckinStatusPath:   "status:401",
		workBuddyResourceSummaryPath: "status:401",
	})
	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	result := runWorkBuddyCheckin(t, site)
	if result.Outcome != domain.CheckinFailed {
		t.Fatalf("outcome = %q", result.Outcome)
	}
	if !strings.Contains(result.Message, "重新登录") {
		t.Fatalf("message should say to sign in again: %q", result.Message)
	}
}

func TestWorkBuddyCheckinUnauthorizedReadsBothShapes(t *testing.T) {
	if !workBuddyUnauthorized(&workBuddyError{code: http.StatusUnauthorized}) {
		t.Fatal("an envelope 401 must be recognised")
	}
	if !workBuddyUnauthorized(&workBuddyError{code: 11153, status: http.StatusUnauthorized, msg: "refused"}) {
		t.Fatal("a 401 carrying a business code is still a 401")
	}
	if !workBuddyUnauthorized(errors.New("WorkBuddy 返回 HTTP 401")) {
		t.Fatal("a bare status line must be recognised too")
	}
	if workBuddyUnauthorized(&workBuddyError{code: workBuddyCheckinAlready}) {
		t.Fatal("a business code is not a 401")
	}
	if workBuddyUnauthorized(nil) {
		t.Fatal("no error is not a 401")
	}
}

// A token WorkBuddy revoked early shows up as a 401 on the check-in and on the
// credits probe alike. That one is repairable: refresh once, then press the
// day's check-in with the new token instead of writing off the account.
func TestWorkBuddyCheckinRefreshesARevokedToken(t *testing.T) {
	statusCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, workBuddyCheckinStatusPath):
			statusCalls++
			if strings.Contains(r.Header.Get("Authorization"), "stale") {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("<html>gateway</html>"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":false}}`))
		case strings.HasSuffix(r.URL.Path, workBuddyResourceSummaryPath):
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("<html>gateway</html>"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	old := workBuddyCNEndpoint
	workBuddyCNEndpoint = server.URL
	defer func() { workBuddyCNEndpoint = old }()

	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	refreshes := 0
	result, updated := workBuddyCheckin(context.Background(), site,
		domain.WorkBuddyCredential{AccessToken: "stale", UserID: "u-1"},
		func() (domain.WorkBuddyCredential, error) {
			refreshes++
			return domain.WorkBuddyCredential{AccessToken: "fresh", UserID: "u-1"}, nil
		})
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want exactly one", refreshes)
	}
	if updated.AccessToken != "fresh" {
		t.Fatalf("credential = %+v, want the rotated one", updated)
	}
	if statusCalls != 2 {
		t.Fatalf("status calls = %d, want the refusal and the retry", statusCalls)
	}
	if result.Outcome != domain.CheckinInactive {
		t.Fatalf("outcome = %q (%s)", result.Outcome, result.Message)
	}
}
