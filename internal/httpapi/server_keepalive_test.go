package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

func TestAppleKeepAliveScanIntervalPollsBeforeBaseInterval(t *testing.T) {
	if got := appleKeepAliveScanInterval(3 * time.Minute); got != 30*time.Second {
		t.Fatalf("3 分钟基础周期的扫描间隔为 %s，期望 30s", got)
	}
	if got := appleKeepAliveScanInterval(12 * time.Second); got != 5*time.Second {
		t.Fatalf("12 秒基础周期的扫描间隔为 %s，期望 5s", got)
	}
}

func TestAPIResponsesDisableCaching(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("API 缓存策略为 %q，期望 no-store", got)
	}
}

func TestAppleKeepAliveRoundSavesUpdatedState(t *testing.T) {
	state, session := newKeepAliveTestState(t, true, time.Now().Add(-time.Hour))
	cfg := config.Default()
	cfg.AppleAccountKeepAliveJitterPercent = 0
	server := New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls int
	server.keepAliveState = func(_ context.Context, loginState domain.LoginState) (domain.LoginState, error) {
		calls++
		loginState.Scnt = "fresh-scnt"
		loginState.APIKey = "fresh-key"
		return loginState, nil
	}

	server.keepAliveAppleRound(context.Background(), 3*time.Minute)

	if calls != 1 {
		t.Fatalf("保活调用次数为 %d，期望 1", calls)
	}
	updated, ok := state.ICloudSessionByAccountID(session.AccountID)
	if !ok {
		t.Fatal("没有找到保活后的 Apple 登录态")
	}
	loginState, ok := protocol.LoginStateForKind(updated, domain.LoginStateAppleAccount)
	if !ok || loginState.Scnt != "fresh-scnt" || loginState.APIKey != "fresh-key" || !loginState.LastCheckOK || loginState.LastStatusMessage != "Apple Account 登录态保活成功" {
		t.Fatalf("保活后的登录态不正确：%+v，存在=%t", loginState, ok)
	}
	assertKeepAliveEvent(t, state, "开始 Apple 登录态保活：keepalive@example.com")
	assertKeepAliveEvent(t, state, "Apple 登录态保活成功：keepalive@example.com")
}

func TestAppleKeepAliveRoundRecordsTransientFailure(t *testing.T) {
	state, _ := newKeepAliveTestState(t, true, time.Now().Add(-time.Hour))
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.keepAliveState = func(_ context.Context, loginState domain.LoginState) (domain.LoginState, error) {
		return loginState, errors.New("连接暂时超时")
	}

	server.keepAliveAppleRound(context.Background(), 3*time.Minute)

	assertKeepAliveEvent(t, state, "开始 Apple 登录态保活：keepalive@example.com")
	assertKeepAliveEvent(t, state, "Apple 登录态保活暂时失败：keepalive@example.com")
}

func TestAppleKeepAliveRoundSkipsFreshState(t *testing.T) {
	state, _ := newKeepAliveTestState(t, true, time.Now())
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.keepAliveState = func(_ context.Context, loginState domain.LoginState) (domain.LoginState, error) {
		t.Fatal("不应调用尚未到期的登录态")
		return loginState, nil
	}
	server.keepAliveAppleRound(context.Background(), 3*time.Minute)
}

func TestAppleKeepAliveRoundDoesNotPersistPartialStateOnTransientFailure(t *testing.T) {
	state, session := newKeepAliveTestState(t, true, time.Now().Add(-time.Hour))
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls int
	server.keepAliveState = func(_ context.Context, loginState domain.LoginState) (domain.LoginState, error) {
		calls++
		loginState.Cookies = append(loginState.Cookies, domain.SessionCookie{Name: "rotated", Value: "cookie", Domain: "apple.com", Path: "/"})
		return loginState, errors.New("短期令牌刷新失败")
	}

	server.keepAliveAppleRound(context.Background(), 3*time.Minute)
	if calls != 1 {
		t.Fatalf("失效状态重试次数为 %d，期望 1", calls)
	}
	updated, ok := state.ICloudSessionByAccountID(session.AccountID)
	if !ok {
		t.Fatal("没有找到重试后的登录态")
	}
	loginState, ok := protocol.LoginStateForKind(updated, domain.LoginStateAppleAccount)
	if !ok || !loginState.LastCheckOK || len(loginState.Cookies) != 1 || !strings.Contains(loginState.LastStatusMessage, "自动恢复中") {
		t.Fatalf("保活失败状态保存不正确：%+v，存在=%t", loginState, ok)
	}
	assertKeepAliveEvent(t, state, "后自动重试")
}

