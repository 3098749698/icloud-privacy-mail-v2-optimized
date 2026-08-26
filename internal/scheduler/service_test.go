package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

type recordingCreator struct {
	attempts int
	syncs    int
	failAt   int
	err      error
	synced   []domain.Mailbox
	syncErr  error
}

type blockingSyncCreator struct {
	started chan struct{}
	release chan struct{}
	synced  []domain.Mailbox
}

func (f *blockingSyncCreator) Create(context.Context, string, string, string, string) (domain.Mailbox, error) {
	return domain.Mailbox{}, errors.New("unexpected create")
}

func (f *blockingSyncCreator) SyncRemote(ctx context.Context, _ string) ([]domain.Mailbox, error) {
	close(f.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
		return f.synced, nil
	}
}

func (f *recordingCreator) Create(_ context.Context, accountID, _, _, _ string) (domain.Mailbox, error) {
	f.attempts++
	if f.failAt > 0 && f.attempts == f.failAt {
		return domain.Mailbox{}, f.err
	}
	return domain.Mailbox{ID: fmt.Sprintf("created_%d", f.attempts), AccountID: accountID, Email: fmt.Sprintf("created_%d@icloud.com", f.attempts)}, nil
}

func (f *recordingCreator) SyncRemote(context.Context, string) ([]domain.Mailbox, error) {
	f.syncs++
	return f.synced, f.syncErr
}

func TestAccountRoundCreatesTwentyAndPersistsWaitingSchedule(t *testing.T) {
	state := schedulerTestStore(t, []string{"acc_1"}, nil)
	creator := &recordingCreator{}
	service := &Service{store: state, mailbox: creator, waitFn: func(context.Context, time.Duration) error { return nil }, inFlight: map[string]bool{}}
	now := time.Now()
	_, err := state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, Label: "x", CreateChannel: "auto", IntervalSeconds: 3600, RoundIntervalSecond: 1, RoundLimit: 20, Target: 750, NextRunAt: now})
	if err != nil {
		t.Fatal(err)
	}

	service.runAccountRound(context.Background(), "acc_1")
	task, _ := state.SchedulerTask("acc_1")
	if creator.syncs != 1 {
		t.Fatalf("每轮应先同步，实际同步 %d 次", creator.syncs)
	}
	if creator.attempts != 20 || task.Progress != 20 || task.Success != 20 {
		t.Fatalf("创建结果异常: attempts=%d task=%+v", creator.attempts, task)
	}
	if task.Status != domain.SchedulerTaskWaiting || task.NextRunAt.Before(now.Add(59*time.Minute)) {
		t.Fatalf("下轮调度异常: %+v", task)
	}
}

func TestQuotaErrorWaitsInsteadOfCompleting(t *testing.T) {
	mailboxes := testMailboxes("acc_1", 749)
	state := schedulerTestStore(t, []string{"acc_1"}, mailboxes)
	creator := &recordingCreator{failAt: 1, err: errors.New("已达到当前隐私邮箱创建上限，请稍后再试"), synced: mailboxes}
	service := &Service{store: state, mailbox: creator, waitFn: func(context.Context, time.Duration) error { return nil }, inFlight: map[string]bool{}}
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, Label: "x", CreateChannel: "auto", IntervalSeconds: 60, RoundLimit: 20, Target: 750, NextRunAt: time.Now()})

	service.runAccountRound(context.Background(), "acc_1")
	task, _ := state.SchedulerTask("acc_1")
	if creator.syncs != 2 {
		t.Fatalf("限额后应再次同步，实际同步 %d 次", creator.syncs)
	}
	if task.Status != domain.SchedulerTaskWaiting || task.CompletionReason != "" || task.Progress != 749 {
		t.Fatalf("临时限额不应完成任务: %+v", task)
	}
	if task.Failed != 1 || task.NextRunAt.IsZero() {
		t.Fatalf("限额后应记录错误并等待: %+v", task)
	}
}

