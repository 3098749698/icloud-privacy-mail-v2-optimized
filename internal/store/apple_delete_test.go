package store

import (
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestDeleteAppleAccountCascadesMailboxData(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "delete-fixture@icloud.com", DSID: "fixture", SavedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "delete-mailbox@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.UpsertMessage(mailbox.ID, "remote-1", "imap", "验证码", "sender@example.com", "123456", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := state.ClaimMailboxLease("fixture", "删除测试", "delete-request", "", time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}

	deleted, err := state.DeleteAppleAccount(session.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Mailboxes != 1 || deleted.Messages != 1 || deleted.ICloudSessions != 1 {
		t.Fatalf("级联删除计数异常：%+v", deleted)
	}
	snapshot := state.Snapshot()
	if len(snapshot.AppleAccounts) != 0 || len(snapshot.ICloudSessions) != 0 || len(snapshot.Mailboxes) != 0 || len(snapshot.Messages) != 0 || len(snapshot.MailboxLeases) != 0 {
		t.Fatalf("删除后仍有账号关联数据：%+v", snapshot)
	}
}
