package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestMarkMailboxQuotaReleasedPersistsRemoteMilestone(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email: "released@icloud.com", IsActive: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	releasedAt := time.Now().Round(time.Millisecond)
	marked, err := state.MarkMailboxQuotaReleased(mailbox.ID, releasedAt)
	if err != nil {
		t.Fatalf("保存配额释放状态失败：%v", err)
	}
	if !marked.QuotaReleasedAt.Equal(releasedAt) || marked.ICloudActive {
		t.Fatalf("配额释放状态不正确：%+v", marked)
	}
	reopened, err := Open(state.Path())
	if err != nil {
		t.Fatalf("重新打开状态文件失败：%v", err)
	}
	persisted, ok := reopened.FindMailboxByID(mailbox.ID)
	if !ok || !persisted.QuotaReleasedAt.Equal(releasedAt) || persisted.ICloudActive {
		t.Fatalf("重启后配额释放状态丢失：%+v，存在=%t", persisted, ok)
	}
}

func TestDeleteMailboxRollsBackMemoryWhenStateSaveFails(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email: "rollback-delete@icloud.com", IsActive: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.UpsertMessage(mailbox.ID, "remote-1", "icloud", "code", "sender@example.com", "123456", time.Now()); err != nil {
		t.Fatal(err)
	}
	originalPath := state.path
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	state.path = filepath.Join(blockedParent, "state.json")
	deleteErr := state.DeleteMailbox(mailbox.ID)
	state.path = originalPath
	if deleteErr == nil {
		t.Fatal("状态文件保存失败时删除邮箱却返回成功")
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); !ok {
		t.Fatal("状态文件保存失败后内存中的邮箱没有回滚")
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 1 {
		t.Fatalf("状态文件保存失败后内存中的邮件没有回滚：%d", len(messages))
	}
}

func TestSetMailboxStatusHandlesOptionalNote(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email: "fixture@icloud.com",
		Label: "x_1",
		Note:  "原备注",
	}, "原备注")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}

	newNote := "新备注"
	updated, err := state.SetMailboxStatus(mailbox.ID, nil, nil, domain.StatusUsed, &newNote)
	if err != nil {
		t.Fatalf("更新邮箱状态和备注失败：%v", err)
	}
	if updated.Note != "新备注" {
		t.Fatalf("邮箱备注未更新：%q", updated.Note)
	}

	updated, err = state.SetMailboxStatus(mailbox.ID, nil, nil, domain.StatusAvailable, nil)
	if err != nil {
		t.Fatalf("仅更新邮箱状态失败：%v", err)
	}
	if updated.Note != "新备注" {
		t.Fatalf("仅更新状态时备注发生变化：%q", updated.Note)
	}

	emptyNote := ""
	updated, err = state.SetMailboxStatus(mailbox.ID, nil, nil, "", &emptyNote)
	if err != nil {
		t.Fatalf("清空邮箱备注失败：%v", err)
	}
	if updated.Note != "" {
		t.Fatalf("邮箱备注未清空：%q", updated.Note)
	}
}

func TestValidateSessionRenewsSlidingExpiry(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	if _, err := state.SetupAdmin("admin", "test-hash"); err != nil {
		t.Fatalf("创建测试管理员失败：%v", err)
	}
	expiresAt := time.Now().Add(time.Hour)
	if err := state.SaveSession("session-hash", expiresAt); err != nil {
		t.Fatalf("保存测试会话失败：%v", err)
	}
	admin, renewedUntil, ok, err := state.ValidateSession("session-hash", time.Now().Add(30*24*time.Hour), 0)
	if err != nil || !ok || admin.Username != "admin" {
		t.Fatalf("续期会话校验异常：admin=%+v expires=%s ok=%t err=%v", admin, renewedUntil, ok, err)
	}
	if !renewedUntil.After(expiresAt) {
		t.Fatalf("会话没有顺延：旧=%s 新=%s", expiresAt, renewedUntil)
	}
	loaded := state.Snapshot()
	if len(loaded.Sessions) != 1 || !loaded.Sessions[0].ExpiresAt.Equal(renewedUntil) {
		t.Fatalf("续期没有写入状态：%+v", loaded.Sessions)
	}
}