func TestRoundWaitsAndKeepsCheckingAtActualTarget(t *testing.T) {
	mailboxes := testMailboxes("acc_1", 750)
	state := schedulerTestStore(t, []string{"acc_1"}, mailboxes)
	creator := &recordingCreator{synced: mailboxes}
	service := &Service{store: state, mailbox: creator, inFlight: map[string]bool{}}
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, Label: "x", CreateChannel: "auto", IntervalSeconds: 60, RoundLimit: 20, Target: 750, NextRunAt: time.Now()})

	service.runAccountRound(context.Background(), "acc_1")
	task, _ := state.SchedulerTask("acc_1")
	if creator.attempts != 0 || task.Status != domain.SchedulerTaskWaiting || task.CompletionReason != "capacity_full" || task.NextRunAt.IsZero() {
		t.Fatalf("达到实际目标后应保持巡检: %+v", task)
	}
}

func TestPauseDuringCapacitySyncRemainsPaused(t *testing.T) {
	mailboxes := testMailboxes("acc_1", 750)
	state := schedulerTestStore(t, []string{"acc_1"}, mailboxes)
	creator := &blockingSyncCreator{started: make(chan struct{}), release: make(chan struct{}), synced: mailboxes}
	service := &Service{store: state, mailbox: creator, inFlight: map[string]bool{}}
	_, err := state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, Label: "x", CreateChannel: "auto",
		IntervalSeconds: 60, RoundLimit: 20, Target: 750, NextRunAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		service.runAccountRound(context.Background(), "acc_1")
	}()
	select {
	case <-creator.started:
	case <-time.After(time.Second):
		t.Fatal("remote sync did not start")
	}
	if _, err := service.StopAccount("acc_1", "user paused"); err != nil {
		t.Fatal(err)
	}
	close(creator.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("account round did not finish")
	}

	task, _ := state.SchedulerTask("acc_1")
	if task.Status != domain.SchedulerTaskPaused || !task.NextRunAt.IsZero() || task.CompletionReason == "capacity_full" {
		t.Fatalf("capacity result overrode a concurrent pause: %+v", task)
	}
}

func TestCapacityTaskCreatesAgainAfterRemoteAliasWasDeleted(t *testing.T) {
	full := testMailboxes("acc_1", 750)
	state := schedulerTestStore(t, []string{"acc_1"}, full)
	creator := &recordingCreator{synced: full}
	service := &Service{store: state, mailbox: creator, waitFn: func(context.Context, time.Duration) error { return nil }, inFlight: map[string]bool{}}
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, Label: "x", CreateChannel: "auto", IntervalSeconds: 60, RoundLimit: 20, Target: 750, NextRunAt: time.Now()})

	service.runAccountRound(context.Background(), "acc_1")
	creator.synced = testMailboxes("acc_1", 749)
	service.runAccountRound(context.Background(), "acc_1")
	task, _ := state.SchedulerTask("acc_1")
	if creator.attempts != 1 || task.Progress != 750 || task.Status != domain.SchedulerTaskWaiting || task.CompletionReason != "capacity_full" {
		t.Fatalf("Apple 别名删除后没有自动补建到目标: attempts=%d task=%+v", creator.attempts, task)
	}
}

func TestCapacityReasonClearsWhileTaskIsStillRefilling(t *testing.T) {
	full := testMailboxes("acc_1", 750)
	state := schedulerTestStore(t, []string{"acc_1"}, full)
	creator := &recordingCreator{synced: testMailboxes("acc_1", 748)}
	service := &Service{store: state, mailbox: creator, waitFn: func(context.Context, time.Duration) error { return nil }, inFlight: map[string]bool{}}
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, CompletionReason: "capacity_full",
		Label: "x", CreateChannel: "auto", IntervalSeconds: 60, RoundLimit: 1, Target: 750,
		Progress: 750, NextRunAt: time.Now(),
	})

	service.runAccountRound(context.Background(), "acc_1")
	task, _ := state.SchedulerTask("acc_1")
	if creator.attempts != 1 || task.Progress != 749 || task.Status != domain.SchedulerTaskWaiting || task.CompletionReason != "" {
		t.Fatalf("task retained a stale capacity marker while refilling: attempts=%d task=%+v", creator.attempts, task)
	}
}

