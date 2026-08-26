package mailbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

func TestSyncGroupCallerCancellationDoesNotCancelSharedRun(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	service := &Service{
		syncs: make(map[string]*syncCall),
		syncRunner: func(ctx context.Context, _ []domain.Mailbox, _ time.Time, _ string, _ int) (int, error) {
			close(started)
			<-release
			finished <- ctx.Err()
			return 7, nil
		},
	}

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := service.syncGroup(callerCtx, "fixture", nil, time.Time{}, "", 0)
		result <- err
	}()
	<-started
	cancelCaller()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("调用方应按自己的 context 返回，得到 %v", err)
	}
	select {
	case <-finished:
		t.Fatal("调用方取消后共享同步不应被取消或提前结束")
	default:
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatalf("共享同步 context 被调用方取消：%v", err)
	}
}

func TestSyncGroupFreshDoesNotReuseOlderSharedRun(t *testing.T) {
	started := make(chan int, 2)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	service := &Service{
		syncs: make(map[string]*syncCall),
	}
	runs := 0
	service.syncRunner = func(context.Context, []domain.Mailbox, time.Time, string, int) (int, error) {
		runs++
		run := runs
		started <- run
		if run == 1 {
			<-releaseFirst
		} else {
			<-releaseSecond
		}
		return run, nil
	}

	firstResult := make(chan int, 1)
	go func() {
		count, _ := service.syncGroup(context.Background(), "fixture", nil, time.Time{}, "", 0)
		firstResult <- count
	}()
	if run := <-started; run != 1 {
		t.Fatalf("首次共享同步序号=%d，期望 1", run)
	}

	freshResult := make(chan int, 1)
	go func() {
		count, _ := service.SyncMailboxBatchFresh(context.Background(), []domain.Mailbox{{AccountID: "fixture"}}, time.Time{}, "", 0)
		freshResult <- count
	}()
	close(releaseFirst)
	if run := <-started; run != 2 {
		t.Fatalf("新鲜同步复用了旧调用，下一轮序号=%d", run)
	}
	close(releaseSecond)
	if count := <-firstResult; count != 1 {
		t.Fatalf("原共享同步结果=%d，期望 1", count)
	}
	if count := <-freshResult; count != 2 {
		t.Fatalf("新鲜同步结果=%d，期望来自第二轮", count)
	}
}

func TestSyncGroupFreshJoinsRunStartedAfterRequest(t *testing.T) {
	freshAfter := time.Now()
	call := &syncCall{done: make(chan struct{}), startedAt: freshAfter.Add(time.Millisecond), count: 9}
	service := &Service{syncs: map[string]*syncCall{"fixture": call}}
	service.syncRunner = func(context.Context, []domain.Mailbox, time.Time, string, int) (int, error) {
		t.Fatal("请求开始后的共享同步不应再追加一轮")
		return 0, nil
	}

	result := make(chan int, 1)
	go func() {
		count, _ := service.syncGroupFresh(context.Background(), "fixture", nil, time.Time{}, "", 0, freshAfter)
		result <- count
	}()
	close(call.done)
	if count := <-result; count != 9 {
		t.Fatalf("应复用请求开始后的同步结果，得到 %d", count)
	}
}

func TestTenConcurrentFreshCallersShareOneTrailingRound(t *testing.T) {
	started := make(chan int, 2)
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	service := &Service{syncs: make(map[string]*syncCall)}
	var mu sync.Mutex
	runs := 0
	service.syncRunner = func(context.Context, []domain.Mailbox, time.Time, string, int) (int, error) {
		mu.Lock()
		runs++
		run := runs
		mu.Unlock()
		started <- run
		<-release[run-1]
		return run, nil
	}

	firstDone := make(chan struct{})
	go func() {
		_, _ = service.syncGroup(context.Background(), "fixture", nil, time.Time{}, "", 0)
		close(firstDone)
	}()
	if run := <-started; run != 1 {
		t.Fatalf("initial run=%d, want 1", run)
	}

	const callers = 10
	start := make(chan struct{})
	results := make(chan int, callers)
	freshAfter := time.Now().Add(time.Millisecond)
	for range callers {
		go func() {
			<-start
			count, _ := service.syncGroupFresh(context.Background(), "fixture", nil, time.Time{}, "", 0, freshAfter)
			results <- count
		}()
	}
	close(start)
	time.Sleep(20 * time.Millisecond)
	close(release[0])
	if run := <-started; run != 2 {
		t.Fatalf("trailing run=%d, want 2", run)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	startedRuns := runs
	mu.Unlock()
	if startedRuns != 2 {
		t.Fatalf("10 callers started %d rounds, want 2 total", startedRuns)
	}
	close(release[1])
	<-firstDone
	for range callers {
		if count := <-results; count != 2 {
			t.Fatalf("fresh caller result=%d, want trailing round result 2", count)
		}
	}
}

func TestAccountCoordinatorSerializesDifferentFolders(t *testing.T) {
	started := make(chan string, 2)
	releaseInbox := make(chan struct{})
	releaseJunk := make(chan struct{})
	service := &Service{syncs: make(map[string]*syncCall)}
	var mu sync.Mutex
	active := 0
	maxActive := 0
	service.syncFolderRunner = func(_ context.Context, _ []domain.Mailbox, _ time.Time, _ string, _ int, folder string) (int, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		started <- folder
		if folder == protocol.IMAPSyncFolderInbox {
			<-releaseInbox
		} else {
			<-releaseJunk
		}
		mu.Lock()
		active--
		mu.Unlock()
		return 1, nil
	}

	go service.syncGroupFolderFresh(context.Background(), "fixture", nil, time.Time{}, "", 0, protocol.IMAPSyncFolderInbox, time.Now())
	if folder := <-started; folder != protocol.IMAPSyncFolderInbox {
		t.Fatalf("first folder=%q", folder)
	}
	junkDone := make(chan struct{})
	go func() {
		_, _ = service.syncGroupFolderFresh(context.Background(), "fixture", nil, time.Time{}, "", 0, protocol.IMAPSyncFolderJunk, time.Now())
		close(junkDone)
	}()
	select {
	case folder := <-started:
		t.Fatalf("folder %q overlapped active inbox scan", folder)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseInbox)
	if folder := <-started; folder != protocol.IMAPSyncFolderJunk {
		t.Fatalf("trailing folder=%q", folder)
	}
	close(releaseJunk)
	<-junkDone
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("max active account syncs=%d, want 1", maxActive)
	}
}

