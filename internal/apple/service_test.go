package apple

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

func TestSupportsMailboxAutoDeleteWithAppleAccountState(t *testing.T) {
	service := &Service{cfg: config.Config{AppleAccountAPIKey: "configured-key"}}
	session := domain.ICloudSession{LoginStates: []domain.LoginState{{
		Kind:    domain.LoginStateAppleAccount,
		Cookies: []domain.SessionCookie{{Name: "caw-at", Value: "fixture"}},
	}}}
	if !service.supportsMailboxAutoDelete(session) {
		t.Fatal("Apple Account 管理态和配置 API key 应支持限时导出")
	}
}

func TestAccountSummaryDoesNotExposeCredentialOrTokens(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "private@example.com", SavedAt: time.Now(),
		LoginStates: []domain.LoginState{{
			Kind: domain.LoginStateAppleAccount, Scnt: "public-scnt", SessionToken: "secret-session-token", TrustToken: "secret-trust-token",
			Cookies: []domain.SessionCookie{{Name: "session", Value: "secret-cookie"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveAppleCredential(session.AccountID, session.AppleID, "secret-password", "trusted_device"); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.Default(), state)
	data, err := json.Marshal(service.Accounts())
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(data)
	for _, secret := range []string{"secret-password", "secret-session-token", "secret-trust-token", "secret-cookie", "public-scnt", "password_ciphertext"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("账号摘要泄露敏感字段 %q：%s", secret, serialized)
		}
	}
	if !strings.Contains(serialized, `"relogin_ready":true`) {
		t.Fatalf("账号摘要未标记可重新登录：%s", serialized)
	}
}

func TestNewIMAPLoginStateProviders(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		email    string
		host     string
	}{
		{name: "iCloud default", email: "owner@icloud.com", host: "imap.mail.me.com"},
		{name: "QQ", provider: "qq", email: "owner@qq.com", host: "imap.qq.com"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, err := newIMAPLoginState(IMAPLoginRequest{
				Provider:    test.provider,
				Email:       test.email,
				AppPassword: "authorization-code",
			})
			if err != nil {
				t.Fatalf("newIMAPLoginState() error = %v", err)
			}
			if state.IMAPHost != test.host || state.IMAPPort != 993 {
				t.Fatalf("IMAP endpoint = %s:%d, want %s:993", state.IMAPHost, state.IMAPPort, test.host)
			}
			if state.IMAPEmail != test.email || state.IMAPUsername != test.email {
				t.Fatalf("IMAP identity = %q/%q, want %q", state.IMAPEmail, state.IMAPUsername, test.email)
			}
		})
	}
}

func TestNewIMAPLoginStateRejectsOutlookPassword(t *testing.T) {
	_, err := newIMAPLoginState(IMAPLoginRequest{
		Provider:    "outlook",
		Email:       "owner@outlook.com",
		AppPassword: "legacy-password",
	})
	if err == nil {
		t.Fatal("newIMAPLoginState() accepted Outlook password authentication")
	}
}

func TestNewIMAPLoginStateRejectsUnknownProvider(t *testing.T) {
	_, err := newIMAPLoginState(IMAPLoginRequest{
		Provider:    "unknown",
		Email:       "owner@example.com",
		AppPassword: "authorization-code",
	})
	if err == nil {
		t.Fatal("newIMAPLoginState() accepted an unknown provider")
	}
}