func TestNotifyCapacityReleasedAdvancesCapacityFullTask(t *testing.T) {
	state := schedulerTestStore(t, []string{"acc_1"}, testMailboxes("acc_1", 749))
	future := time.Now().Add(time.Hour)
	_, err := state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, CompletionReason: "capacity_full",
		IntervalSeconds: 3600, RoundLimit: 20, Target: 750, Progress: 750, NextRunAt: future,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{store: state, mailbox: &recordingCreator{}, wake: make(chan struct{}, 1), inFlight: map[string]bool{}}

	before := time.Now()
	if !service.NotifyCapacityReleased("acc_1") {
		t.Fatal("capacity-full task was not notified")
	}
	task, _ := state.SchedulerTask("acc_1")
	if task.Status != domain.SchedulerTaskWaiting || task.CompletionReason != "capacity_full" {
		t.Fatalf("notification changed task lifecycle: %+v", task)
	}
	if task.NextRunAt.Before(before) || task.NextRunAt.After(time.Now()) {
		t.Fatalf("task was not made immediately due: next_run_at=%s", task.NextRunAt)
	}
	if len(task.Events) == 0 || task.Events[len(task.Events)-1].Type != "capacity_released" {
		t.Fatalf("capacity release event missing: %+v", task.Events)
	}
	select {
	case <-service.wake:
	default:
		t.Fatal("capacity release did not wake the scheduler dispatcher")
	}
}

func TestNotifyCapacityReleasedLeavesPausedAndOrdinaryWaitingTasksAlone(t *testing.T) {
	state := schedulerTestStore(t, []string{"acc_1", "acc_2"}, nil)
	future := time.Now().Add(time.Hour)
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "acc_1", Status: domain.SchedulerTaskPaused, CompletionReason: "capacity_full", NextRunAt: future,
	})
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "acc_2", Status: domain.SchedulerTaskWaiting, NextRunAt: future,
	})
	service := &Service{store: state, mailbox: &recordingCreator{}, inFlight: map[string]bool{}}

	if service.NotifyCapacityReleased("acc_1") || service.NotifyCapacityReleased("acc_2") || service.NotifyCapacityReleased("missing") {
		t.Fatal("notification woke a task that was not waiting at capacity")
	}
	paused, _ := state.SchedulerTask("acc_1")
	waiting, _ := state.SchedulerTask("acc_2")
	if !paused.NextRunAt.Equal(future) || !waiting.NextRunAt.Equal(future) {
		t.Fatalf("unrelated tasks changed: paused=%+v waiting=%+v", paused, waiting)
	}
}

func TestDueTasksRecoversOrphanedRunningTask(t *testing.T) {
	state := schedulerTestStore(t, []string{"acc_1"}, nil)
	service := &Service{store: state, mailbox: &recordingCreator{}, inFlight: map[string]bool{}}
	_, err := state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "acc_1", Status: domain.SchedulerTaskRunning, Label: "x", CreateChannel: "auto",
		IntervalSeconds: 60, RoundLimit: 20, Target: 750,
	})
	if err != nil {
		t.Fatal(err)
	}

	due, _ := service.dueTasks()
	task, _ := state.SchedulerTask("acc_1")
	if len(due) != 1 || task.Status != domain.SchedulerTaskWaiting || task.NextRunAt.IsZero() {
		t.Fatalf("orphaned running task was not recovered: due=%+v task=%+v", due, task)
	}
	if len(task.Events) == 0 || task.Events[len(task.Events)-1].Type != "orphan_recovered" {
		t.Fatalf("orphan recovery event missing: %+v", task.Events)
	}
}