func TestAppleKeepAliveEligibleRetriesConfirmedFailure(t *testing.T) {
	state := domain.LoginState{
		Scnt:              "saved-scnt",
		APIKey:            "saved-key",
		Cookies:           []domain.SessionCookie{{Name: "session", Value: "fixture"}},
		LastCheckedAt:     time.Now(),
		LastCheckOK:       false,
		LastStatusMessage: "Apple Account 登录态已失效",
	}
	if !appleKeepAliveEligible(state) {
		t.Fatal("尚未熔断的异常登录态没有进入后台恢复调度")
	}
	state.ReloginRequired = true
	if appleKeepAliveEligible(state) {
		t.Fatal("明确要求重新登录的状态仍进入了后台恢复调度")
	}
}

func TestAppleKeepAliveFailureBackoffAndFutureTargetArePreserved(t *testing.T) {
	state, session := newKeepAliveTestState(t, true, time.Now().Add(-time.Hour))
	cfg := config.Default()
	cfg.AppleAccountKeepAliveJitterPercent = 0
	server := New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls int
	server.keepAliveState = func(_ context.Context, loginState domain.LoginState) (domain.LoginState, error) {
		calls++
		return loginState, protocol.NewError("apple_account_auth_failed", "登录态已失效", true)
	}

	server.keepAliveAppleRound(context.Background(), 3*time.Minute)
	key := appleKeepAliveSessionKey(session)
	first := server.keepAliveTargets[key]
	if first.Failures != 1 || first.Interval != time.Minute {
		t.Fatalf("首次失败退避异常：%+v", first)
	}

	// Store round-trips time values. A future target must still win even if the
	// persisted state timestamp differs from the in-memory checked timestamp.
	stored, _ := state.ICloudSessionByAccountID(session.AccountID)
	loginState, _ := protocol.LoginStateForKind(stored, domain.LoginStateAppleAccount)
	loginState.LastCheckedAt = loginState.LastCheckedAt.Add(time.Millisecond)
	stored = protocol.WithLoginState(stored, loginState)
	if _, err := state.SaveICloudSession(stored); err != nil {
		t.Fatalf("保存测试登录态失败：%v", err)
	}
	server.keepAliveAppleRound(context.Background(), 3*time.Minute)
	if calls != 1 {
		t.Fatalf("未来重试时间未生效，保活调用次数=%d", calls)
	}

	server.keepAliveMu.Lock()
	forced := server.keepAliveTargets[key]
	forced.NextAt = time.Now().Add(-time.Second)
	server.keepAliveTargets[key] = forced
	server.keepAliveMu.Unlock()
	server.keepAliveAppleRound(context.Background(), 3*time.Minute)
	second := server.keepAliveTargets[key]
	if calls != 2 || second.Failures != 2 || second.Interval != 2*time.Minute {
		t.Fatalf("连续失败退避异常：calls=%d target=%+v", calls, second)
	}
}

