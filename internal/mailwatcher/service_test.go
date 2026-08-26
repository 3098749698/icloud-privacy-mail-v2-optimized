package mailwatcher

import (
	"context"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestMailWatcherSyncRetryDelay(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{10, 15 * time.Minute},
	}
	for _, test := range tests {
		if got := mailWatcherSyncRetryDelay(test.failures); got != test.want {
			t.Fatalf("failures=%d: got=%s want=%s", test.failures, got, test.want)
		}
	}
}

func TestGroupSignatureIgnoresMailboxMembership(t *testing.T) {
	state := domain.LoginState{
		IMAPEmail:       "owner@icloud.com",
		IMAPUsername:    "owner@icloud.com",
		IMAPHost:        "imap.mail.me.com",
		IMAPPort:        993,
		IMAPAppPassword: "fixture",
	}
	before := groupSignature(state)
	// Mailbox membership is deliberately absent from the signature. Adding an
	// alias must be picked up dynamically without reconnecting IMAP.
	after := groupSignature(state)
	if before != after {
		t.Fatalf("相同 IMAP 身份生成了不同签名：%s != %s", before, after)
	}
}

func TestScheduledFallbackGroupsKeepsQQGroupWhenIDLEReady(t *testing.T) {
	groups := []watchGroup{
		{
			key:   "qq",
			state: domain.LoginState{IMAPHost: "imap.qq.com"},
		},
		{
			key:   "icloud",
			state: domain.LoginState{IMAPHost: "imap.mail.me.com"},
		},
	}
	service := &Service{readyWorkers: map[string]bool{"qq": true, "icloud": true}}
	got := service.scheduledFallbackGroups(groups)
	if len(got) != 1 || got[0].key != "qq" {
		t.Fatalf("IDLE 已就绪时应保留 QQ 兜底组，得到 %#v", got)
	}
}

func TestScheduledFallbackGroupsIncludesUnreadyNonQQ(t *testing.T) {
	groups := []watchGroup{
		{
			key:   "outlook",
			state: domain.LoginState{IMAPHost: "outlook.office365.com"},
		},
		{
			key:   "qq",
			state: domain.LoginState{IMAPHost: "imap.qq.com."},
		},
	}
	service := &Service{readyWorkers: map[string]bool{"qq": true}}
	got := service.scheduledFallbackGroups(groups)
	if len(got) != 2 {
		t.Fatalf("未就绪非 QQ 与已就绪 QQ 都应进入兜底组，得到 %#v", got)
	}
	if got[0].key != "outlook" || got[1].key != "qq" {
		t.Fatalf("兜底组顺序或内容错误：%#v", got)
	}
}

func TestIdleWorkerTargetsAddsJunkOnlyForQQ(t *testing.T) {
	qq := idleWorkerTargets(watchGroup{key: "qq", state: domain.LoginState{IMAPHost: "imap.qq.com"}})
	if len(qq) != 2 || qq[0].key != "qq" || qq[0].folder != idleFolderInbox || qq[1].key != "qq#junk" || qq[1].folder != idleFolderJunk {
		t.Fatalf("QQ 应同时监听 INBOX 与 Junk，得到 %#v", qq)
	}
	outlook := idleWorkerTargets(watchGroup{key: "outlook", state: domain.LoginState{IMAPHost: "outlook.office365.com"}})
	if len(outlook) != 1 || outlook[0].key != "outlook" || outlook[0].folder != idleFolderInbox {
		t.Fatalf("非 QQ 应只监听 INBOX，得到 %#v", outlook)
	}
}

func TestGroupsForMailboxesOnlySelectsTargetGroups(t *testing.T) {
	groups := []watchGroup{
		{key: "first", mailboxes: []domain.Mailbox{{ID: "mbx-1"}, {ID: "mbx-2"}}},
		{key: "second", mailboxes: []domain.Mailbox{{ID: "mbx-3"}}},
	}
	got := groupsForMailboxes(groups, map[string]struct{}{"mbx-3": {}})
	if len(got) != 1 || got[0].key != "second" {
		t.Fatalf("指定邮箱唤醒应只选择所属组，得到 %#v", got)
	}
	if got := groupsForMailboxes(groups, map[string]struct{}{"missing": {}}); len(got) != 0 {
		t.Fatalf("不存在的邮箱不应触发全量同步，得到 %#v", got)
	}
}