func TestCapacityPersistenceFailureRecoversAfterStorageReturns(t *testing.T) {
	mailboxes := testMailboxes("acc_1", 750)
	state := schedulerTestStore(t, []string{"acc_1"}, mailboxes)
	creator := &blockingSyncCreator{started: make(chan struct{}), release: make(chan struct{}), synced: mailboxes}
	service := &Service{store: state, mailbox: creator, inFlight: map[string]bool{}}
	_, err := state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, Label: "x", CreateChannel: "auto",
		IntervalSeconds: 60, RoundLimit: 20, Target: 750, NextRunAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		service.runAccountRound(context.Background(), "acc_1")
	}()
	select {
	case <-creator.started:
	case <-time.After(time.Second):
		t.Fatal("remote sync did not start")
	}
	statePath := state.Path()
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	close(creator.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("account round did not finish")
	}
	stuck, _ := state.SchedulerTask("acc_1")
	if stuck.Status != domain.SchedulerTaskRunning {
		t.Fatalf("fixture did not produce an unpersisted running state: %+v", stuck)
	}

	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	due, _ := service.dueTasks()
	recovered, _ := state.SchedulerTask("acc_1")
	if len(due) != 1 || recovered.Status != domain.SchedulerTaskWaiting || recovered.NextRunAt.IsZero() {
		t.Fatalf("task did not recover after persistence returned: due=%+v task=%+v", due, recovered)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("recovered task was not persisted: %v", err)
	}
}

func TestRemoteSyncCountOverridesStaleLocalCount(t *testing.T) {
	local := testMailboxes("acc_1", 750)
	remote := testMailboxes("acc_1", 749)
	state := schedulerTestStore(t, []string{"acc_1"}, local)
	creator := &recordingCreator{failAt: 1, err: errors.New("已达到当前隐私邮箱创建上限，请稍后再试"), synced: remote}
	service := &Service{store: state, mailbox: creator, waitFn: func(context.Context, time.Duration) error { return nil }, inFlight: map[string]bool{}}
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, Label: "x", CreateChannel: "auto", IntervalSeconds: 60, RoundLimit: 20, Target: 750, NextRunAt: time.Now()})

	service.runAccountRound(context.Background(), "acc_1")
	task, _ := state.SchedulerTask("acc_1")
	if task.Status != domain.SchedulerTaskWaiting || task.Progress != 749 || task.CompletionReason != "" {
		t.Fatalf("本地陈旧记录不应导致任务提前完成: %+v", task)
	}
}

func TestSyncFailureWaitsWithoutUsingStaleLocalCount(t *testing.T) {
	state := schedulerTestStore(t, []string{"acc_1"}, testMailboxes("acc_1", 750))
	creator := &recordingCreator{syncErr: errors.New("temporary sync failure")}
	service := &Service{store: state, mailbox: creator, waitFn: func(context.Context, time.Duration) error { return nil }, inFlight: map[string]bool{}}
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_1", Status: domain.SchedulerTaskWaiting, Label: "x", CreateChannel: "auto", IntervalSeconds: 60, RoundLimit: 20, Target: 750, NextRunAt: time.Now()})

	service.runAccountRound(context.Background(), "acc_1")
	task, _ := state.SchedulerTask("acc_1")
	if creator.attempts != 0 || task.Status != domain.SchedulerTaskWaiting || task.CompletionReason != "" {
		t.Fatalf("同步失败时不应创建或完成任务: attempts=%d task=%+v", creator.attempts, task)
	}
}

func testMailboxes(accountID string, count int) []domain.Mailbox {
	mailboxes := make([]domain.Mailbox, 0, count)
	for i := 0; i < count; i++ {
		mailboxes = append(mailboxes, domain.Mailbox{ID: fmt.Sprintf("mbx_%d", i), AccountID: accountID, Email: fmt.Sprintf("x%d@icloud.com", i)})
	}
	return mailboxes
}

