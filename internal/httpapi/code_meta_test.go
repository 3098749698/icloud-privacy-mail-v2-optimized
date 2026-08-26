package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

func TestCodeDiagnosticsIncludesStableFields(t *testing.T) {
	lastSyncAt := time.Date(2026, 8, 18, 21, 0, 0, 0, time.UTC)
	diagnostics := codeDiagnostics{
		startedAt:     time.Now().Add(-20 * time.Millisecond),
		syncStartedAt: time.Now().Add(-10 * time.Millisecond),
		syncElapsed:   8 * time.Millisecond,
	}
	meta := diagnostics.meta("sync", lastSyncAt)
	if meta["source"] != "sync" {
		t.Fatalf("source = %#v", meta["source"])
	}
	if elapsed, ok := meta["elapsed_ms"].(int64); !ok || elapsed < 0 {
		t.Fatalf("elapsed_ms = %#v", meta["elapsed_ms"])
	}
	if elapsed, ok := meta["sync_elapsed_ms"].(int64); !ok || elapsed != 8 {
		t.Fatalf("sync_elapsed_ms = %#v", meta["sync_elapsed_ms"])
	}
	if got, ok := meta["last_sync_at"].(time.Time); !ok || !got.Equal(lastSyncAt) {
		t.Fatalf("last_sync_at = %#v", meta["last_sync_at"])
	}

	zeroMeta := newCodeDiagnostics().meta("cache", time.Time{})
	if _, exists := zeroMeta["last_sync_at"]; !exists || zeroMeta["last_sync_at"] != nil {
		t.Fatalf("零值 last_sync_at 应稳定返回 null：%#v", zeroMeta["last_sync_at"])
	}
}

func TestPublicCodeCacheHitReturnsDiagnostics(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	if _, _, err := state.UpsertMessage(mailbox.ID, "imap:101", "imap", "OpenAI verification code 123456", "noreply@openai.com", "Your code is 123456", time.Now()); err != nil {
		t.Fatalf("保存缓存邮件失败：%v", err)
	}

	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&preview=1"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeCodeResponse(t, recorder)
	if success, _ := response["success"].(bool); !success {
		t.Fatalf("缓存命中未成功：%v", response)
	}
	data, ok := response["data"].(map[string]any)
	if !ok || data["folder"] != "inbox" {
		t.Fatalf("收件箱来源诊断错误：%v", response)
	}
	assertCodeMeta(t, response, "cache", true)
}

func TestPublicCodeJunkCacheHasSameSuccessShape(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	if _, _, err := state.UpsertMessage(mailbox.ID, "imap:junk:202", "imap", "OpenAI verification code 778899", "noreply@openai.com", "Your code is 778899", time.Now()); err != nil {
		t.Fatalf("保存垃圾箱缓存邮件失败：%v", err)
	}

	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&preview=1"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	response := decodeCodeResponse(t, recorder)
	data, ok := response["data"].(map[string]any)
	if recorder.Code != http.StatusOK || !ok || data["code"] != "778899" || data["folder"] != "junk" {
		t.Fatalf("垃圾箱验证码响应与收件箱不一致：status=%d body=%v", recorder.Code, response)
	}
	if data["email"] != mailbox.Email || data["subject"] == nil || data["from"] == nil || data["received_at"] == nil || data["detected_at"] == nil || data["delivery_delay_ms"] == nil || data["message_id"] == nil {
		t.Fatalf("垃圾箱验证码响应字段不完整：%v", data)
	}
	assertCodeMeta(t, response, "cache", true)
}

func TestPublicCodeICloudWebJunkCacheReportsJunkFolder(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	if _, _, err := state.UpsertMessage(mailbox.ID, "icloud:Junk:203", "icloud", "OpenAI verification code 887766", "noreply@openai.com", "Your code is 887766", time.Now()); err != nil {
		t.Fatalf("保存 iCloud Web 垃圾箱邮件失败：%v", err)
	}
	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&preview=1"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	response := decodeCodeResponse(t, recorder)
	data, ok := response["data"].(map[string]any)
	if recorder.Code != http.StatusOK || !ok || data["code"] != "887766" || data["folder"] != "junk" {
		t.Fatalf("iCloud Web 垃圾箱来源错误：status=%d body=%v", recorder.Code, response)
	}
}