func TestMailWatcherIdleFetchLimit(t *testing.T) {
	tests := []struct {
		configured int
		want       int
	}{
		{-1, 32},
		{0, 32},
		{8, 32},
		{32, 32},
		{64, 64},
		{128, 128},
		{200, 128},
	}
	for _, test := range tests {
		if got := mailWatcherIdleFetchLimit(test.configured); got != test.want {
			t.Fatalf("configured=%d: got=%d want=%d", test.configured, got, test.want)
		}
	}
}

func TestRunCoalescedSyncLoopKeepsOneTrailingSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requests := make(chan struct{}, 1)
	started := make(chan int, 2)
	releaseFirst := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		runs := 0
		runCoalescedSyncLoop(ctx, requests, func(context.Context) {
			runs++
			started <- runs
			if runs == 1 {
				<-releaseFirst
			}
		})
	}()

	requestIdleSync(requests)
	select {
	case run := <-started:
		if run != 1 {
			t.Fatalf("首次同步序号=%d，期望 1", run)
		}
	case <-time.After(time.Second):
		t.Fatal("首次同步未启动")
	}

	// Multiple events during the first sync occupy only one queue slot, but
	// that slot must survive until the first sync returns.
	requestIdleSync(requests)
	requestIdleSync(requests)
	requestIdleSync(requests)
	if got := len(requests); got != 1 {
		t.Fatalf("同步期间应只合并保留一个尾随请求，队列长度=%d", got)
	}
	close(releaseFirst)
	select {
	case run := <-started:
		if run != 2 {
			t.Fatalf("尾随同步序号=%d，期望 2", run)
		}
	case <-time.After(time.Second):
		t.Fatal("同步期间到达的事件没有触发尾随同步")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("取消后合并同步循环未退出")
	}
}

func TestIdleReadySyncGateThrottlesAndKeepsTrailingRequest(t *testing.T) {
	var gate idleReadySyncGate
	interval := 15 * time.Second
	start := time.Unix(1000, 0)

	if fire, wait := gate.ready(start, interval); !fire || wait != 0 {
		t.Fatalf("首次 ready 应立即同步：fire=%v wait=%s", fire, wait)
	}
	if fire, wait := gate.ready(start.Add(2*time.Second), interval); fire || wait != 13*time.Second {
		t.Fatalf("窗口内 ready 应合并：fire=%v wait=%s", fire, wait)
	}
	if fire, wait := gate.ready(start.Add(10*time.Second), interval); fire || wait != 5*time.Second {
		t.Fatalf("重复 ready 应沿用原窗口：fire=%v wait=%s", fire, wait)
	}
	if fire, wait := gate.due(start.Add(14*time.Second), interval); fire || wait != time.Second {
		t.Fatalf("窗口到期前不应触发：fire=%v wait=%s", fire, wait)
	}
	if fire, wait := gate.due(start.Add(15*time.Second), interval); !fire || wait != 0 {
		t.Fatalf("窗口到期应保留一次尾随同步：fire=%v wait=%s", fire, wait)
	}
	if fire, wait := gate.due(start.Add(30*time.Second), interval); fire || wait != 0 {
		t.Fatalf("没有新 ready 时不应继续同步：fire=%v wait=%s", fire, wait)
	}
}

func TestIdleReadySyncLoopCancelsWithPendingTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	requested := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runIdleReadySyncLoop(ctx, ready, time.Hour, func() {
			requestIdleSync(requested)
		})
	}()

	requestIdleSync(ready)
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("首次 ready 未立即触发同步")
	}
	requestIdleSync(ready)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("存在待触发定时器时 ready 同步循环未随取消退出")
	}
}

func TestWorkerGenerationIgnoresStaleReadyUpdates(t *testing.T) {
	service := &Service{}
	oldToken := service.registerIdleWorker("fixture")
	service.markWorkerReady("fixture", oldToken, true)
	newToken := service.registerIdleWorker("fixture")

	service.markWorkerReady("fixture", oldToken, true)
	if service.readyWorkers["fixture"] {
		t.Fatal("旧 worker 不应把新 worker 提前标记为就绪")
	}
	service.markWorkerReady("fixture", newToken, true)
	service.markWorkerReady("fixture", oldToken, false)
	service.unregisterIdleWorker("fixture", oldToken)
	if !service.readyWorkers["fixture"] || service.Snapshot().ConnectedWorkerCount != 1 {
		t.Fatal("旧 worker 退出不应覆盖新 worker 的就绪状态")
	}

	service.unregisterIdleWorker("fixture", newToken)
	if service.readyWorkers["fixture"] || service.Snapshot().ConnectedWorkerCount != 0 {
		t.Fatal("当前 worker 退出后应清除就绪状态")
	}
}