func TestResumeRestoresPersistedActiveTasks(t *testing.T) {
	state := schedulerTestStore(t, []string{"acc_1", "acc_2", "acc_3"}, nil)
	future := time.Now().Add(time.Hour)
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_1", Status: domain.SchedulerTaskRunning, IntervalSeconds: 3600, RoundLimit: 20, Target: 750, NextRunAt: future})
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_2", Status: domain.SchedulerTaskPaused, IntervalSeconds: 3600, RoundLimit: 20, Target: 750})
	_, _ = state.SaveSchedulerTask(domain.SchedulerTask{AccountID: "acc_3", Status: domain.SchedulerTaskCompleted, CompletionReason: "target", Progress: 750, IntervalSeconds: 3600, RoundLimit: 20, Target: 750})
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{store: state, mailbox: &recordingCreator{synced: testMailboxes("acc_3", 750)}, waitFn: waitFor, inFlight: map[string]bool{}}
	t.Cleanup(func() { cancel(); service.Shutdown() })
	if _, err := service.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	active, _ := state.SchedulerTask("acc_1")
	paused, _ := state.SchedulerTask("acc_2")
	var legacyFull domain.SchedulerTask
	deadline := time.Now().Add(time.Second)
	for {
		legacyFull, _ = state.SchedulerTask("acc_3")
		if legacyFull.Status == domain.SchedulerTaskWaiting && legacyFull.CompletionReason == "capacity_full" && !legacyFull.NextRunAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if active.Status != domain.SchedulerTaskWaiting || paused.Status != domain.SchedulerTaskPaused || legacyFull.Status != domain.SchedulerTaskWaiting || legacyFull.CompletionReason != "capacity_full" || legacyFull.NextRunAt.IsZero() {
		t.Fatalf("恢复状态异常 active=%s paused=%s legacyFull=%+v", active.Status, paused.Status, legacyFull)
	}
}

func TestTasksAreIndependent(t *testing.T) {
	state := schedulerTestStore(t, []string{"acc_1", "acc_2"}, nil)
	service := &Service{store: state, mailbox: &recordingCreator{}, inFlight: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := Config{Label: "x", CreateChannel: "auto", IntervalSeconds: 3600, RoundIntervalSecond: 1}
	if _, err := service.StartAccount(ctx, "acc_1", cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartAccount(ctx, "acc_2", cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StopAccount("acc_1", "pause"); err != nil {
		t.Fatal(err)
	}
	one, _ := state.SchedulerTask("acc_1")
	two, _ := state.SchedulerTask("acc_2")
	if one.Status != domain.SchedulerTaskPaused || !isActiveTask(two) {
		t.Fatalf("账号任务相互干扰 one=%s two=%s", one.Status, two.Status)
	}
	cancel()
	service.Shutdown()
}

func TestStartAccountUsesCustomLimits(t *testing.T) {
	state := schedulerTestStore(t, []string{"acc_1"}, nil)
	service := &Service{store: state, mailbox: &recordingCreator{}, inFlight: map[string]bool{}}
	cfg := service.normalizeConfig(Config{
		Label:               "x",
		CreateChannel:       "auto",
		IntervalSeconds:     3600,
		RoundIntervalSecond: 1,
		RoundLimit:          7,
		Target:              123,
	})
	task, err := service.startAccount(cfg, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if task.RoundLimit != 7 || task.Target != 123 {
		t.Fatalf("自定义任务数量未保存: round_limit=%d target=%d", task.RoundLimit, task.Target)
	}
}

func TestNormalizeConfigCapsLimitsAtAccountMaximum(t *testing.T) {
	service := &Service{}
	cfg := service.normalizeConfig(Config{RoundLimit: 900, Target: 900})
	if cfg.Target != 750 || cfg.RoundLimit != 750 {
		t.Fatalf("任务数量上限异常: round_limit=%d target=%d", cfg.RoundLimit, cfg.Target)
	}
}

func schedulerTestStore(t *testing.T, accountIDs []string, mailboxes []domain.Mailbox) *store.Store {
	t.Helper()
	now := time.Now()
	accounts := make([]domain.AppleAccount, 0, len(accountIDs))
	sessions := make([]domain.ICloudSession, 0, len(accountIDs))
	for _, id := range accountIDs {
		accounts = append(accounts, domain.AppleAccount{ID: id, AppleID: id + "@icloud.com", Status: domain.StatusActive, CreatedAt: now, UpdatedAt: now})
		sessions = append(sessions, domain.ICloudSession{AccountID: id, AppleID: id + "@icloud.com", SavedAt: now})
	}
	for i := range mailboxes {
		mailboxes[i].CreatedAt, mailboxes[i].UpdatedAt = now, now
	}
	path := filepath.Join(t.TempDir(), "state.json")
	raw, err := json.Marshal(domain.State{SchemaVersion: domain.SchemaVersion, NextID: len(mailboxes) + 1, AppleAccounts: accounts, ICloudSessions: sessions, Mailboxes: mailboxes})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
