package mailbox

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

type remoteMailboxDeleteClientFixture struct {
	remote       protocol.ICloudRemoteMailbox
	operations   []string
	remoteIDs    []string
	listCalls    int
	listErr      error
	deleteErr    error
	deleteActive *bool
	moveErr      error
	onDelete     func()
}

func (f *remoteMailboxDeleteClientFixture) ListPrivacyMailboxes(context.Context, protocol.ICloudSession) ([]protocol.ICloudRemoteMailbox, error) {
	f.operations = append(f.operations, "查询远端邮箱")
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listCalls == 1 {
		return []protocol.ICloudRemoteMailbox{f.remote}, nil
	}
	return nil, nil
}

func (f *remoteMailboxDeleteClientFixture) DeletePrivacyMailbox(_ context.Context, _ protocol.ICloudSession, anonymousID string) error {
	f.operations = append(f.operations, "删除远端邮箱")
	if anonymousID != f.remote.AnonymousID {
		return errors.New("远端邮箱标识不匹配")
	}
	if f.onDelete != nil {
		f.onDelete()
	}
	return f.deleteErr
}

func (f *remoteMailboxDeleteClientFixture) ListPrivacyMailboxesWithAppleAccount(_ context.Context, session protocol.ICloudSession, _ string) ([]protocol.ICloudRemoteMailbox, protocol.ICloudSession, error) {
	f.operations = append(f.operations, "新接口查询远端邮箱")
	f.listCalls++
	if f.listErr != nil {
		return nil, session, f.listErr
	}
	if f.listCalls == 1 {
		return []protocol.ICloudRemoteMailbox{f.remote}, session, nil
	}
	return nil, session, nil
}

func (f *remoteMailboxDeleteClientFixture) DeletePrivacyMailboxWithAppleAccount(_ context.Context, session protocol.ICloudSession, _ string, anonymousID string, active bool) (protocol.ICloudSession, error) {
	f.operations = append(f.operations, "新接口删除远端邮箱")
	f.deleteActive = new(bool)
	*f.deleteActive = active
	if anonymousID != f.remote.AnonymousID {
		return session, errors.New("远端邮箱标识不匹配")
	}
	if f.onDelete != nil {
		f.onDelete()
	}
	if state, ok := protocol.LoginStateForKind(session, domain.LoginStateAppleAccount); ok {
		state.Cookies = append(state.Cookies, domain.SessionCookie{Name: "rotated", Value: "saved", Domain: "apple.com", Path: "/"})
		session = protocol.WithLoginState(session, state)
	}
	return session, f.deleteErr
}

func (f *remoteMailboxDeleteClientFixture) MoveRemoteMessagesToTrash(_ context.Context, _ protocol.ICloudSession, remoteIDs []string) (protocol.ICloudMailCleanupResult, error) {
	f.operations = append(f.operations, "移动远端邮件")
	f.remoteIDs = append([]string(nil), remoteIDs...)
	if f.moveErr != nil {
		return protocol.ICloudMailCleanupResult{}, f.moveErr
	}
	return protocol.ICloudMailCleanupResult{MovedToTrash: len(remoteIDs)}, nil
}

func (f *remoteMailboxDeleteClientFixture) EmptyTrash(context.Context, protocol.ICloudSession) (int, error) {
	f.operations = append(f.operations, "清空远端废纸篓")
	return 1, nil
}

