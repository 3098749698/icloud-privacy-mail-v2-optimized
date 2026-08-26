package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestAppleCredentialIsEncryptedAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := Open(path)
	if err != nil {
		t.Fatalf("打开状态失败：%v", err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "secret@example.com",
		SavedAt: time.Now(),
		LoginStates: []domain.LoginState{{
			Kind: domain.LoginStateAppleAccount, Scnt: "scnt", Cookies: []domain.SessionCookie{{Name: "session", Value: "cookie"}},
		}},
	})
	if err != nil {
		t.Fatalf("保存账号失败：%v", err)
	}
	const password = "Sensitive Password 123!"
	if err := state.SaveAppleCredential(session.AccountID, session.AppleID, password, "trusted_device"); err != nil {
		t.Fatalf("保存凭据失败：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取状态文件失败：%v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatal("状态文件包含 Apple 明文密码")
	}
	keyInfo, err := os.Stat(path + ".apple-credentials.key")
	if err != nil {
		t.Fatalf("凭据密钥文件不存在：%v", err)
	}
	if runtime.GOOS != "windows" && keyInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("凭据密钥权限过宽：%o", keyInfo.Mode().Perm())
	}
	credential, ok, err := state.AppleCredential(session.AccountID)
	if err != nil || !ok || credential.Password != password || credential.TwoFactorMethod != "trusted_device" {
		t.Fatalf("凭据回读异常：ok=%t err=%v credential=%+v", ok, err, credential)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开状态失败：%v", err)
	}
	credential, ok, err = reopened.AppleCredential(session.AccountID)
	if err != nil || !ok || credential.Password != password {
		t.Fatalf("重启后凭据回读异常：ok=%t err=%v", ok, err)
	}
}

func TestDeleteAppleAccountDeletesCredential(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "delete@example.com", SavedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveAppleCredential(session.AccountID, session.AppleID, "password", "sms"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.DeleteAppleAccount(session.AccountID); err != nil {
		t.Fatal(err)
	}
	if state.HasAppleCredential(session.AccountID) {
		t.Fatal("删除账号后仍保留加密凭据")
	}
}

func TestOpenMigratesConfirmedAppleAuthFailureToCircuitBreaker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := `{
  "schema_version": 6,
  "next_id": 2,
  "apple_accounts": [{"id":"account_1","apple_id":"owner@example.com","created_at":"2026-08-24T00:00:00Z","updated_at":"2026-08-24T00:00:00Z"}],
  "icloud_sessions": [{"account_id":"account_1","apple_id":"owner@example.com","login_states":[{"kind":"apple_account","last_check_ok":false,"last_status_message":"Apple Account 连续 3 次认证失败，需要重新登录"}]}],
  "settings": {"mailbox_page_size":7,"message_retention_hours":24},
  "create_settings": {},
  "created_at":"2026-08-24T00:00:00Z","updated_at":"2026-08-24T00:00:00Z"
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	session, ok := state.ICloudSessionByAccountID("account_1")
	if !ok || len(session.LoginStates) != 1 || !session.LoginStates[0].ReloginRequired {
		t.Fatalf("旧确认掉线状态未迁移到熔断：%+v", session.LoginStates)
	}
}