func TestExportAvailableMailboxesMarksUsedAndSkipsPreviouslyExported(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 3; index++ {
		_, _, err := state.UpsertMailboxFromRemote("acc_1", domain.RemoteMailbox{
			Email: fmt.Sprintf("export-%d@icloud.com", index), IsActive: true,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
	}

	first, err := state.ExportAvailableMailboxes("acc_1", 2, time.Now())
	if err != nil || len(first) != 2 {
		t.Fatalf("首次导出异常：count=%d err=%v", len(first), err)
	}
	for _, mailbox := range first {
		stored, ok := state.FindMailboxByID(mailbox.ID)
		if !ok || stored.Status != domain.StatusUsed {
			t.Fatalf("导出邮箱未标记已使用：%+v", stored)
		}
	}
	second, err := state.ExportAvailableMailboxes("acc_1", 2, time.Now())
	if err != nil || len(second) != 1 || second[0].ID == first[0].ID || second[0].ID == first[1].ID {
		t.Fatalf("第二次导出应只返回剩余邮箱：first=%v second=%v err=%v", first, second, err)
	}
}

func TestRollbackMailboxExportOnlyReleasesMatchingTransaction(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 2; index++ {
		if _, _, err := state.UpsertMailboxFromRemote("acc_rollback", domain.RemoteMailbox{Email: fmt.Sprintf("rollback-%d@icloud.com", index), IsActive: true}, ""); err != nil {
			t.Fatal(err)
		}
	}
	exportedAt := time.Now().UTC().Truncate(time.Millisecond)
	exported, err := state.ExportAvailableMailboxesWithValidity("acc_rollback", 1, exportedAt, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rolledBack, err := state.RollbackMailboxExport(exported, exportedAt)
	if err != nil || rolledBack != 1 {
		t.Fatalf("导出回滚失败：count=%d err=%v", rolledBack, err)
	}
	stored, _ := state.FindMailboxByID(exported[0].ID)
	if stored.Status != domain.StatusAvailable || !stored.ExportedAt.IsZero() || !stored.AutoDeleteAt.IsZero() {
		t.Fatalf("邮箱未恢复到可用状态：%+v", stored)
	}
	if rolledBack, err := state.RollbackMailboxExport(exported, exportedAt.Add(time.Second)); err != nil || rolledBack != 0 {
		t.Fatalf("不同导出事务不应被回滚：count=%d err=%v", rolledBack, err)
	}
}

func TestExportAvailableMailboxesWithValidityPersistsExpiryAndClaimsOnlyWhenDue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("acc_expiry", domain.RemoteMailbox{
		Email: "expiring-export@icloud.com", IsActive: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	exportedAt := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	exported, err := state.ExportAvailableMailboxesWithValidity("acc_expiry", 1, exportedAt, 7*24*time.Hour)
	if err != nil || len(exported) != 1 {
		t.Fatalf("带有效期导出失败：items=%+v err=%v", exported, err)
	}
	wantExpiry := exportedAt.Add(7 * 24 * time.Hour)
	if !exported[0].ExportedAt.Equal(exportedAt) || !exported[0].AutoDeleteAt.Equal(wantExpiry) {
		t.Fatalf("导出生命周期不正确：%+v", exported[0])
	}
	if claimed, err := state.ClaimExpiredMailboxes(wantExpiry.Add(-time.Second), 1, time.Minute); err != nil || len(claimed) != 0 {
		t.Fatalf("未到期邮箱不应被领取：claimed=%+v err=%v", claimed, err)
	}
	claimed, err := state.ClaimExpiredMailboxes(wantExpiry, 1, 2*time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != mailbox.ID {
		t.Fatalf("到期邮箱领取失败：claimed=%+v err=%v", claimed, err)
	}
	if claimed[0].DeleteAttempts != 1 || !claimed[0].DeleteLastAttemptAt.Equal(wantExpiry) || !claimed[0].DeleteRetryAt.Equal(wantExpiry.Add(2*time.Minute)) {
		t.Fatalf("删除领取状态不正确：%+v", claimed[0])
	}

	// State-file fields are the recovery queue. Reopening the store must keep
	// the in-flight lease and make the record eligible after it expires.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("重启后打开状态失败：%v", err)
	}
	if claimed, err := reopened.ClaimExpiredMailboxes(wantExpiry.Add(time.Minute), 1, time.Minute); err != nil || len(claimed) != 0 {
		t.Fatalf("重启后不应抢占仍在处理的记录：claimed=%+v err=%v", claimed, err)
	}
	claimed, err = reopened.ClaimExpiredMailboxes(wantExpiry.Add(2*time.Minute), 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].DeleteAttempts != 2 {
		t.Fatalf("重启后到期任务未继续：claimed=%+v err=%v", claimed, err)
	}
}

func TestRecordMailboxAutoDeleteFailurePersistsRetryState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("acc_retry", domain.RemoteMailbox{
		Email: "retry-export@icloud.com", IsActive: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := state.ExportAvailableMailboxesWithValidity("acc_retry", 1, now.Add(-48*time.Hour), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	claimed, err := state.ClaimExpiredMailboxes(now, 1, 2*time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("领取到期邮箱失败：claimed=%+v err=%v", claimed, err)
	}
	retryAt := now.Add(5 * time.Minute)
	updated, err := state.RecordMailboxAutoDeleteFailure(mailbox.ID, now, retryAt, "Apple 临时不可用")
	if err != nil {
		t.Fatalf("记录删除失败状态失败：%v", err)
	}
	if updated.DeleteLastError != "Apple 临时不可用" || !updated.DeleteRetryAt.Equal(retryAt) || updated.DeleteAttempts != 1 {
		t.Fatalf("删除失败状态不正确：%+v", updated)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := reopened.FindMailboxByID(mailbox.ID)
	if !ok || stored.DeleteLastError != updated.DeleteLastError || !stored.DeleteRetryAt.Equal(retryAt) {
		t.Fatalf("重启后删除重试状态丢失：%+v，存在=%t", stored, ok)
	}
}

func TestSetMailboxSyncCursorsUpdatesBatchAndRejectsUnknownWithoutPartialChanges(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := state.UpsertMailboxFromRemote("acc_1", domain.RemoteMailbox{Email: "cursor-first@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := state.UpsertMailboxFromRemote("acc_1", domain.RemoteMailbox{Email: "cursor-second@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	syncedAt := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	updated, err := state.SetMailboxSyncCursors(map[string]string{first.ID: "101", second.ID: "202"}, syncedAt)
	if err != nil || updated != 2 {
		t.Fatalf("批量更新游标失败：updated=%d err=%v", updated, err)
	}
	storedFirst, _ := state.FindMailboxByID(first.ID)
	storedSecond, _ := state.FindMailboxByID(second.ID)
	if !storedFirst.LastSyncAt.Equal(syncedAt) || storedFirst.LastSyncUID != "101" ||
		!storedSecond.LastSyncAt.Equal(syncedAt) || storedSecond.LastSyncUID != "202" {
		t.Fatalf("批量游标未正确保存：first=%+v second=%+v", storedFirst, storedSecond)
	}

	beforeFirst, _ := state.FindMailboxByID(first.ID)
	beforeSecond, _ := state.FindMailboxByID(second.ID)
	if _, err := state.SetMailboxSyncCursors(map[string]string{first.ID: "303", "missing": "404"}, syncedAt.Add(time.Minute)); err == nil {
		t.Fatal("未知邮箱游标应返回错误")
	}
	afterFirst, _ := state.FindMailboxByID(first.ID)
	afterSecond, _ := state.FindMailboxByID(second.ID)
	if afterFirst != beforeFirst || afterSecond != beforeSecond {
		t.Fatalf("未知邮箱错误不应留下部分更新：before=%+v/%+v after=%+v/%+v", beforeFirst, beforeSecond, afterFirst, afterSecond)
	}
}

func TestApplyMailboxSyncDeduplicatesAndPersistsMessagesAndCursorsTogether(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := state.UpsertMailboxFromRemote("acc_1", domain.RemoteMailbox{Email: "apply-first@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := state.UpsertMailboxFromRemote("acc_1", domain.RemoteMailbox{Email: "apply-second@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.UpsertMessage(first.ID, "remote-1", "imap", "旧验证码", "sender", "111111", time.Now()); err != nil {
		t.Fatal(err)
	}
	syncedAt := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC)
	created, err := state.ApplyMailboxSync([]MailboxSyncUpdate{
		{MailboxID: first.ID, SyncedAt: syncedAt, LastSyncUID: "101", Messages: []domain.Message{
			{RemoteID: "remote-1", Subject: "重复邮件", Body: "222222"},
			{RemoteID: "remote-2", Subject: "新邮件", Body: "333333", ReceivedAt: syncedAt},
			{RemoteID: "remote-2", Subject: "重复新邮件", Body: "444444", ReceivedAt: syncedAt},
			{Subject: "无远端 ID 1", Body: "555555"},
			{Subject: "无远端 ID 2", Body: "666666"},
		}},
		{MailboxID: second.ID, SyncedAt: syncedAt, LastSyncUID: "202", Messages: []domain.Message{{RemoteID: "second-1", Subject: "第二个邮箱", Body: "777777"}}},
	})
	if err != nil || created != 4 {
		t.Fatalf("批量同步去重/新增数量不正确：created=%d err=%v", created, err)
	}
	if messages := state.MessagesForMailbox(first.ID); len(messages) != 4 {
		t.Fatalf("第一个邮箱应有 4 封缓存邮件，实际 %d", len(messages))
	}
	storedFirst, _ := state.FindMailboxByID(first.ID)
	storedSecond, _ := state.FindMailboxByID(second.ID)
	if storedFirst.ReceiveCount != 4 || storedFirst.LastSyncUID != "101" || !storedFirst.LastSyncAt.Equal(syncedAt) {
		t.Fatalf("第一个邮箱同步元数据不正确：%+v", storedFirst)
	}
	if storedSecond.ReceiveCount != 1 || storedSecond.LastSyncUID != "202" || !storedSecond.LastSyncAt.Equal(syncedAt) {
		t.Fatalf("第二个邮箱同步元数据不正确：%+v", storedSecond)
	}

	beforeMessages := len(state.Snapshot().Messages)
	beforeFirst, _ := state.FindMailboxByID(first.ID)
	if _, err := state.ApplyMailboxSync([]MailboxSyncUpdate{{MailboxID: first.ID, SyncedAt: syncedAt.Add(time.Minute), LastSyncUID: "303"}, {MailboxID: "missing", SyncedAt: syncedAt.Add(time.Minute), LastSyncUID: "404", Messages: []domain.Message{{Body: "不应写入"}}}}); err == nil {
		t.Fatal("未知邮箱批量同步应返回错误")
	}
	afterFirst, _ := state.FindMailboxByID(first.ID)
	if len(state.Snapshot().Messages) != beforeMessages || afterFirst != beforeFirst {
		t.Fatalf("未知邮箱错误留下了部分批量同步：before=%+v after=%+v", beforeFirst, afterFirst)
	}
}

func TestMailboxLeaseCommitMarksUsedOnlyAfterSuccess(t *testing.T) {
	state, mailbox := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	claimed, lease, created, err := state.ClaimMailboxLease("gpt-register-next", "注册任务 #1", "request-1", "等待注册", 30*time.Minute, now)
	if err != nil {
		t.Fatalf("领取邮箱租约失败：%v", err)
	}
	if !created || lease.State != domain.MailboxLeaseClaimed {
		t.Fatalf("新租约状态不正确：created=%t lease=%+v", created, lease)
	}
	if claimed.Status != domain.StatusReserved || claimed.ActiveLeaseID != lease.ID {
		t.Fatalf("领取后邮箱应为已预留：%+v", claimed)
	}
	if claimed.Status == domain.StatusUsed {
		t.Fatal("注册成功前邮箱不应标记为已使用")
	}

	committed, committedLease, idempotent, err := state.CommitMailboxLease(lease.ID, lease.Project, "注册成功", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("提交邮箱租约失败：%v", err)
	}
	if idempotent || committed.Status != domain.StatusUsed || committed.ActiveLeaseID != "" || committedLease.State != domain.MailboxLeaseCommitted {
		t.Fatalf("提交后的邮箱或租约状态不正确：mailbox=%+v lease=%+v idempotent=%t", committed, committedLease, idempotent)
	}
	if committed.Note != "注册成功" || committedLease.Note != "注册成功" {
		t.Fatalf("提交备注没有同步：mailbox=%q lease=%q", committed.Note, committedLease.Note)
	}

	_, repeated, idempotent, err := state.CommitMailboxLease(lease.ID, lease.Project, "重复请求不覆盖备注", now.Add(2*time.Minute))
	if err != nil || !idempotent || repeated.Note != "注册成功" {
		t.Fatalf("重复提交没有返回幂等结果：lease=%+v idempotent=%t err=%v", repeated, idempotent, err)
	}
	if _, _, _, err := state.ReleaseMailboxLease(lease.ID, lease.Project, "错误释放", now.Add(3*time.Minute)); !errors.Is(err, ErrLeaseCommitted) {
		t.Fatalf("已提交租约应拒绝释放，实际错误：%v", err)
	}

	stored, ok := state.FindMailboxByID(mailbox.ID)
	if !ok || stored.Status != domain.StatusUsed {
		t.Fatalf("持久化邮箱状态不正确：%+v，存在=%t", stored, ok)
	}
}

func TestMailboxLeaseReleaseAndRequestIDAreIdempotent(t *testing.T) {
	state, _ := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC)
	_, lease, created, err := state.ClaimMailboxLease("Fixture", "注册", "same-request", "初始备注", time.Hour, now)
	if err != nil || !created {
		t.Fatalf("首次领取失败：created=%t err=%v", created, err)
	}
	mailbox, repeatedLease, created, err := state.ClaimMailboxLease("fixture", "注册", "same-request", "不同备注", time.Hour, now.Add(time.Minute))
	if err != nil || created || repeatedLease.ID != lease.ID || mailbox.Status != domain.StatusReserved {
		t.Fatalf("重复领取没有返回原租约：mailbox=%+v lease=%+v created=%t err=%v", mailbox, repeatedLease, created, err)
	}
	if _, _, _, err := state.ClaimMailboxLease("fixture", "另一个任务", "same-request", "", time.Hour, now); !errors.Is(err, ErrLeaseRequestConflict) {
		t.Fatalf("复用 request_id 到不同任务应冲突，实际错误：%v", err)
	}

	released, releasedLease, idempotent, err := state.ReleaseMailboxLease(lease.ID, "fixture", "注册失败，释放邮箱", now.Add(2*time.Minute))
	if err != nil || idempotent || released.Status != domain.StatusAvailable || releasedLease.State != domain.MailboxLeaseReleased {
		t.Fatalf("释放结果不正确：mailbox=%+v lease=%+v idempotent=%t err=%v", released, releasedLease, idempotent, err)
	}
	_, repeatedLease, idempotent, err = state.ReleaseMailboxLease(lease.ID, "fixture", "重复释放", now.Add(3*time.Minute))
	if err != nil || !idempotent || repeatedLease.Note != "注册失败，释放邮箱" {
		t.Fatalf("重复释放没有返回幂等结果：lease=%+v idempotent=%t err=%v", repeatedLease, idempotent, err)
	}
}

func TestMailboxLeaseExpiryReturnsMailboxToPool(t *testing.T) {
	state, mailbox := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	_, lease, _, err := state.ClaimMailboxLease("fixture", "过期测试", "expiry-request", "等待处理", time.Minute, now)
	if err != nil {
		t.Fatalf("领取测试租约失败：%v", err)
	}
	count, err := state.ExpireMailboxLeases(now.Add(2 * time.Minute))
	if err != nil || count != 1 {
		t.Fatalf("回收过期租约失败：count=%d err=%v", count, err)
	}
	stored, ok := state.FindMailboxByID(mailbox.ID)
	if !ok || stored.Status != domain.StatusAvailable || stored.ActiveLeaseID != "" {
		t.Fatalf("过期后邮箱没有恢复可用：%+v，存在=%t", stored, ok)
	}
	expired, ok := state.FindMailboxLease(lease.ID)
	if !ok || expired.State != domain.MailboxLeaseExpired || expired.ExpiredAt.IsZero() {
		t.Fatalf("租约过期状态不正确：%+v，存在=%t", expired, ok)
	}
	_, _, idempotent, err := state.ReleaseMailboxLease(lease.ID, "fixture", "过期后释放", now.Add(3*time.Minute))
	if err != nil || !idempotent {
		t.Fatalf("过期租约释放应按已回收返回幂等成功：idempotent=%t err=%v", idempotent, err)
	}
}

func TestMailboxLeaseNoteCanBeUpdated(t *testing.T) {
	state, _ := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC)
	_, lease, _, err := state.ClaimMailboxLease("fixture", "备注测试", "note-request", "原备注", time.Hour, now)
	if err != nil {
		t.Fatalf("领取测试租约失败：%v", err)
	}
	mailbox, updated, err := state.SetMailboxLeaseNote(lease.ID, "fixture", "人工审核中", now.Add(time.Minute))
	if err != nil || mailbox.Note != "人工审核中" || updated.Note != "人工审核中" {
		t.Fatalf("租约备注更新失败：mailbox=%+v lease=%+v err=%v", mailbox, updated, err)
	}
	if _, _, err := state.SetMailboxLeaseNote(lease.ID, "another-project", "越权备注", now); !errors.Is(err, ErrLeaseProjectMismatch) {
		t.Fatalf("不同项目不应更新租约备注，实际错误：%v", err)
	}
}

func newLeaseTestStore(t *testing.T) (*Store, domain.Mailbox) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email:    "lease-fixture@icloud.com",
		Label:    "lease_1",
		IsActive: true,
	}, "测试邮箱")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	return state, mailbox
}