func TestEventSyncPreemptsScheduledFallback(t *testing.T) {
	started := make(chan string, 2)
	service := &Service{syncs: make(map[string]*syncCall)}
	service.syncFolderRunner = func(ctx context.Context, _ []domain.Mailbox, _ time.Time, _ string, _ int, folder string) (int, error) {
		started <- folder
		if folder == protocol.IMAPSyncFolderAll {
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return 1, nil
	}
	mailboxes := []domain.Mailbox{{AccountID: "fixture"}}
	lowDone := make(chan error, 1)
	go func() {
		_, err := service.SyncMailboxBatchScheduled(context.Background(), mailboxes, time.Time{}, "OpenAI", 8)
		lowDone <- err
	}()
	if folder := <-started; folder != protocol.IMAPSyncFolderAll {
		t.Fatalf("scheduled folder=%q", folder)
	}
	highDone := make(chan error, 1)
	go func() {
		_, err := service.SyncMailboxBatchFolderFresh(context.Background(), mailboxes, time.Time{}, "OpenAI", 8, protocol.IMAPSyncFolderJunk)
		highDone <- err
	}()
	select {
	case folder := <-started:
		if folder != protocol.IMAPSyncFolderJunk {
			t.Fatalf("event folder=%q", folder)
		}
	case err := <-highDone:
		t.Fatalf("event sync returned before starting: %v", err)
	case <-time.After(time.Second):
		service.syncMu.Lock()
		running := service.syncs["fixture"]
		keys := make([]string, 0, len(service.syncs))
		for key := range service.syncs {
			keys = append(keys, key)
		}
		detail := fmt.Sprintf("keys=%v", keys)
		if running != nil {
			detail += fmt.Sprintf(" folder=%q priority=%d cancelNil=%v", running.folder, running.priority, running.cancel == nil)
		}
		service.syncMu.Unlock()
		t.Fatalf("event sync did not preempt scheduled fallback; running=%s", detail)
	}
	if err := <-highDone; err != nil {
		t.Fatalf("event sync failed: %v", err)
	}
	if err := <-lowDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("scheduled fallback was not canceled: %v", err)
	}
}

func TestCodeChangeGenerationWakesAllWaiters(t *testing.T) {
	service := &Service{}
	const waiters = 10
	ready := make(chan struct{})
	woken := make(chan struct{}, waiters)
	for range waiters {
		changes := service.CodeChanges("mailbox")
		go func() {
			<-ready
			<-changes
			woken <- struct{}{}
		}()
	}
	close(ready)
	service.notifyCodeChange("mailbox")
	for range waiters {
		select {
		case <-woken:
		case <-time.After(time.Second):
			t.Fatal("not all mailbox waiters were notified")
		}
	}
	if next := service.CodeChanges("mailbox"); next == nil {
		t.Fatal("notification generation was not replaced")
	}
}

func TestConcurrentCodeConsumersClaimMessageOnce(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{Email: "claim-once@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.UpsertMessage(mailbox.ID, "imap:claim-once", "imap", "OpenAI verification code 918273", "noreply@openai.com", "Your code is 918273", time.Now()); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.Default(), state)
	const workers = 24
	start := make(chan struct{})
	results := make(chan bool, workers)
	errorsCh := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, found, err := service.CachedCodeWithQuery(mailbox.ID, CodeQuery{MarkAsServed: true})
			results <- found
			errorsCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("并发取码失败：%v", err)
		}
	}
	foundCount := 0
	for found := range results {
		if found {
			foundCount++
		}
	}
	if foundCount != 1 {
		t.Fatalf("同一验证码应只发放一次，实际成功 %d 次", foundCount)
	}
}