func TestAppleKeepAliveUsesManageExpiryMinusLeadTime(t *testing.T) {
	server := New(config.Default(), mustOpenKeepAliveStore(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	checkedAt := now.Add(-time.Minute)
	expiresAt := now.Add(14 * time.Minute)
	nextAt, interval := server.nextAppleKeepAliveTarget(domain.LoginState{
		LastCheckedAt:   checkedAt,
		ManageExpiresAt: expiresAt,
	}, 3*time.Minute, now)
	if want := expiresAt.Add(-appleKeepAliveRefreshLead); !nextAt.Equal(want) {
		t.Fatalf("动态保活目标=%s，期望=%s", nextAt, want)
	}
	if want := nextAt.Sub(checkedAt); interval != want {
		t.Fatalf("动态保活间隔=%s，期望=%s", interval, want)
	}
}

func TestAppleKeepAliveAuthFailureThresholdIsConsecutive(t *testing.T) {
	server := New(config.Default(), mustOpenKeepAliveStore(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	session := domain.ICloudSession{AccountID: "fixture"}
	state := domain.LoginState{LastCheckedAt: time.Now()}
	for attempt := 1; attempt <= appleKeepAliveAuthFailThreshold; attempt++ {
		_, authFails := server.retryAppleKeepAliveTarget(session, state, 3*time.Minute, true)
		if authFails != attempt {
			t.Fatalf("第 %d 次认证失败计数=%d", attempt, authFails)
		}
	}
	_, authFails := server.retryAppleKeepAliveTarget(session, state, 3*time.Minute, false)
	if authFails != 0 {
		t.Fatalf("非认证失败应中断连续认证失败计数，得到 %d", authFails)
	}
}

func TestAppleKeepAliveFailureStatusWaitsForAuthThreshold(t *testing.T) {
	state := domain.LoginState{LastCheckOK: true}
	if confirmed := applyAppleKeepAliveFailureStatus(&state, true, 1); confirmed || !state.LastCheckOK || !strings.Contains(state.LastStatusMessage, "自动恢复中") {
		t.Fatalf("首次认证失败不应立即要求重登：confirmed=%t state=%+v", confirmed, state)
	}
	if confirmed := applyAppleKeepAliveFailureStatus(&state, true, appleKeepAliveAuthFailThreshold); !confirmed || state.LastCheckOK || !strings.Contains(state.LastStatusMessage, "需要重新登录") {
		t.Fatalf("连续认证失败达到阈值后状态错误：confirmed=%t state=%+v", confirmed, state)
	}
}

func TestRunAppleKeepAliveChecksImmediately(t *testing.T) {
	state, _ := newKeepAliveTestState(t, true, time.Now().Add(-time.Hour))
	cfg := config.Default()
	cfg.AppleAccountKeepAliveMS = 180000
	cfg.AppleAccountKeepAliveJitterPercent = 0
	server := New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	called := make(chan struct{}, 1)
	server.keepAliveState = func(_ context.Context, loginState domain.LoginState) (domain.LoginState, error) {
		called <- struct{}{}
		return loginState, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		server.runAppleKeepAlive(ctx)
		close(finished)
	}()

	select {
	case <-called:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("保活服务启动后没有立即扫描登录态")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("取消后保活服务没有及时停止")
	}
}

func TestMailWatcherSettingTakesEffectWithoutRestart(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	cfg := config.Default()
	server := New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	server.StartBackground(ctx)
	t.Cleanup(cancel)

	waitForWatcherStatus(t, server, func(status bool, enabled bool, groups int) bool {
		return status && !enabled && groups == 0
	})
	settings := state.Settings()
	settings.EnableMailWatcher = true
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatalf("开启测试邮件监听失败：%v", err)
	}
	server.watcher.Wake("")
	waitForWatcherStatus(t, server, func(status bool, enabled bool, groups int) bool {
		return status && enabled && groups == 0
	})

	cancel()
	waitForWatcherStatus(t, server, func(status bool, _ bool, _ int) bool { return !status })
}

func waitForWatcherStatus(t *testing.T, server *Server, match func(running bool, enabled bool, groups int) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := server.watcher.Snapshot()
		if match(status.Running, status.Enabled, status.GroupCount) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	status := server.watcher.Snapshot()
	t.Fatalf("等待邮件监听状态超时：%+v", status)
}

func assertKeepAliveEvent(t *testing.T, state *store.Store, text string) {
	t.Helper()
	for _, event := range state.Dashboard().Events {
		if strings.Contains(event.Message, text) {
			return
		}
	}
	t.Fatalf("运行记录中没有找到 %q", text)
}

func newKeepAliveTestState(t *testing.T, lastCheckOK bool, lastCheckedAt time.Time) (*store.Store, domain.ICloudSession) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	settings := state.Settings()
	settings.EnableAppleKeepAlive = true
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatalf("启用测试保活设置失败：%v", err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "keepalive@example.com",
		SavedAt: time.Now(),
		LoginStates: []domain.LoginState{{
			Kind:              domain.LoginStateAppleAccount,
			Scnt:              "saved-scnt",
			APIKey:            "saved-key",
			Cookies:           []domain.SessionCookie{{Name: "session", Value: "fixture", Domain: "apple.com", Path: "/"}},
			LastCheckedAt:     lastCheckedAt,
			LastCheckOK:       lastCheckOK,
			LastStatusMessage: "测试状态",
		}},
	})
	if err != nil {
		t.Fatalf("保存测试 Apple 登录态失败：%v", err)
	}
	return state, session
}

func mustOpenKeepAliveStore(t *testing.T) *store.Store {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	return state
}