func TestPublicCodeCacheOnlyDoesNotReturnCodeOlderThanFiveMinutes(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	if _, _, err := state.UpsertMessage(mailbox.ID, "imap:stale-cache-only", "imap", "OpenAI verification code 112233", "noreply@openai.com", "Your code is 112233", time.Now().Add(-6*time.Minute)); err != nil {
		t.Fatalf("保存旧缓存邮件失败：%v", err)
	}

	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&cache=1&preview=1&allow_stale=1"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeCodeResponse(t, recorder)
	if success, _ := response["success"].(bool); success {
		t.Fatalf("cache-only 查询不应返回五分钟前的旧验证码：%v", response)
	}
	if response["code"] != "no_code" {
		t.Fatalf("错误码=%#v body=%v", response["code"], response)
	}
	if response["retryable"] != true || response["retry_after_ms"] != float64(2000) {
		t.Fatalf("no_code 缺少轮询提示：%v", response)
	}
	assertCodeMeta(t, response, "cache", true)
}

func TestPublicCodeSyncFailureExplicitlyFallsBackToStaleCache(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	if _, _, err := state.UpsertMessage(mailbox.ID, "imap:junk:stale-fallback", "imap", "OpenAI verification code 223344", "noreply@openai.com", "Your code is 223344", time.Now().Add(-6*time.Minute)); err != nil {
		t.Fatalf("保存旧缓存邮件失败：%v", err)
	}

	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&wait_ms=200&preview=1&allow_stale=1"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeCodeResponse(t, recorder)
	if success, _ := response["success"].(bool); !success {
		t.Fatalf("同步失败后未使用显式旧缓存回退：%v", response)
	}
	data, ok := response["data"].(map[string]any)
	if !ok || data["code"] != "223344" || data["stale_cache"] != true || data["folder"] != "junk" {
		t.Fatalf("旧缓存回退数据不完整：%v", response)
	}
	assertCodeMeta(t, response, "stale-cache", true)
}