func TestDeleteRemoteConfirmsAliasRemovalBeforeCleaningMessages(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{
		remote: protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
	}
	client.onDelete = func() {
		if _, ok := state.FindMailboxByID(mailbox.ID); !ok {
			t.Error("删除 Apple 远端邮箱时，本地邮箱记录应仍然存在")
		}
		if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 2 {
			t.Errorf("Apple 远端删除确认前本地邮件不应清空：%d 封", len(messages))
		}
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	if err := service.DeleteRemote(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("彻底删除邮箱失败：%v", err)
	}
	expected := []string{"查询远端邮箱", "删除远端邮箱", "查询远端邮箱", "移动远端邮件"}
	if !reflect.DeepEqual(client.operations, expected) {
		t.Fatalf("删除执行顺序不正确：得到 %v，期望 %v", client.operations, expected)
	}
	if !reflect.DeepEqual(client.remoteIDs, []string{"icloud:Inbox:101"}) {
		t.Fatalf("远端邮件标识不正确：%v", client.remoteIDs)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); ok {
		t.Fatal("完成邮件清理和 Apple 删除后，本地邮箱记录仍然存在")
	}
}

func TestDeleteRemoteUsesAppleAccountManagementAndPersistsRotatedState(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	session, _ := state.ICloudSessionByAccountID(mailbox.AccountID)
	session.DSID = ""
	session.PremiumMailBaseURL = ""
	session.Cookies = nil
	session.LoginStates = []domain.LoginState{{
		Kind:    domain.LoginStateAppleAccount,
		APIKey:  "saved-api-key",
		Cookies: []domain.SessionCookie{{Name: "caw-at", Value: "fixture", Domain: "apple.com", Path: "/"}},
	}}
	if _, err := state.SaveICloudSession(session); err != nil {
		t.Fatal(err)
	}
	client := &remoteMailboxDeleteClientFixture{
		remote: protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email, IsActive: false},
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	if err := service.DeleteRemote(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("新接口彻底删除邮箱失败：%v", err)
	}
	if !reflect.DeepEqual(client.operations, []string{"新接口查询远端邮箱", "新接口删除远端邮箱", "新接口查询远端邮箱", "移动远端邮件"}) {
		t.Fatalf("新接口删除步骤异常：%v", client.operations)
	}
	if client.deleteActive == nil || *client.deleteActive {
		t.Fatalf("停用邮箱不应重复调用 stop：%v", client.deleteActive)
	}
	updated, ok := state.ICloudSessionByAccountID(mailbox.AccountID)
	if !ok {
		t.Fatal("删除后登录态丢失")
	}
	loginState, ok := protocol.LoginStateForKind(updated, domain.LoginStateAppleAccount)
	if !ok || len(loginState.Cookies) != 2 {
		t.Fatalf("新接口轮换登录态未保存：%+v", loginState)
	}
}

func TestDeleteRemoteNotifiesCapacityReleaseAfterLocalDeletion(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{
		remote: protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client
	notified := ""
	service.SetCapacityReleasedHandler(func(accountID string) {
		notified = accountID
		if _, exists := state.FindMailboxByID(mailbox.ID); exists {
			t.Error("配额通知早于本地邮箱记录删除")
		}
	})

	if err := service.DeleteRemote(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("彻底删除邮箱失败：%v", err)
	}
	if notified != mailbox.AccountID {
		t.Fatalf("配额通知账号不正确：得到 %q，期望 %q", notified, mailbox.AccountID)
	}
}

func TestDeleteRemoteStillFinishesAfterAppleConfirmsAliasRemoval(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{
		remote:  protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
		moveErr: errors.New("远端邮件清理测试失败"),
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	if err := service.DeleteRemote(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("Apple 别名已确认删除后不应因邮件收尾失败重试：%v", err)
	}
	if !reflect.DeepEqual(client.operations, []string{"查询远端邮箱", "删除远端邮箱", "查询远端邮箱", "移动远端邮件"}) {
		t.Fatalf("删除步骤异常：%v", client.operations)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); ok {
		t.Fatal("Apple 别名已不存在，本地邮箱仍被保留并误占统计")
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 0 {
		t.Fatalf("删除邮箱记录后仍残留本地邮件：%d 封", len(messages))
	}
}

func TestDeleteRemoteSkipsMailAPIWhenNoRemoteMessagesExist(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	if _, err := state.DeleteMailboxMessages(mailbox.ID); err != nil {
		t.Fatal(err)
	}
	client := &remoteMailboxDeleteClientFixture{remote: protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email}, moveErr: errors.New("不应访问邮件接口")}
	service := NewService(config.Config{}, state)
	service.deleteClient = client
	if err := service.DeleteRemote(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("没有关联远端邮件时删除失败：%v", err)
	}
	expected := []string{"查询远端邮箱", "删除远端邮箱", "查询远端邮箱"}
	if !reflect.DeepEqual(client.operations, expected) {
		t.Fatalf("没有远端邮件仍访问了邮件清理接口：%v", client.operations)
	}
}

func TestDeleteExpiredMailboxesRemovesRemoteBeforeLocalRecord(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	now := time.Now()
	if _, err := state.ExportAvailableMailboxesWithValidity(mailbox.AccountID, 1, now.Add(-48*time.Hour), 24*time.Hour); err != nil {
		t.Fatalf("设置到期导出失败：%v", err)
	}
	client := &remoteMailboxDeleteClientFixture{
		remote: protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
	}
	client.onDelete = func() {
		if _, ok := state.FindMailboxByID(mailbox.ID); !ok {
			t.Error("Apple 远端删除确认前不应删除本地记录")
		}
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	result, err := service.DeleteExpiredMailboxes(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("回收到期邮箱失败：%v", err)
	}
	if result.Claimed != 1 || result.Deleted != 1 || result.Failed != 0 {
		t.Fatalf("回收统计不正确：%+v", result)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); ok {
		t.Fatal("Apple 删除并确认不存在后，本地记录仍然存在")
	}
	expected := []string{"查询远端邮箱", "删除远端邮箱", "查询远端邮箱", "移动远端邮件"}
	if !reflect.DeepEqual(client.operations, expected) {
		t.Fatalf("到期删除顺序不正确：得到 %v，期望 %v", client.operations, expected)
	}
}

func TestDeleteExpiredMailboxesTreatsConfirmedAliasRemovalAsSuccess(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	now := time.Now()
	if _, err := state.ExportAvailableMailboxesWithValidity(mailbox.AccountID, 1, now.Add(-48*time.Hour), 24*time.Hour); err != nil {
		t.Fatalf("设置到期导出失败：%v", err)
	}
	client := &remoteMailboxDeleteClientFixture{
		remote:  protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
		moveErr: errors.New("Apple 临时连接失败"),
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	result, err := service.DeleteExpiredMailboxes(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("失败重试状态不应中断整轮回收：%v", err)
	}
	if result.Claimed != 1 || result.Deleted != 1 || result.Failed != 0 || len(result.Failures) != 0 {
		t.Fatalf("Apple 别名已确认删除却未计为回收成功：%+v", result)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); ok {
		t.Fatal("Apple 别名已释放名额，本地记录仍然存在")
	}
}

func TestDeleteExpiredMailboxesKeepsRemoteFailuresSeparateFromCleanup(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	now := time.Now()
	if _, err := state.ExportAvailableMailboxesWithValidity(mailbox.AccountID, 1, now.Add(-48*time.Hour), 24*time.Hour); err != nil {
		t.Fatalf("设置到期导出失败：%v", err)
	}
	client := &remoteMailboxDeleteClientFixture{listErr: errors.New("Apple 列表暂时不可用")}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	result, err := service.DeleteExpiredMailboxes(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("远端失败应保存退避状态而不是中断整轮：%v", err)
	}
	if result.Claimed != 1 || result.Deleted != 0 || result.QuotaReleased != 0 || result.CleanupPending != 0 || result.Failed != 1 || len(result.Failures) != 1 || len(result.CleanupFailures) != 0 {
		t.Fatalf("远端失败统计不正确：%+v", result)
	}
	stored, ok := state.FindMailboxByID(mailbox.ID)
	if !ok || !stored.QuotaReleasedAt.IsZero() || !strings.Contains(stored.DeleteLastError, "Apple 列表暂时不可用") {
		t.Fatalf("远端失败状态不正确：%+v，存在=%t", stored, ok)
	}
	if !reflect.DeepEqual(client.operations, []string{"查询远端邮箱"}) {
		t.Fatalf("远端列表失败后仍执行了后续操作：%v", client.operations)
	}
}

func TestDeleteExpiredMailboxesRetriesOnlyLocalCleanupAfterQuotaRelease(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	now := time.Now()
	if _, err := state.ExportAvailableMailboxesWithValidity(mailbox.AccountID, 1, now.Add(-48*time.Hour), 24*time.Hour); err != nil {
		t.Fatalf("设置到期导出失败：%v", err)
	}
	client := &remoteMailboxDeleteClientFixture{
		remote: protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client
	service.deleteLocal = func(string) error { return errors.New("本地状态文件暂时不可写") }
	notifications := 0
	service.SetCapacityReleasedHandler(func(string) { notifications++ })

	result, err := service.DeleteExpiredMailboxes(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("本地清理失败不应中断整轮回收：%v", err)
	}
	if result.Claimed != 1 || result.Deleted != 0 || result.QuotaReleased != 1 || result.CleanupPending != 1 || result.Failed != 0 || len(result.CleanupFailures) != 1 {
		t.Fatalf("配额释放后的清理统计不正确：%+v", result)
	}
	if !reflect.DeepEqual(client.operations, []string{"查询远端邮箱", "删除远端邮箱", "查询远端邮箱", "移动远端邮件"}) {
		t.Fatalf("首次删除执行顺序不正确：%v", client.operations)
	}
	if notifications != 0 {
		t.Fatalf("本地记录尚未删除时不应通知补建：%d", notifications)
	}
	stored, ok := state.FindMailboxByID(mailbox.ID)
	if !ok || stored.QuotaReleasedAt.IsZero() || !stored.DeleteRetryAt.After(stored.DeleteLastAttemptAt) || !strings.Contains(stored.DeleteLastError, "本地状态文件暂时不可写") {
		t.Fatalf("配额释放和本地清理重试状态未保存：%+v，存在=%t", stored, ok)
	}
	reopened, err := store.Open(state.Path())
	if err != nil {
		t.Fatalf("重新打开状态文件失败：%v", err)
	}
	persisted, ok := reopened.FindMailboxByID(mailbox.ID)
	if !ok || persisted.QuotaReleasedAt.IsZero() || persisted.DeleteRetryAt.IsZero() {
		t.Fatalf("重启后配额释放状态丢失：%+v，存在=%t", persisted, ok)
	}

	client.operations = nil
	service.deleteLocal = state.DeleteMailbox
	retryResult, err := service.DeleteExpiredMailboxes(context.Background(), stored.DeleteRetryAt.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("重试本地清理失败：%v", err)
	}
	if retryResult.Claimed != 1 || retryResult.Deleted != 1 || retryResult.QuotaReleased != 0 || retryResult.CleanupPending != 0 || retryResult.Failed != 0 {
		t.Fatalf("本地清理重试统计不正确：%+v", retryResult)
	}
	if !reflect.DeepEqual(client.operations, []string{"移动远端邮件"}) {
		t.Fatalf("配额已释放后仍调用 Apple 列表或删除：%v", client.operations)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); ok {
		t.Fatal("本地清理重试成功后邮箱记录仍然存在")
	}
	if notifications != 1 {
		t.Fatalf("本地清理完成后应通知一次补建：%d", notifications)
	}
}

func newDeleteServiceFixture(t *testing.T) (*store.Store, domain.Mailbox) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID:            "delete-fixture@icloud.com",
		DSID:               "fixture-dsid",
		PremiumMailBaseURL: "https://mail.example.test",
		Cookies:            []domain.SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-token"}},
	})
	if err != nil {
		t.Fatalf("创建测试 Apple 登录态失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{
		AnonymousID: "anonymous-delete-fixture",
		Email:       "delete-fixture@icloud.com",
		Label:       "delete_fixture",
		IsActive:    true,
	}, "删除流程测试")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	if _, _, err := state.UpsertMessage(mailbox.ID, "icloud:Inbox:101", "icloud", "验证码 101", "sender@example.com", "101", time.Now()); err != nil {
		t.Fatalf("保存远端测试邮件失败：%v", err)
	}
	if _, _, err := state.UpsertMessage(mailbox.ID, "", "local", "本地缓存", "sender@example.com", "local", time.Now()); err != nil {
		t.Fatalf("保存本地测试邮件失败：%v", err)
	}
	return state, mailbox
}
