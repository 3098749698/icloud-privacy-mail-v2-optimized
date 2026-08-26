package protocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSupportsAppleAccountMailboxManagement(t *testing.T) {
	withState := func(state LoginState) ICloudSession {
		state.Kind = LoginStateAppleAccount
		return ICloudSession{LoginStates: []LoginState{state}}
	}
	cookie := []SessionCookie{{Name: "session", Value: "fixture", Path: "/"}}
	tests := []struct {
		name     string
		session  ICloudSession
		fallback string
		want     bool
	}{
		{name: "missing state", session: ICloudSession{}, want: false},
		{name: "missing cookies", session: withState(LoginState{APIKey: "saved-key"}), want: false},
		{name: "missing key", session: withState(LoginState{Cookies: cookie}), want: false},
		{name: "saved key without scnt", session: withState(LoginState{Cookies: cookie, APIKey: "saved-key"}), want: true},
		{name: "fallback key without scnt", session: withState(LoginState{Cookies: cookie}), fallback: "fallback-key", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := SupportsAppleAccountMailboxManagement(test.session, test.fallback); got != test.want {
				t.Fatalf("SupportsAppleAccountMailboxManagement()=%t，期望=%t", got, test.want)
			}
		})
	}
}

func TestDeletePrivacyMailboxWithAppleAccountStopsActiveAliasThenRemoves(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get("X-Apple-Api-Key") != "saved-key" {
			t.Errorf("api_key=%q，期望 saved-key", r.Header.Get("X-Apple-Api-Key"))
		}
		switch r.URL.Path {
		case "/account/manage/email/private/alias-id/stop":
			if r.Method != http.MethodDelete {
				t.Errorf("停用方法=%s，期望 DELETE", r.Method)
			}
			http.SetCookie(w, &http.Cookie{Name: "caw-at", Value: "after-stop", Path: "/"})
			w.Header().Set("scnt", "stop-scnt")
			w.WriteHeader(http.StatusNoContent)
		case "/account/manage/email/private/alias-id/remove":
			if r.Method != http.MethodDelete {
				t.Errorf("移除方法=%s，期望 DELETE", r.Method)
			}
			if !strings.Contains(r.Header.Get("Cookie"), "caw-at=after-stop") {
				t.Errorf("移除请求未使用停用响应轮换的 Cookie：%q", r.Header.Get("Cookie"))
			}
			if r.Header.Get("scnt") != "stop-scnt" {
				t.Errorf("移除请求 scnt=%q，期望 stop-scnt", r.Header.Get("scnt"))
			}
			w.Header().Set("X-Apple-ID-Session-Id", "remove-session")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restoreAppleAccountBaseURL(t, server.URL)

	updated, err := NewICloudClient().DeletePrivacyMailboxWithAppleAccount(
		context.Background(), appleAccountDeleteTestSession("saved-key"), "", "alias-id", true,
	)
	if err != nil {
		t.Fatalf("删除活跃别名失败：%v", err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	wantPaths := []string{
		"DELETE /account/manage/email/private/alias-id/stop",
		"DELETE /account/manage/email/private/alias-id/remove",
	}
	if strings.Join(gotPaths, "|") != strings.Join(wantPaths, "|") {
		t.Fatalf("请求顺序=%v，期望=%v", gotPaths, wantPaths)
	}
	state, ok := LoginStateForKind(updated, LoginStateAppleAccount)
	if !ok || state.Scnt != "stop-scnt" || state.SessionID != "remove-session" {
		t.Fatalf("未保存响应轮换的管理态：%+v", state)
	}
	if !strings.Contains(cookieHeader(state.Cookies, server.URL+"/account/manage"), "caw-at=after-stop") {
		t.Fatalf("未保存响应轮换的 Cookie：%+v", state.Cookies)
	}
}

func TestDeletePrivacyMailboxWithAppleAccountInactiveUsesFallbackKey(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || r.URL.Path != "/account/manage/email/private/inactive-id/remove" {
			t.Errorf("非活跃别名请求=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Apple-Api-Key") != "fallback-key" {
			t.Errorf("api_key=%q，期望 fallback-key", r.Header.Get("X-Apple-Api-Key"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	restoreAppleAccountBaseURL(t, server.URL)

	updated, err := NewICloudClient().DeletePrivacyMailboxWithAppleAccount(
		context.Background(), appleAccountDeleteTestSession(""), " fallback-key ", "inactive-id", false,
	)
	if err != nil {
		t.Fatalf("删除非活跃别名失败：%v", err)
	}
	if calls != 1 {
		t.Fatalf("请求次数=%d，期望仅 remove 一次", calls)
	}
	state, _ := LoginStateForKind(updated, LoginStateAppleAccount)
	if state.APIKey != "fallback-key" {
		t.Fatalf("fallback api_key 未写入返回会话：%+v", state)
	}
}

func TestDeletePrivacyMailboxWithAppleAccountRefreshesOnceAfterAuthFailure(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	removeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/account/manage/email/private/alias-id/remove":
			removeCalls++
			if removeCalls == 1 {
				http.SetCookie(w, &http.Cookie{Name: "caw-at", Value: "rotated-on-401", Path: "/"})
				w.Header().Set("scnt", "rotated-scnt")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-Apple-Api-Key") != "fresh-key" || r.Header.Get("scnt") != "fresh-scnt" {
				t.Errorf("重试未使用刷新态：api_key=%q scnt=%q", r.Header.Get("X-Apple-Api-Key"), r.Header.Get("scnt"))
			}
			if !strings.Contains(r.Header.Get("Cookie"), "caw-at=rotated-on-401") {
				t.Errorf("刷新重试未保留 401 轮换 Cookie：%q", r.Header.Get("Cookie"))
			}
			http.SetCookie(w, &http.Cookie{Name: "caw-at", Value: "final-cookie", Path: "/"})
			w.WriteHeader(http.StatusNoContent)
		case "/account/manage/gs/ws/token":
			if r.Header.Get("scnt") != "rotated-scnt" || !strings.Contains(r.Header.Get("Cookie"), "caw-at=rotated-on-401") {
				t.Errorf("刷新未使用鉴权失败响应的最新状态：scnt=%q cookie=%q", r.Header.Get("scnt"), r.Header.Get("Cookie"))
			}
			w.Header().Set("scnt", "fresh-scnt")
			_, _ = w.Write([]byte(`{"timeOutInterval":15}`))
		case "/account/manage":
			_, _ = w.Write([]byte(`{"apiKey":"fresh-key"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restoreAppleAccountBaseURL(t, server.URL)

	updated, err := NewICloudClient().DeletePrivacyMailboxWithAppleAccount(
		context.Background(), appleAccountDeleteTestSession("stale-key"), "", "alias-id", false,
	)
	if err != nil {
		t.Fatalf("鉴权刷新后删除失败：%v", err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	wantPaths := []string{
		"DELETE /account/manage/email/private/alias-id/remove",
		"GET /account/manage/gs/ws/token",
		"GET /account/manage",
		"DELETE /account/manage/email/private/alias-id/remove",
	}
	if strings.Join(gotPaths, "|") != strings.Join(wantPaths, "|") {
		t.Fatalf("鉴权恢复顺序=%v，期望=%v", gotPaths, wantPaths)
	}
	state, _ := LoginStateForKind(updated, LoginStateAppleAccount)
	if state.APIKey != "fresh-key" || state.Scnt != "fresh-scnt" {
		t.Fatalf("刷新后的管理态未保存：%+v", state)
	}
	if !strings.Contains(cookieHeader(state.Cookies, server.URL+"/account/manage"), "caw-at=final-cookie") {
		t.Fatalf("删除重试响应的 Cookie 未保存：%+v", state.Cookies)
	}
}

func TestDeletePrivacyMailboxWithAppleAccountReportsStopBranchAndSkipsRemove(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason":"alias cannot be stopped"}`))
	}))
	defer server.Close()
	restoreAppleAccountBaseURL(t, server.URL)

	_, err := NewICloudClient().DeletePrivacyMailboxWithAppleAccount(
		context.Background(), appleAccountDeleteTestSession("saved-key"), "", "alias-id", true,
	)
	if err == nil {
		t.Fatal("期望停用失败")
	}
	code, message, _ := ErrorDetails(err)
	if code != "apple_account_hme_stop_failed" || !strings.Contains(message, "阶段：停用隐私邮箱") {
		t.Fatalf("停用分支诊断不完整：code=%q message=%q", code, message)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/stop") {
		t.Fatalf("停用失败后仍执行了其他分支：%v", paths)
	}
}

func TestDeletePrivacyMailboxWithAppleAccountValidatesRequiredState(t *testing.T) {
	client := NewICloudClient()
	tests := []struct {
		name     string
		session  ICloudSession
		fallback string
		id       string
		wantCode string
	}{
		{name: "missing id", session: appleAccountDeleteTestSession("key"), id: " ", wantCode: "apple_account_hme_id_missing"},
		{name: "missing state", session: ICloudSession{}, id: "id", wantCode: "apple_account_session_missing"},
		{name: "missing cookies", session: ICloudSession{LoginStates: []LoginState{{Kind: LoginStateAppleAccount, APIKey: "key"}}}, id: "id", wantCode: "apple_account_session_missing"},
		{name: "missing key", session: appleAccountDeleteTestSession(""), id: "id", wantCode: "apple_account_api_key_missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.DeletePrivacyMailboxWithAppleAccount(context.Background(), test.session, test.fallback, test.id, false)
			code, _, _ := ErrorDetails(err)
			if code != test.wantCode {
				t.Fatalf("错误 code=%q，期望=%q；err=%v", code, test.wantCode, err)
			}
		})
	}
}

func appleAccountDeleteTestSession(apiKey string) ICloudSession {
	return ICloudSession{
		AccountID: "account-fixture",
		LoginStates: []LoginState{{
			Kind:    LoginStateAppleAccount,
			Host:    "appleid.apple.com",
			Origin:  "https://account.apple.com",
			APIKey:  apiKey,
			Cookies: []SessionCookie{{Name: "session", Value: "fixture", Path: "/"}},
		}},
	}
}

func restoreAppleAccountBaseURL(t *testing.T, baseURL string) {
	t.Helper()
	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = baseURL
	t.Cleanup(func() { appleAccountManageBaseURL = previous })
}