func TestAdminCodeCacheHitReturnsDiagnostics(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	if _, _, err := state.UpsertMessage(mailbox.ID, "imap:102", "imap", "OpenAI verification code 654321", "noreply@openai.com", "Your code is 654321", time.Now()); err != nil {
		t.Fatalf("保存缓存邮件失败：%v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/mailboxes/"+mailbox.ID+"/code?preview=1", nil)
	request.SetPathValue("id", mailbox.ID)
	server.handleMailboxCode(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeCodeResponse(t, recorder)
	if success, _ := response["success"].(bool); !success {
		t.Fatalf("管理端缓存命中未成功：%v", response)
	}
	assertCodeMeta(t, response, "cache", true)
}

func TestAdminCodeSyncFailureExplicitlyFallsBackToStaleCache(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	if _, _, err := state.UpsertMessage(mailbox.ID, "imap:junk:admin-stale-fallback", "imap", "OpenAI verification code 334455", "noreply@openai.com", "Your code is 334455", time.Now().Add(-6*time.Minute)); err != nil {
		t.Fatalf("保存旧缓存邮件失败：%v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/mailboxes/"+mailbox.ID+"/code?preview=1&allow_stale=1", nil)
	request.SetPathValue("id", mailbox.ID)
	server.handleMailboxCode(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeCodeResponse(t, recorder)
	if success, _ := response["success"].(bool); !success {
		t.Fatalf("管理端同步失败后未使用显式旧缓存回退：%v", response)
	}
	data, ok := response["data"].(map[string]any)
	if !ok || data["code"] != "334455" || data["stale_cache"] != true || data["folder"] != "junk" {
		t.Fatalf("管理端旧缓存回退数据不完整：%v", response)
	}
	assertCodeMeta(t, response, "stale-cache", true)
}

func TestPublicCodeSyncFailureReturnsDiagnosticsWithoutProviderDetails(t *testing.T) {
	server, _, mailbox := newCodeDiagnosticsFixture(t)
	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&wait_ms=200"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("状态码=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeCodeResponse(t, recorder)
	if response["code"] != "mail_sync_failed" {
		t.Fatalf("错误码=%#v body=%v", response["code"], response)
	}
	assertCodeMeta(t, response, "sync", false)
	message, _ := response["message"].(string)
	if message != "同步验证码邮件失败，请检查 IMAP 或 iCloud 登录态后重试" {
		t.Fatalf("错误信息不稳定或泄露服务商细节：%q", message)
	}
}

func TestPublicAllowStaleWithoutCachedCodePreservesSyncFailure(t *testing.T) {
	server, _, mailbox := newCodeDiagnosticsFixture(t)
	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&wait_ms=50&preview=1&allow_stale=1"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("没有旧缓存时同步错误被误报为 no_code：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeCodeResponse(t, recorder)
	if response["code"] != "mail_sync_failed" {
		t.Fatalf("错误码=%#v body=%v", response["code"], response)
	}
	if response["retryable"] != true || response["retry_after_ms"] != float64(2000) {
		t.Fatalf("同步失败缺少轮询提示：%v", response)
	}
}

func TestPublicCodeKeepsPollingCacheAfterDirectSyncFailure(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	inserted := make(chan error, 1)
	time.AfterFunc(50*time.Millisecond, func() {
		_, _, err := state.UpsertMessage(mailbox.ID, "imap:junk:303", "imap", "OpenAI verification code 445566", "noreply@openai.com", "Your code is 445566", time.Now())
		inserted <- err
	})

	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&wait_ms=500&preview=1"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if err := <-inserted; err != nil {
		t.Fatalf("模拟 IDLE 写入缓存失败：%v", err)
	}
	response := decodeCodeResponse(t, recorder)
	data, ok := response["data"].(map[string]any)
	if recorder.Code != http.StatusOK || !ok || data["code"] != "445566" || data["folder"] != "junk" {
		t.Fatalf("同步失败后没有继续等待缓存：status=%d body=%v", recorder.Code, response)
	}
}

func TestPublicCodeWaitUsesNotificationWithoutRepeatedSync(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	changes := make(chan struct{})
	server.codeChanges = func(string) <-chan struct{} { return changes }
	var calls atomic.Int32
	server.syncMailboxFresh = func(_ context.Context, mailboxID string) (int, error) {
		calls.Add(1)
		return 0, nil
	}
	time.AfterFunc(50*time.Millisecond, func() {
		_, _, _ = state.UpsertMessage(mailbox.ID, "imap:junk:late-404", "imap", "OpenAI verification code 556677", "noreply@openai.com", "Your code is 556677", time.Now())
		close(changes)
	})

	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&wait_ms=500&preview=1"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	response := decodeCodeResponse(t, recorder)
	data, ok := response["data"].(map[string]any)
	if recorder.Code != http.StatusOK || !ok || data["code"] != "556677" || data["folder"] != "junk" {
		t.Fatalf("通知唤醒未返回晚到验证码：status=%d calls=%d body=%v", recorder.Code, calls.Load(), response)
	}
	if calls.Load() != 1 {
		t.Fatalf("等待期间触发了 %d 轮同步，期望共享兜底仅一轮", calls.Load())
	}
}

func TestTenIdleReadyWaitersUseNotificationWithoutIMAPScans(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	server.mailboxIdleReady = func(string) bool { return true }
	changes := make(chan struct{})
	server.codeChanges = func(string) <-chan struct{} { return changes }
	var scans atomic.Int32
	server.syncMailboxFresh = func(context.Context, string) (int, error) {
		scans.Add(1)
		return 0, nil
	}

	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&wait_ms=1000&preview=1"
	const waiters = 10
	responses := make(chan *httptest.ResponseRecorder, waiters)
	var wg sync.WaitGroup
	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
			responses <- recorder
		}()
	}
	time.Sleep(40 * time.Millisecond)
	providerAt := time.Now().Add(-2 * time.Second)
	_, err := state.ApplyMailboxSync([]store.MailboxSyncUpdate{{
		MailboxID: mailbox.ID,
		Messages: []domain.Message{{
			RemoteID: "imap:junk:notify-10", Source: "imap", Subject: "OpenAI verification code 667788",
			From: "noreply@openai.com", Body: "Your code is 667788", ReceivedAt: providerAt.Add(-time.Second), ProviderReceivedAt: &providerAt,
		}},
		SyncedAt: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	close(changes)
	wg.Wait()
	close(responses)
	if scans.Load() != 0 {
		t.Fatalf("IDLE ready waiters triggered %d IMAP scans", scans.Load())
	}
	for recorder := range responses {
		response := decodeCodeResponse(t, recorder)
		data, ok := response["data"].(map[string]any)
		if recorder.Code != http.StatusOK || !ok || data["code"] != "667788" {
			t.Fatalf("notification response status=%d body=%v", recorder.Code, response)
		}
		if data["provider_received_at"] == nil || data["api_returned_at"] == nil {
			t.Fatalf("timeline fields missing: %v", data)
		}
		meta, _ := response["meta"].(map[string]any)
		if meta["idle_ready"] != true || meta["fetch_mode"] != "idle-targeted" {
			t.Fatalf("idle metadata mismatch: %v", meta)
		}
	}
}

func TestTenIdleUnreadyWaitersShareOneFallbackScan(t *testing.T) {
	server, state, mailbox := newCodeDiagnosticsFixture(t)
	server.mailboxIdleReady = func(string) bool { return false }
	changes := make(chan struct{})
	server.codeChanges = func(string) <-chan struct{} { return changes }
	var scans atomic.Int32
	scanStarted := make(chan struct{})
	releaseScan := make(chan struct{})
	server.syncMailboxFresh = func(context.Context, string) (int, error) {
		if scans.Add(1) == 1 {
			close(scanStarted)
		}
		<-releaseScan
		return 0, nil
	}

	requestURL := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?key=" + url.QueryEscape(mailbox.APIToken) + "&wait_ms=1000&preview=1"
	const waiters = 10
	responses := make(chan *httptest.ResponseRecorder, waiters)
	var wg sync.WaitGroup
	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
			responses <- recorder
		}()
	}
	<-scanStarted
	time.Sleep(40 * time.Millisecond)
	providerAt := time.Now().Add(-time.Second)
	_, err := state.ApplyMailboxSync([]store.MailboxSyncUpdate{{
		MailboxID: mailbox.ID,
		Messages: []domain.Message{{
			RemoteID: "imap:inbox:shared-fallback", Source: "imap", Subject: "OpenAI verification code 778899",
			From: "noreply@openai.com", Body: "Your code is 778899", ReceivedAt: providerAt, ProviderReceivedAt: &providerAt,
		}},
		SyncedAt: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	close(changes)
	close(releaseScan)
	wg.Wait()
	close(responses)
	if scans.Load() != 1 {
		t.Fatalf("10 waiters started %d fallback scans, want 1", scans.Load())
	}
	for recorder := range responses {
		response := decodeCodeResponse(t, recorder)
		data, ok := response["data"].(map[string]any)
		if recorder.Code != http.StatusOK || !ok || data["code"] != "778899" {
			t.Fatalf("shared fallback response status=%d body=%v", recorder.Code, response)
		}
	}
}

func newCodeDiagnosticsFixture(t *testing.T) (*Server, *store.Store, domain.Mailbox) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	settings := state.Settings()
	settings.EnablePublicMailboxAPI = true
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatalf("开启公共 API 失败：%v", err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "code-meta@example.com"})
	if err != nil {
		t.Fatalf("保存测试登录态失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{
		Email: "code-meta@icloud.com", IsActive: true,
	}, "诊断元数据测试")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	cfg := config.Default()
	server := New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return server, state, mailbox
}

func decodeCodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败：%v body=%s", err, recorder.Body.String())
	}
	return response
}

func assertCodeMeta(t *testing.T, response map[string]any, source string, wantLastSync bool) {
	t.Helper()
	meta, ok := response["meta"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 meta：%v", response)
	}
	if meta["source"] != source {
		t.Fatalf("source=%#v want=%q meta=%v", meta["source"], source, meta)
	}
	if _, ok := meta["elapsed_ms"].(float64); !ok {
		t.Fatalf("elapsed_ms 类型错误：%#v", meta["elapsed_ms"])
	}
	if _, ok := meta["sync_elapsed_ms"].(float64); !ok {
		t.Fatalf("sync_elapsed_ms 类型错误：%#v", meta["sync_elapsed_ms"])
	}
	if _, exists := meta["last_sync_at"]; !exists {
		t.Fatalf("缺少 last_sync_at：%v", meta)
	}
	if wantLastSync && meta["last_sync_at"] == nil {
		t.Fatalf("缓存命中应返回 last_sync_at：%v", meta)
	}
}