func TestMergeIMAPCursorPreservesConcurrentTokenAndDoesNotRegress(t *testing.T) {
	current := domain.ICloudSession{
		LoginStates: []domain.LoginState{{
			Kind:              domain.LoginStateICloudIMAP,
			IMAPEmail:         "owner@outlook.com",
			IMAPAuthMethod:    "xoauth2",
			OAuthAccessToken:  "fresh-access-token",
			OAuthRefreshToken: "fresh-refresh-token",
			IMAPLastSyncUID:   "200",
		}},
	}
	fallback := domain.LoginState{
		Kind:              domain.LoginStateICloudIMAP,
		OAuthAccessToken:  "stale-access-token",
		OAuthRefreshToken: "stale-refresh-token",
		IMAPLastSyncUID:   "100",
	}
	updated := mergeIMAPCursor(current, fallback, "150", time.Now())
	state, ok := protocol.LoginStateForKind(updated, domain.LoginStateICloudIMAP)
	if !ok {
		t.Fatal("IMAP 登录态丢失")
	}
	if state.OAuthAccessToken != "fresh-access-token" || state.OAuthRefreshToken != "fresh-refresh-token" {
		t.Fatalf("并发刷新的 OAuth token 被覆盖：%q/%q", state.OAuthAccessToken, state.OAuthRefreshToken)
	}
	if state.IMAPLastSyncUID != "200" {
		t.Fatalf("游标发生回退：%q", state.IMAPLastSyncUID)
	}

	updated = mergeIMAPCursor(updated, fallback, "250", time.Now())
	state, _ = protocol.LoginStateForKind(updated, domain.LoginStateICloudIMAP)
	if state.OAuthAccessToken != "fresh-access-token" || state.IMAPLastSyncUID != "250" {
		t.Fatalf("较新游标更新异常：token=%q uid=%q", state.OAuthAccessToken, state.IMAPLastSyncUID)
	}
}

func TestMergeIMAPSyncCursorsAdvancesJunkIndependently(t *testing.T) {
	inboxAt := time.Now().Add(-time.Minute)
	current := domain.ICloudSession{LoginStates: []domain.LoginState{{
		Kind:                domain.LoginStateICloudIMAP,
		OAuthAccessToken:    "fresh-access-token",
		IMAPLastSyncUID:     "200",
		IMAPLastSyncAt:      inboxAt,
		IMAPJunkLastSyncUID: "300",
	}}}
	fallback := domain.LoginState{
		Kind:                domain.LoginStateICloudIMAP,
		OAuthAccessToken:    "stale-access-token",
		IMAPLastSyncUID:     "100",
		IMAPJunkLastSyncUID: "100",
	}
	syncedAt := time.Now()
	updated := mergeIMAPSyncCursors(current, fallback, "150", "350", syncedAt)
	state, ok := protocol.LoginStateForKind(updated, domain.LoginStateICloudIMAP)
	if !ok {
		t.Fatal("IMAP 登录态丢失")
	}
	if state.OAuthAccessToken != "fresh-access-token" {
		t.Fatalf("并发刷新的 OAuth token 被覆盖：%q", state.OAuthAccessToken)
	}
	if state.IMAPLastSyncUID != "200" || !state.IMAPLastSyncAt.Equal(inboxAt) {
		t.Fatalf("INBOX 游标被 Junk 同步改写：uid=%q at=%s", state.IMAPLastSyncUID, state.IMAPLastSyncAt)
	}
	if state.IMAPJunkLastSyncUID != "350" || !state.IMAPJunkLastSyncAt.Equal(syncedAt) {
		t.Fatalf("Junk 游标未独立推进：uid=%q at=%s", state.IMAPJunkLastSyncUID, state.IMAPJunkLastSyncAt)
	}

	updated = mergeIMAPSyncCursors(updated, fallback, "", "325", syncedAt.Add(time.Minute))
	state, _ = protocol.LoginStateForKind(updated, domain.LoginStateICloudIMAP)
	if state.IMAPJunkLastSyncUID != "350" || !state.IMAPJunkLastSyncAt.Equal(syncedAt) {
		t.Fatalf("Junk 游标发生回退：uid=%q at=%s", state.IMAPJunkLastSyncUID, state.IMAPJunkLastSyncAt)
	}
}

func TestMaxIMAPCursorHandlesLegacyPrefix(t *testing.T) {
	if got := maxIMAPCursor("250", "imap:200"); got != "250" {
		t.Fatalf("应保留较大游标，得到 %q", got)
	}
	if got := maxIMAPCursor("imap:300", "250"); got != "imap:300" {
		t.Fatalf("应保留带前缀的较大游标，得到 %q", got)
	}
	if got := maxIMAPCursor("", "42"); got != "42" {
		t.Fatalf("空游标合并错误，得到 %q", got)
	}
}
