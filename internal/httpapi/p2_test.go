package httpapi

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

func TestMailboxAPIURLUsesSyncWaitAndPreview(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	cfg := config.Default()
	cfg.PublicBaseURL = "https://mail.example.test/"
	server := New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request, err := http.NewRequest(http.MethodGet, "https://admin.example.test/exports", nil)
	if err != nil {
		t.Fatalf("创建测试请求失败：%v", err)
	}
	mailbox := domain.Mailbox{Email: "code+fixture@example.test", APIToken: "token/with+chars"}
	link := server.mailboxAPIURL(request, mailbox)
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("解析生成的取码链接失败：%v", err)
	}
	if got, want := parsed.Scheme+"://"+parsed.Host, "https://mail.example.test"; got != want {
		t.Fatalf("基础地址为 %q，期望 %q", got, want)
	}
	if got, want := parsed.Path, "/api/v1/mailboxes/code+fixture@example.test/code"; got != want {
		t.Fatalf("邮箱路径为 %q，期望 %q", got, want)
	}
	values := parsed.Query()
	if got := values.Get("key"); got != mailbox.APIToken {
		t.Fatalf("API Key 为 %q，期望 %q", got, mailbox.APIToken)
	}
	if got := values.Get("wait_ms"); got != "30000" {
		t.Fatalf("wait_ms 为 %q，期望 30000", got)
	}
	if got := values.Get("preview"); got != "1" {
		t.Fatalf("preview 为 %q，期望 1", got)
	}
}

func TestMailboxExportAcceptsValidDaysAndPersistsExpiry(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID:            "valid-days-owner@icloud.com",
		DSID:               "valid-days-dsid",
		PremiumMailBaseURL: "https://mail.example.test",
		Cookies:            []domain.SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-token"}},
	})
	if err != nil {
		t.Fatalf("创建可回收 Apple 登录态失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{
		Email: "valid-days@icloud.com", IsActive: true,
	}, "")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPost, "/api/runtime/export-mailbox-apis", strings.NewReader(fmt.Sprintf(`{"format":"txt","account_id":%q,"count":1,"valid_days":7}`, session.AccountID)))
	response := httptest.NewRecorder()
	before := time.Now()
	server.writeMailboxExport(response, request, true)
	after := time.Now()
	if response.Code != http.StatusOK {
		t.Fatalf("导出返回 %d：%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Export-Valid-Days"); got != "7" {
		t.Fatalf("有效天数响应头为 %q，期望 7", got)
	}
	stored, ok := state.FindMailboxByID(mailbox.ID)
	if !ok || stored.Status != domain.StatusUsed || stored.ExportedAt.IsZero() || stored.AutoDeleteAt.IsZero() {
		t.Fatalf("导出有效期未落库：%+v，存在=%t", stored, ok)
	}
	if stored.ExportedAt.Before(before) || stored.ExportedAt.After(after) {
		t.Fatalf("导出时间不在请求窗口内：%s", stored.ExportedAt)
	}
	if want := stored.ExportedAt.Add(7 * 24 * time.Hour); !stored.AutoDeleteAt.Equal(want) {
		t.Fatalf("到期时间为 %s，期望 %s", stored.AutoDeleteAt, want)
	}
}

func TestMailboxExportRejectsExpiryWithoutRemoteDeleteState(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "new-interface-only@icloud.com",
		LoginStates: []domain.LoginState{{
			Kind: domain.LoginStateAppleAccount, SessionID: "new-only-session",
			Cookies: []domain.SessionCookie{{Name: "caw-at", Value: "fixture"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "new-only@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPost, "/api/runtime/export-mailbox-emails", strings.NewReader(fmt.Sprintf(`{"format":"txt","account_id":%q,"count":1,"valid_days":7}`, session.AccountID)))
	response := httptest.NewRecorder()
	server.writeMailboxExport(response, request, false)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"auto_delete_unavailable"`) {
		t.Fatalf("缺少 API key 的新接口登录态不应承诺自动释放配额：status=%d body=%s", response.Code, response.Body.String())
	}
	stored, _ := state.FindMailboxByID(mailbox.ID)
	if stored.Status != domain.StatusAvailable || !stored.ExportedAt.IsZero() {
		t.Fatalf("拒绝限时导出后邮箱状态被消耗：%+v", stored)
	}
}

func TestMailboxExportAcceptsExpiryWithAppleAccountDeleteState(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "new-delete-interface@icloud.com",
		LoginStates: []domain.LoginState{{
			Kind: domain.LoginStateAppleAccount, APIKey: "fixture-api-key",
			Cookies: []domain.SessionCookie{{Name: "caw-at", Value: "fixture"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "new-delete@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPost, "/api/runtime/export-mailbox-emails", strings.NewReader(fmt.Sprintf(`{"format":"txt","account_id":%q,"count":1,"valid_days":7}`, session.AccountID)))
	response := httptest.NewRecorder()
	server.writeMailboxExport(response, request, false)
	if response.Code != http.StatusOK {
		t.Fatalf("新接口限时导出返回 %d：%s", response.Code, response.Body.String())
	}
	stored, _ := state.FindMailboxByID(mailbox.ID)
	if stored.Status != domain.StatusUsed || stored.ExportedAt.IsZero() || stored.AutoDeleteAt.IsZero() {
		t.Fatalf("新接口限时导出未保存有效期：%+v", stored)
	}
}

type failingDownloadWriter struct {
	header http.Header
	status int
}

func (w *failingDownloadWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingDownloadWriter) WriteHeader(status int) { w.status = status }
func (w *failingDownloadWriter) Write([]byte) (int, error) {
	return 0, errors.New("模拟客户端下载中断")
}

func TestMailboxExportWriteFailureReturnsClaimedRowsToAvailablePool(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{Email: "rollback-download@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPost, "/api/runtime/export-mailbox-emails", strings.NewReader(`{"format":"txt","account_id":"account_fixture","count":1,"valid_days":0}`))
	response := &failingDownloadWriter{}
	server.writeMailboxExport(response, request, false)
	stored, _ := state.FindMailboxByID(mailbox.ID)
	if stored.Status != domain.StatusAvailable || !stored.ExportedAt.IsZero() || !stored.AutoDeleteAt.IsZero() {
		t.Fatalf("下载失败后邮箱没有退回可用池：%+v", stored)
	}
}

func TestExpiredMailboxCodeAPIStopsServingDuringRemoteDeleteRetry(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	settings := state.Settings()
	settings.EnablePublicMailboxAPI = true
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email: "expired-code@icloud.com", IsActive: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ExportAvailableMailboxesWithValidity("account_fixture", 1, time.Now().Add(-48*time.Hour), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/mailboxes/expired-code@icloud.com/code?key="+url.QueryEscape(mailbox.APIToken), nil)
	request.SetPathValue("email", mailbox.Email)
	response := httptest.NewRecorder()
	server.handlePublicMailboxCode(response, request)
	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), `"code":"mailbox_expired"`) {
		t.Fatalf("过期邮箱仍可取码：status=%d body=%s", response.Code, response.Body.String())
	}
}
