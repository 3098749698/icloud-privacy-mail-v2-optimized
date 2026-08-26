package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestApplyMailboxSyncIMAPNoopAdvancesMemoryWithoutWriting(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email: "noop-sync@icloud.com", IsActive: true,
	}, "测试")
	if err != nil {
		t.Fatal(err)
	}
	firstAt := time.Date(2026, 8, 18, 20, 0, 0, 0, time.UTC)
	if _, err := state.ApplyMailboxSync([]MailboxSyncUpdate{{
		MailboxID: mailbox.ID, LastSyncUID: "100", SyncedAt: firstAt, SkipIfUnchanged: true,
	}}); err != nil {
		t.Fatalf("首次保存游标失败：%v", err)
	}
	beforeBytes, err := os.ReadFile(state.Path())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := state.FindMailboxByID(mailbox.ID)

	secondAt := firstAt.Add(12 * time.Second)
	if created, err := state.ApplyMailboxSync([]MailboxSyncUpdate{{
		MailboxID: mailbox.ID, LastSyncUID: "100", SyncedAt: secondAt, SkipIfUnchanged: true,
	}}); err != nil || created != 0 {
		t.Fatalf("无变化 IMAP 轮次结果异常：created=%d err=%v", created, err)
	}
	afterBytes, err := os.ReadFile(state.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(beforeBytes) {
		t.Fatal("无变化 IMAP 轮次不应写入 state.json")
	}
	after, _ := state.FindMailboxByID(mailbox.ID)
	if !after.LastSyncAt.Equal(secondAt) {
		t.Fatalf("内存 LastSyncAt 未推进：before=%s after=%s want=%s", before.LastSyncAt, after.LastSyncAt, secondAt)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatal("无变化 IMAP 轮次不应更新 UpdatedAt")
	}

	thirdAt := secondAt.Add(12 * time.Second)
	if created, err := state.ApplyMailboxSync([]MailboxSyncUpdate{{
		MailboxID: mailbox.ID, LastSyncUID: "100", SyncedAt: thirdAt, SkipIfUnchanged: true,
		Messages: []domain.Message{{RemoteID: "imap:junk:101", Subject: "新邮件", Body: "654321"}},
	}}); err != nil || created != 1 {
		t.Fatalf("有新邮件 IMAP 轮次结果异常：created=%d err=%v", created, err)
	}
	finalBytes, err := os.ReadFile(state.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(finalBytes) == string(afterBytes) {
		t.Fatal("有新邮件时应持久化 state.json")
	}
}
