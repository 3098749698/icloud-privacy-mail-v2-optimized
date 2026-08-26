package store

import (
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestSaveICloudSessionDoesNotOverwriteNewerLoginState(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	base := time.Now().Add(-time.Hour).Round(time.Second)
	saved, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "merge@example.com",
		SavedAt: base,
		LoginStates: []domain.LoginState{
			{
				Kind:              domain.LoginStateAppleAccount,
				SavedAt:           base.Add(10 * time.Minute),
				LastCheckedAt:     base.Add(40 * time.Minute),
				LastCheckOK:       false,
				LastStatusMessage: "管理态已失效",
			},
			{
				Kind:            domain.LoginStateICloudIMAP,
				SavedAt:         base.Add(5 * time.Minute),
				IMAPAppPassword: "old-password",
			},
		},
	})
	if err != nil {
		t.Fatalf("保存初始状态失败：%v", err)
	}

	merged, err := state.SaveICloudSession(domain.ICloudSession{
		AccountID: saved.AccountID,
		AppleID:   saved.AppleID,
		SavedAt:   base.Add(45 * time.Minute),
		LoginStates: []domain.LoginState{
			{
				Kind:              domain.LoginStateAppleAccount,
				SavedAt:           base.Add(20 * time.Minute),
				LastCheckedAt:     base.Add(20 * time.Minute),
				LastCheckOK:       true,
				LastStatusMessage: "缓存中的旧正常状态",
			},
			{
				Kind:            domain.LoginStateICloudIMAP,
				SavedAt:         base.Add(45 * time.Minute),
				IMAPAppPassword: "new-password",
			},
		},
	})
	if err != nil {
		t.Fatalf("合并状态失败：%v", err)
	}

	appleState := findLoginState(t, merged, domain.LoginStateAppleAccount)
	if appleState.LastCheckOK || appleState.LastStatusMessage != "管理态已失效" {
		t.Fatalf("较新的 Apple 失效状态被旧缓存覆盖：%+v", appleState)
	}
	imapState := findLoginState(t, merged, domain.LoginStateICloudIMAP)
	if imapState.IMAPAppPassword != "new-password" {
		t.Fatalf("较新的 IMAP 状态没有保存：%+v", imapState)
	}
}

func findLoginState(t *testing.T, session domain.ICloudSession, kind string) domain.LoginState {
	t.Helper()
	for _, state := range session.LoginStates {
		if state.Kind == kind {
			return state
		}
	}
	t.Fatalf("未找到登录态 %s", kind)
	return domain.LoginState{}
}
