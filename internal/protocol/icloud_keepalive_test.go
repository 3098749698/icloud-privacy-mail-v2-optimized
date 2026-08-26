package protocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestKeepAliveRefreshesTokenBeforePortalWarm(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("scnt", "fresh-scnt")
		w.Header().Set("X-Apple-ID-Session-Id", "fresh-session")
		switch r.URL.Path {
		case "/account/manage/section/privacy":
			http.SetCookie(w, &http.Cookie{Name: "caw-at", Value: "rotated", Path: "/"})
			_, _ = w.Write([]byte("<html></html>"))
		case "/bootstrap/portal", "/account/manage/gs/ws/token":
			_, _ = w.Write([]byte(`{"timeOutInterval":15}`))
		case "/session/extend":
			_, _ = w.Write([]byte(`{}`))
		case "/account/manage":
			_, _ = w.Write([]byte(`{"apiKey":"fresh-key"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	previousBaseURL := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	defer func() { appleAccountManageBaseURL = previousBaseURL }()

	client := NewICloudClient()
	state, err := client.KeepAliveAppleAccountManageState(context.Background(), LoginState{
		Kind:   LoginStateAppleAccount,
		Host:   "appleid.apple.com",
		Origin: "https://account.apple.com",
		Scnt:   "saved-scnt",
		APIKey: "saved-key",
		Cookies: []SessionCookie{{
			Name: "session", Value: "fixture", Path: "/",
		}},
	})
	if err != nil {
		t.Fatalf("保活失败：%v", err)
	}
	if state.Scnt != "fresh-scnt" || state.SessionID != "fresh-session" || state.APIKey != "fresh-key" {
		t.Fatalf("保活未保存轮换后的状态：%+v", state)
	}
	if len(paths) < 3 || paths[0] != "/session/extend" || paths[1] != "/account/manage/gs/ws/token" || paths[2] != "/account/manage" {
		t.Fatalf("保活请求顺序不正确：%v", paths)
	}
}

func TestKeepAliveExtendsParentSessionWithCurrentHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/extend":
			if r.Header.Get("X-Apple-Api-Key") != "saved-key" || r.Header.Get("scnt") != "saved-scnt" || r.Header.Get("X-Apple-ID-Session-Id") != "saved-session" {
				t.Fatalf("父会话续期请求头不完整：%v", r.Header)
			}
			http.SetCookie(w, &http.Cookie{Name: "caw-at", Value: "extended", Path: "/"})
			w.Header().Set("scnt", "extended-scnt")
			w.Header().Set("X-Apple-ID-Session-Id", "extended-session")
			_, _ = w.Write([]byte(`{}`))
		case "/account/manage/gs/ws/token":
			if !strings.Contains(r.Header.Get("Cookie"), "caw-at=extended") || r.Header.Get("scnt") != "extended-scnt" {
				t.Fatalf("token 刷新没有使用续期后的状态：cookie=%q scnt=%q", r.Header.Get("Cookie"), r.Header.Get("scnt"))
			}
			_, _ = w.Write([]byte(`{"timeOutInterval":15}`))
		case "/account/manage":
			_, _ = w.Write([]byte(`{"apiKey":"saved-key"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	previousBaseURL := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	defer func() { appleAccountManageBaseURL = previousBaseURL }()

	state, err := NewICloudClient().KeepAliveAppleAccountManageState(context.Background(), LoginState{
		Kind: LoginStateAppleAccount, Scnt: "saved-scnt", SessionID: "saved-session", APIKey: "saved-key",
		Cookies: []SessionCookie{{Name: "session", Value: "fixture", Path: "/"}},
	})
	if err != nil {
		t.Fatalf("父会话续期失败：%v", err)
	}
	if state.Scnt != "extended-scnt" || state.SessionID != "extended-session" || !state.LastCheckOK {
		t.Fatalf("续期后的状态没有保存：%+v", state)
	}
}

func TestKeepAliveFailureReturnsOriginalState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/account/manage/section/privacy":
			http.SetCookie(w, &http.Cookie{Name: "caw-at", Value: "rotated", Path: "/"})
			w.Header().Set("scnt", "partial-scnt")
			_, _ = w.Write([]byte("<html></html>"))
		case "/bootstrap/portal":
			_, _ = w.Write([]byte(`{"timeOutInterval":15}`))
		case "/account/manage/gs/ws/token":
			w.WriteHeader(http.StatusUnauthorized)
		case "/session/extend":
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	previousBaseURL := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	defer func() { appleAccountManageBaseURL = previousBaseURL }()

	savedAt := time.Now().Add(-time.Hour).Round(time.Second)
	expiresAt := savedAt.Add(15 * time.Minute)
	original := LoginState{
		Kind:            LoginStateAppleAccount,
		Host:            "appleid.apple.com",
		Origin:          "https://account.apple.com",
		SavedAt:         savedAt,
		Scnt:            "saved-scnt",
		SessionID:       "saved-session",
		APIKey:          "saved-key",
		ManageExpiresAt: expiresAt,
		Cookies: []SessionCookie{{
			Name: "session", Value: "fixture", Path: "/",
		}},
	}

	state, err := NewICloudClient().KeepAliveAppleAccountManageState(context.Background(), original)
	if err == nil {
		t.Fatal("期望短期 token 刷新返回 401")
	}
	code, _, _ := ErrorDetails(err)
	if code != "apple_account_auth_failed" {
		t.Fatalf("错误类型为 %q，期望 apple_account_auth_failed", code)
	}
	if state.Scnt != original.Scnt || state.SessionID != original.SessionID || !state.SavedAt.Equal(savedAt) || !state.ManageExpiresAt.Equal(expiresAt) {
		t.Fatalf("失败后保存了部分刷新状态：%+v", state)
	}
	if len(state.Cookies) != 1 || state.Cookies[0].Value != "fixture" {
		t.Fatalf("失败后 Cookie 被部分替换：%+v", state.Cookies)
	}
}

func TestRefreshRecoversFromStaleScntWithRotatedCookie(t *testing.T) {
	var tokenCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/account/manage/gs/ws/token":
			tokenCalls++
			if tokenCalls == 1 {
				if r.Header.Get("scnt") != "stale-scnt" {
					t.Fatalf("首次刷新没有优先使用当前 scnt：%q", r.Header.Get("scnt"))
				}
				http.SetCookie(w, &http.Cookie{Name: "caw-at", Value: "rotated", Path: "/"})
				w.Header().Set("scnt", "rotated-scnt")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("scnt") != "rotated-scnt" {
				t.Fatalf("401 后没有使用轮换的 scnt：%q", r.Header.Get("scnt"))
			}
			if !strings.Contains(r.Header.Get("Cookie"), "caw-at=rotated") {
				t.Fatalf("恢复请求没有携带 401 响应轮换的 Cookie：%q", r.Header.Get("Cookie"))
			}
			w.Header().Set("scnt", "fresh-scnt")
			_, _ = w.Write([]byte(`{"timeOutInterval":15}`))
		case "/account/manage/section/privacy", "/bootstrap/portal":
			_, _ = w.Write([]byte(`{}`))
		case "/account/manage":
			_, _ = w.Write([]byte(`{"apiKey":"fresh-key"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	previousBaseURL := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	defer func() { appleAccountManageBaseURL = previousBaseURL }()

	state, err := NewICloudClient().RefreshAppleAccountManageState(context.Background(), LoginState{
		Kind:   LoginStateAppleAccount,
		Host:   "appleid.apple.com",
		Origin: "https://account.apple.com",
		Scnt:   "stale-scnt",
		APIKey: "stale-key",
		Cookies: []SessionCookie{{
			Name: "session", Value: "fixture", Path: "/",
		}},
	})
	if err != nil {
		t.Fatalf("旧 scnt 恢复失败：%v", err)
	}
	if tokenCalls != 2 || state.Scnt != "fresh-scnt" || state.APIKey != "fresh-key" {
		t.Fatalf("恢复后的状态不正确：calls=%d state=%+v", tokenCalls, state)
	}
}

func TestAppleRedirectPreservesIntermediateSessionState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.SetCookie(w, &http.Cookie{Name: "caw-at", Value: "redirect-cookie", Path: "/"})
			w.Header().Set("scnt", "redirect-scnt")
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		if !strings.Contains(r.Header.Get("Cookie"), "caw-at=redirect-cookie") {
			t.Fatalf("重定向请求没有带上中间响应 Cookie：%q", r.Header.Get("Cookie"))
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	state := LoginState{Cookies: []SessionCookie{{Name: "session", Value: "fixture", Path: "/"}}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatalf("创建请求失败：%v", err)
	}
	resp, err := NewICloudClient().doAppleAccountWithRedirectState(req, &state)
	if err != nil {
		t.Fatalf("跟随重定向失败：%v", err)
	}
	_ = resp.Body.Close()
	if state.Scnt != "redirect-scnt" || !strings.Contains(cookieHeader(state.Cookies, server.URL+"/final"), "caw-at=redirect-cookie") {
		t.Fatalf("中间响应状态没有保存：%+v", state)
	}
}

func TestCookieHeaderPrefersLongerMatchingPath(t *testing.T) {
	header := cookieHeader([]SessionCookie{
		{Name: "session", Value: "root", Domain: "example.com", Path: "/"},
		{Name: "session", Value: "account", Domain: "example.com", Path: "/account"},
	}, "https://example.com/account/manage")
	if header != "session=account; session=root" {
		t.Fatalf("Cookie Path 顺序不正确：%q", header)
	}
}

func TestCreateDoesNotPostponeManagementRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("scnt", "rotated-scnt")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/account/manage/email/private/add":
			_, _ = w.Write([]byte(`{"emailAddress":"created@icloud.com"}`))
		case r.Method == http.MethodPut && r.URL.Path == "/account/manage/email/private/add/complete":
			_, _ = w.Write([]byte(`{"emailAddress":"created@icloud.com","active":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	previousBaseURL := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	defer func() { appleAccountManageBaseURL = previousBaseURL }()

	checkedAt := time.Now().Add(-time.Minute).Round(time.Second)
	session := ICloudSession{
		AccountID: "acc_fixture",
		AppleID:   "fixture@icloud.com",
		LoginStates: []LoginState{{
			Kind:            LoginStateAppleAccount,
			Host:            "appleid.apple.com",
			Origin:          "https://account.apple.com",
			Scnt:            "saved-scnt",
			SessionID:       "saved-session",
			APIKey:          "saved-key",
			ManageExpiresAt: time.Now().Add(10 * time.Minute),
			LastCheckedAt:   checkedAt,
			LastCheckOK:     true,
			Cookies:         []SessionCookie{{Name: "session", Value: "fixture", Path: "/"}},
		}},
	}

	remote, updated, err := NewICloudClient().CreatePrivacyMailboxWithAppleAccount(context.Background(), session, "", "label", "note")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if remote.Email != "created@icloud.com" {
		t.Fatalf("创建结果异常：%+v", remote)
	}
	state, ok := LoginStateForKind(updated, LoginStateAppleAccount)
	if !ok || !state.LastCheckedAt.Equal(checkedAt) {
		t.Fatalf("创建操作推迟了管理 token 保活：%+v", state)
	}
	if state.Scnt != "rotated-scnt" {
		t.Fatalf("创建响应中的 scnt 没有保存：%+v", state)
	}
}

func TestAppleAccountOperationKeyMatchesKeepAliveAndCreate(t *testing.T) {
	state := LoginState{SessionID: "shared-session", Scnt: "shared-scnt", APIKey: "shared-key"}
	createKey := appleAccountOperationKey(ICloudSession{OwnerID: "admin_1", AccountID: "account_1", AppleID: "owner@icloud.com"}, state)
	keepAliveKey := appleAccountOperationKey(ICloudSession{}, state)
	if createKey != keepAliveKey {
		t.Fatalf("创建与保活使用了不同会话锁：create=%q keepalive=%q", createKey, keepAliveKey)
	}
	if strings.Contains(createKey, "admin_1") {
		t.Fatalf("会话锁不应包含仅完整 session 才有的 OwnerID：%q", createKey)
	}
}
