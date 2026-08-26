package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

const (
	maxEvents                      = 200
	maxTaskEvents                  = 100
	defaultRoundLimitPerAccount    = 20
	defaultMailboxTargetPerAccount = 750
	defaultWorkerConcurrency       = 3
)

type mailboxCreator interface {
	Create(context.Context, string, string, string, string) (domain.Mailbox, error)
}

type remoteSyncer interface {
	SyncRemote(context.Context, string) ([]domain.Mailbox, error)
}

type Config struct {
	AccountIDs          []string `json:"account_ids"`
	Label               string   `json:"label"`
	Note                string   `json:"note"`
	CreateChannel       string   `json:"create_channel"`
	IntervalMinutes     int      `json:"interval_minutes"`
	IntervalSeconds     int      `json:"interval_seconds,omitempty"`
	RoundIntervalSecond int      `json:"round_interval_seconds"`
	RoundLimit          int      `json:"round_limit"`
	Target              int      `json:"target"`
}

type Event struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	AccountID string    `json:"account_id,omitempty"`
	MailboxID string    `json:"mailbox_id,omitempty"`
	Email     string    `json:"email,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type Stats struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Waiting   int `json:"waiting"`
	Running   int `json:"running"`
	Paused    int `json:"paused"`
	Completed int `json:"completed"`
	Success   int `json:"success"`
	Failed    int `json:"failed"`
	Progress  int `json:"progress"`
	Target    int `json:"target"`
}

// State keeps legacy aggregate fields while exposing per-account tasks.
type State struct {
	Running              bool                   `json:"running"`
	Status               string                 `json:"status"`
	Tasks                []domain.SchedulerTask `json:"tasks"`
	Stats                Stats                  `json:"stats"`
	AccountIDs           []string               `json:"account_ids"`
	Label                string                 `json:"label"`
	Note                 string                 `json:"note"`
	CreateChannel        string                 `json:"create_channel"`
	IntervalSeconds      int                    `json:"interval_seconds"`
	RoundIntervalSecond  int                    `json:"round_interval_seconds"`
	RoundLimitPerAccount int                    `json:"round_limit_per_account"`
	TargetPerAccount     int                    `json:"target_per_account"`
	AccountProgress      map[string]int         `json:"account_progress,omitempty"`
	BatchIndex           int                    `json:"batch_index"`
	Success              int                    `json:"success"`
	Failed               int                    `json:"failed"`
	StartedAt            time.Time              `json:"started_at,omitempty"`
	LastRunAt            time.Time              `json:"last_run_at,omitempty"`
	NextRunAt            time.Time              `json:"next_run_at,omitempty"`
	StoppedAt            time.Time              `json:"stopped_at,omitempty"`
	LastError            string                 `json:"last_error,omitempty"`
	Events               []Event                `json:"events"`
}

type Service struct {
	mu        sync.Mutex
	store     *store.Store
	mailbox   mailboxCreator
	cancel    context.CancelFunc
	wake      chan struct{}
	sem       chan struct{}
	inFlight  map[string]bool
	waitFn    func(context.Context, time.Duration) error
	runtimeWG sync.WaitGroup
}

func NewService(state *store.Store, mailbox *mailboxservice.Service) *Service {
	return &Service{store: state, mailbox: mailbox, waitFn: waitFor, inFlight: map[string]bool{}}
}

// Resume recovers running/waiting tasks after a process restart.
func (s *Service) Resume(parent context.Context) (State, error) {
	now := time.Now()
	for _, task := range s.store.SchedulerTasks() {
		if task.Status == domain.SchedulerTaskCompleted && task.CompletionReason == "target" {
			if _, err := s.store.UpdateSchedulerTask(task.AccountID, func(next *domain.SchedulerTask) error {
				next.Status = domain.SchedulerTaskWaiting
				next.CompletionReason = "capacity_full"
				next.CompletedAt, next.StoppedAt = time.Time{}, time.Time{}
				next.NextRunAt = now
				appendTaskEvent(next, domain.SchedulerTaskEvent{Type: "continuous_mode", Message: "任务已切换为持续补建模式，将检查空余名额"})
				return nil
			}); err != nil {
				return State{}, err
			}
			continue
		}
		if task.Status != domain.SchedulerTaskRunning && task.Status != domain.SchedulerTaskWaiting {
			continue
		}
		if _, err := s.store.UpdateSchedulerTask(task.AccountID, func(next *domain.SchedulerTask) error {
			next.Status = domain.SchedulerTaskWaiting
			if next.NextRunAt.IsZero() {
				next.NextRunAt = now
			}
			return nil
		}); err != nil {
			return State{}, err
		}
	}
	s.ensureRuntime(parent)
	s.signalWake()
	return s.Snapshot(), nil
}

func (s *Service) Start(parent context.Context, cfg Config) (State, error) {
	cfg = s.normalizeConfig(cfg)
	if len(cfg.AccountIDs) == 0 {
		return State{}, errors.New("请至少选择一个 Apple 账号")
	}
	for _, id := range cfg.AccountIDs {
		if err := s.validateAccount(id); err != nil {
			return State{}, err
		}
	}
	s.ensureRuntime(parent)
	for _, id := range cfg.AccountIDs {
		if _, err := s.startAccount(cfg, id); err != nil {
			return State{}, err
		}
	}
	s.signalWake()
	return s.Snapshot(), nil
}

func (s *Service) StartAccount(parent context.Context, accountID string, cfg Config) (domain.SchedulerTask, error) {
	accountID = strings.TrimSpace(accountID)
	cfg.AccountIDs = []string{accountID}
	cfg = s.normalizeConfig(cfg)
	if err := s.validateAccount(accountID); err != nil {
		return domain.SchedulerTask{}, err
	}
	s.ensureRuntime(parent)
	task, err := s.startAccount(cfg, accountID)
	if err == nil {
		s.signalWake()
	}
	return task, err
}

func (s *Service) startAccount(cfg Config, accountID string) (domain.SchedulerTask, error) {
	now := time.Now()
	count := s.mailboxCounts([]string{accountID})[accountID]
	account, _ := s.store.FindAppleAccount(accountID)
	task, exists := s.store.SchedulerTask(accountID)
	if !exists {
		task = domain.SchedulerTask{AccountID: accountID, AppleID: account.AppleID, CreatedAt: now}
	}
	task.Status = domain.SchedulerTaskWaiting
	task.Label, task.Note, task.CreateChannel = cfg.Label, cfg.Note, cfg.CreateChannel
	task.IntervalSeconds = s.interval(cfg)
	task.RoundIntervalSecond = cfg.RoundIntervalSecond
	task.RoundLimit, task.Target, task.Progress = cfg.RoundLimit, cfg.Target, count
	task.StartedAt, task.NextRunAt = now, now
	task.StoppedAt, task.CompletedAt = time.Time{}, time.Time{}
	task.CompletionReason, task.LastError = "", ""
	appendTaskEvent(&task, domain.SchedulerTaskEvent{Type: "started", Message: "账号创建任务已启动"})
	return s.store.SaveSchedulerTask(task)
}

func (s *Service) StopAccount(accountID, message string) (domain.SchedulerTask, error) {
	if strings.TrimSpace(message) == "" {
		message = "账号创建任务已暂停"
	}
	task, err := s.store.UpdateSchedulerTask(strings.TrimSpace(accountID), func(task *domain.SchedulerTask) error {
		task.Status, task.StoppedAt, task.NextRunAt = domain.SchedulerTaskPaused, time.Now(), time.Time{}
		appendTaskEvent(task, domain.SchedulerTaskEvent{Type: "paused", Message: message})
		return nil
	})
	s.signalWake()
	return task, err
}

func (s *Service) ResumeAccount(parent context.Context, accountID string) (domain.SchedulerTask, error) {
	accountID = strings.TrimSpace(accountID)
	if err := s.validateAccount(accountID); err != nil {
		return domain.SchedulerTask{}, err
	}
	s.ensureRuntime(parent)
	task, err := s.store.UpdateSchedulerTask(accountID, func(task *domain.SchedulerTask) error {
		task.Status, task.NextRunAt = domain.SchedulerTaskWaiting, time.Now()
		task.StoppedAt, task.CompletedAt = time.Time{}, time.Time{}
		task.CompletionReason = ""
		appendTaskEvent(task, domain.SchedulerTaskEvent{Type: "resumed", Message: "账号创建任务已继续"})
		return nil
	})
	if err == nil {
		s.signalWake()
	}
	return task, err
}

func (s *Service) DeleteAccountTask(accountID string) error {
	if _, err := s.StopAccount(accountID, "账号创建任务已删除"); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.Lock()
		busy := s.inFlight[strings.TrimSpace(accountID)]
		s.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("账号任务正在完成当前创建请求，请稍后重试删除")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := s.store.DeleteSchedulerTask(accountID); err != nil {
		return err
	}
	s.signalWake()
	return nil
}

// AddAccounts and RemoveAccounts preserve the old HTTP contract.
func (s *Service) AddAccounts(accountIDs []string) (State, error) {
	cfg := DefaultConfig(s.store)
	cfg.AccountIDs = normalizeAccountIDs(accountIDs)
	if len(cfg.AccountIDs) == 0 {
		return State{}, errors.New("请至少选择一个要加入的 Apple 账号")
	}
	s.mu.Lock()
	runtimeReady := s.cancel != nil
	s.mu.Unlock()
	if !runtimeReady {
		return State{}, errors.New("调度服务尚未运行")
	}
	for _, id := range cfg.AccountIDs {
		if err := s.validateAccount(id); err != nil {
			return State{}, err
		}
		if _, err := s.startAccount(cfg, id); err != nil {
			return State{}, err
		}
	}
	s.signalWake()
	return s.Snapshot(), nil
}

func (s *Service) RemoveAccounts(accountIDs []string) (State, error) {
	accountIDs = normalizeAccountIDs(accountIDs)
	if len(accountIDs) == 0 {
		return State{}, errors.New("请至少选择一个要移出的 Apple 账号")
	}
	for _, id := range accountIDs {
		if _, err := s.StopAccount(id, "账号已移出运行队列"); err != nil {
			return State{}, err
		}
	}
	return s.Snapshot(), nil
}

func (s *Service) Stop(message string) State {
	if strings.TrimSpace(message) == "" {
		message = "定时创建已停止"
	}
	for _, task := range s.store.SchedulerTasks() {
		if isActiveTask(task) {
			_, _ = s.StopAccount(task.AccountID, message)
		}
	}
	return s.Snapshot()
}

// Shutdown waits until the dispatcher and any active account round have stopped.
func (s *Service) Shutdown() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.runtimeWG.Wait()
}

func (s *Service) ClearEvents() State {
	for _, task := range s.store.SchedulerTasks() {
		_, _ = s.store.UpdateSchedulerTask(task.AccountID, func(next *domain.SchedulerTask) error { next.Events = nil; return nil })
	}
	return s.Snapshot()
}

func (s *Service) Snapshot() State {
	tasks := s.store.SchedulerTasks()
	out := State{Tasks: tasks, AccountProgress: map[string]int{}, Events: []Event{}, RoundLimitPerAccount: defaultRoundLimitPerAccount, TargetPerAccount: defaultMailboxTargetPerAccount}
	first := true
	for _, task := range tasks {
		out.Stats.Total++
		out.Stats.Success += task.Success
		out.Stats.Failed += task.Failed
		out.Stats.Progress += task.Progress
		out.Stats.Target += task.Target
		out.Success += task.Success
		out.Failed += task.Failed
		out.BatchIndex += task.RoundIndex
		out.AccountProgress[task.AccountID] = task.Progress
		switch task.Status {
		case domain.SchedulerTaskRunning:
			out.Stats.Running++
			out.Stats.Active++
		case domain.SchedulerTaskWaiting:
			out.Stats.Waiting++
			out.Stats.Active++
		case domain.SchedulerTaskPaused:
			out.Stats.Paused++
		case domain.SchedulerTaskCompleted:
			out.Stats.Completed++
		}
		if isActiveTask(task) {
			out.Running = true
			out.AccountIDs = append(out.AccountIDs, task.AccountID)
		}
		if !task.StartedAt.IsZero() && (first || task.StartedAt.Before(out.StartedAt)) {
			out.StartedAt = task.StartedAt
			first = false
		}
		if task.LastRunAt.After(out.LastRunAt) {
			out.LastRunAt = task.LastRunAt
		}
		if task.StoppedAt.After(out.StoppedAt) {
			out.StoppedAt = task.StoppedAt
		}
		if isActiveTask(task) && !task.NextRunAt.IsZero() && (out.NextRunAt.IsZero() || task.NextRunAt.Before(out.NextRunAt)) {
			out.NextRunAt = task.NextRunAt
		}
		if task.LastError != "" {
			out.LastError = task.LastError
		}
		if out.Label == "" {
			out.Label, out.Note, out.CreateChannel = task.Label, task.Note, task.CreateChannel
			out.IntervalSeconds, out.RoundIntervalSecond = task.IntervalSeconds, task.RoundIntervalSecond
		}
		for _, e := range task.Events {
			out.Events = append(out.Events, Event{ID: e.ID, At: e.At, Type: e.Type, Message: e.Message, AccountID: task.AccountID, MailboxID: e.MailboxID, Email: e.Email, Error: e.Error})
		}
	}
	sort.SliceStable(out.Events, func(i, j int) bool { return out.Events[i].At.Before(out.Events[j].At) })
	if len(out.Events) > maxEvents {
		out.Events = append([]Event(nil), out.Events[len(out.Events)-maxEvents:]...)
	}
	if out.Running {
		out.Status = "running"
	} else if out.Stats.Total == 0 {
		out.Status = "idle"
	} else if out.Stats.Completed == out.Stats.Total {
		out.Status = "completed"
	} else {
		out.Status = "stopped"
	}
	return out
}

func (s *Service) RecordManualSuccess(string, domain.Mailbox) State { return s.Snapshot() }
func (s *Service) RecordManualFailure(string, error) State          { return s.Snapshot() }

// NotifyCapacityReleased advances a capacity-full task so it can refill the
// newly available slot without waiting for its normal polling interval.
func (s *Service) NotifyCapacityReleased(accountID string) bool {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false
	}
	task, ok := s.store.SchedulerTask(accountID)
	if !ok || task.Status != domain.SchedulerTaskWaiting || task.CompletionReason != "capacity_full" {
		return false
	}

	now := time.Now()
	advanced := false
	if task.NextRunAt.IsZero() || task.NextRunAt.After(now) {
		if _, err := s.store.UpdateSchedulerTask(accountID, func(next *domain.SchedulerTask) error {
			if next.Status != domain.SchedulerTaskWaiting || next.CompletionReason != "capacity_full" {
				return nil
			}
			if next.NextRunAt.IsZero() || next.NextRunAt.After(now) {
				next.NextRunAt = now
				appendTaskEvent(next, domain.SchedulerTaskEvent{Type: "capacity_released", Message: "检测到隐私邮箱配额已释放，已提前安排补建检查"})
				advanced = true
			}
			return nil
		}); err != nil {
			return false
		}
	}
	if advanced || !task.NextRunAt.After(now) {
		s.signalWake()
		return true
	}
	return false
}

func (s *Service) ensureRuntime(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel, s.wake, s.sem = cancel, make(chan struct{}, 1), make(chan struct{}, defaultWorkerConcurrency)
	if s.inFlight == nil {
		s.inFlight = map[string]bool{}
	}
	s.runtimeWG.Add(1)
	go func() {
		defer s.runtimeWG.Done()
		s.dispatch(ctx)
	}()
}

func (s *Service) dispatch(ctx context.Context) {
	for {
		tasks, delay := s.dueTasks()
		for _, task := range tasks {
			select {
			case s.sem <- struct{}{}:
				s.mu.Lock()
				if s.inFlight[task.AccountID] {
					s.mu.Unlock()
					<-s.sem
					continue
				}
				s.inFlight[task.AccountID] = true
				s.mu.Unlock()
				s.runtimeWG.Add(1)
				go func(id string) {
					defer s.runtimeWG.Done()
					defer func() { <-s.sem; s.mu.Lock(); delete(s.inFlight, id); s.mu.Unlock(); s.signalWake() }()
					s.runAccountRound(ctx, id)
				}(task.AccountID)
			default:
			}
		}
		if !s.waitForSchedule(ctx, delay) {
			s.mu.Lock()
			s.cancel = nil
			s.mu.Unlock()
			return
		}
	}
}

func (s *Service) dueTasks() ([]domain.SchedulerTask, time.Duration) {
	now, next := time.Now(), time.Now().Add(time.Hour)
	var due []domain.SchedulerTask
	for _, task := range s.store.SchedulerTasks() {
		s.mu.Lock()
		busy := s.inFlight[task.AccountID]
		s.mu.Unlock()
		if busy {
			continue
		}
		if task.Status == domain.SchedulerTaskRunning {
			recovered, err := s.store.UpdateSchedulerTask(task.AccountID, func(next *domain.SchedulerTask) error {
				if next.Status != domain.SchedulerTaskRunning {
					return nil
				}
				next.Status = domain.SchedulerTaskWaiting
				next.NextRunAt = now
				appendTaskEvent(next, domain.SchedulerTaskEvent{Type: "orphan_recovered", Message: "检测到未完成的运行状态，任务已自动恢复调度"})
				return nil
			})
			if err != nil {
				retryAt := now.Add(time.Second)
				if retryAt.Before(next) {
					next = retryAt
				}
				continue
			}
			task = recovered
		}
		if task.Status != domain.SchedulerTaskWaiting {
			continue
		}
		if task.NextRunAt.IsZero() || !task.NextRunAt.After(now) {
			due = append(due, task)
		} else if task.NextRunAt.Before(next) {
			next = task.NextRunAt
		}
	}
	if len(due) > 0 {
		return due, 100 * time.Millisecond
	}
	delay := time.Until(next)
	if delay < 100*time.Millisecond {
		delay = 100 * time.Millisecond
	}
	return nil, delay
}

func (s *Service) runAccountRound(ctx context.Context, accountID string) {
	task, err := s.store.UpdateSchedulerTask(accountID, func(task *domain.SchedulerTask) error {
		if task.Status != domain.SchedulerTaskWaiting {
			return errors.New("task not waiting")
		}
		task.Status, task.NextRunAt = domain.SchedulerTaskRunning, time.Time{}
		task.RoundIndex++
		task.LastRunAt = time.Now()
		appendTaskEvent(task, domain.SchedulerTaskEvent{Type: "round_started", Message: fmt.Sprintf("开始第 %d 轮创建", task.RoundIndex)})
		return nil
	})
	if err != nil {
		return
	}
	synced, syncErr := s.syncRemote(ctx, accountID)
	if syncErr != nil {
		s.waitAfterRoundError(accountID, fmt.Errorf("同步已有邮箱失败：%w", syncErr), "sync_failed")
		return
	}
	count := len(synced)
	_, _ = s.store.UpdateSchedulerTask(accountID, func(next *domain.SchedulerTask) error {
		next.Progress = count
		if count < next.Target {
			next.CompletionReason = ""
		}
		return nil
	})
	if count >= task.Target {
		s.waitAtCapacity(accountID, count, fmt.Sprintf("已达到 %d 个邮箱目标，保持巡检并等待空余名额", task.Target))
		return
	}
	for slot := 0; slot < task.RoundLimit && count < task.Target; slot++ {
		if ctx.Err() != nil || !s.taskRunning(accountID) {
			return
		}
		if slot > 0 && task.RoundIntervalSecond > 0 {
			if err := s.wait(ctx, time.Duration(task.RoundIntervalSecond)*time.Second); err != nil || !s.taskRunning(accountID) {
				return
			}
		}
		mailbox, createErr := s.mailbox.Create(ctx, accountID, task.Label, task.Note, task.CreateChannel)
		if createErr != nil {
			if isQuotaError(createErr) {
				resynced, resyncErr := s.syncRemote(ctx, accountID)
				if resyncErr == nil {
					count = len(resynced)
					_, _ = s.store.UpdateSchedulerTask(accountID, func(next *domain.SchedulerTask) error {
						next.Progress = count
						return nil
					})
				}
				if resyncErr == nil && count >= task.Target {
					s.waitAtCapacity(accountID, count, fmt.Sprintf("同步后已达到 %d 个邮箱目标，保持巡检并等待空余名额", task.Target))
					return
				}
			}
			s.waitAfterRoundError(accountID, createErr, "failed")
			return
		}
		count++
		_, _ = s.store.UpdateSchedulerTask(accountID, func(next *domain.SchedulerTask) error {
			next.Progress, next.Success, next.LastError = count, next.Success+1, ""
			if count < next.Target {
				next.CompletionReason = ""
			}
			appendTaskEvent(next, domain.SchedulerTaskEvent{Type: "created", Message: "已创建隐私邮箱 " + mailbox.Email, MailboxID: mailbox.ID, Email: mailbox.Email})
			return nil
		})
	}
	if count >= task.Target {
		s.waitAtCapacity(accountID, count, fmt.Sprintf("已达到 %d 个邮箱目标，保持巡检并等待空余名额", task.Target))
		return
	}
	_, _ = s.store.UpdateSchedulerTask(accountID, func(next *domain.SchedulerTask) error {
		if next.Status == domain.SchedulerTaskRunning {
			next.Status = domain.SchedulerTaskWaiting
			next.Progress = count
			next.NextRunAt = time.Now().Add(time.Duration(next.IntervalSeconds) * time.Second)
			appendTaskEvent(next, domain.SchedulerTaskEvent{Type: "round_completed", Message: fmt.Sprintf("本轮已完成，下轮最多创建 %d 个", next.RoundLimit)})
		}
		return nil
	})
}

func (s *Service) waitAfterRoundError(accountID string, roundErr error, eventType string) {
	_, _ = s.store.UpdateSchedulerTask(accountID, func(next *domain.SchedulerTask) error {
		next.Failed++
		next.LastError = roundErr.Error()
		if next.Status == domain.SchedulerTaskRunning {
			next.Status = domain.SchedulerTaskWaiting
			next.NextRunAt = time.Now().Add(time.Duration(next.IntervalSeconds) * time.Second)
		}
		appendTaskEvent(next, domain.SchedulerTaskEvent{Type: eventType, Message: "本轮未完成，等待下轮重试", Error: roundErr.Error()})
		return nil
	})
}

func (s *Service) waitAtCapacity(accountID string, count int, message string) {
	_, _ = s.store.UpdateSchedulerTask(accountID, func(task *domain.SchedulerTask) error {
		if task.Status != domain.SchedulerTaskRunning {
			return nil
		}
		task.Status, task.Progress, task.CompletionReason = domain.SchedulerTaskWaiting, count, "capacity_full"
		task.CompletedAt, task.StoppedAt = time.Time{}, time.Time{}
		task.LastError = ""
		task.NextRunAt = time.Now().Add(time.Duration(task.IntervalSeconds) * time.Second)
		appendTaskEvent(task, domain.SchedulerTaskEvent{Type: "capacity_full", Message: message})
		return nil
	})
}

func (s *Service) taskRunning(accountID string) bool {
	task, ok := s.store.SchedulerTask(accountID)
	return ok && task.Status == domain.SchedulerTaskRunning
}

func (s *Service) waitForSchedule(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		duration = 100 * time.Millisecond
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	s.mu.Lock()
	wake := s.wake
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}

func (s *Service) signalWake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake != nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) validateAccount(accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if _, ok := s.store.FindAppleAccount(accountID); !ok {
		return fmt.Errorf("Apple 账号不存在：%s", accountID)
	}
	if _, ok := s.store.ICloudSessionByAccountID(accountID); !ok {
		return fmt.Errorf("Apple 账号没有可用登录态：%s", accountID)
	}
	return nil
}

func (s *Service) mailboxCounts(accountIDs []string) map[string]int {
	counts, selected := make(map[string]int, len(accountIDs)), make(map[string]bool, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			counts[id] = 0
			selected[id] = true
		}
	}
	for _, mailbox := range s.store.AllMailboxes() {
		if selected[mailbox.AccountID] {
			counts[mailbox.AccountID]++
		}
	}
	return counts
}

func (s *Service) syncRemote(ctx context.Context, accountID string) ([]domain.Mailbox, error) {
	syncer, ok := s.mailbox.(remoteSyncer)
	if !ok {
		return nil, nil
	}
	return syncer.SyncRemote(ctx, accountID)
}

func isQuotaError(err error) bool {
	code, message, _ := protocol.ErrorDetails(err)
	if code == "apple_account_hme_limit" || code == "icloud_hme_limit" || strings.Contains(code, "rate") {
		return true
	}
	text := strings.ToLower(message)
	return strings.Contains(text, "创建上限") || strings.Contains(text, "reached the limit of addresses") || strings.Contains(text, "rate limit") || strings.Contains(text, "频率")
}

func appendTaskEvent(task *domain.SchedulerTask, event domain.SchedulerTaskEvent) {
	if event.At.IsZero() {
		event.At = time.Now()
	}
	var maxID int64
	for _, current := range task.Events {
		if current.ID > maxID {
			maxID = current.ID
		}
	}
	event.ID = maxID + 1
	task.Events = append(task.Events, event)
	if len(task.Events) > maxTaskEvents {
		task.Events = append([]domain.SchedulerTaskEvent(nil), task.Events[len(task.Events)-maxTaskEvents:]...)
	}
}

func isActiveTask(task domain.SchedulerTask) bool {
	return task.Status == domain.SchedulerTaskWaiting || task.Status == domain.SchedulerTaskRunning
}

func waitFor(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (s *Service) wait(ctx context.Context, duration time.Duration) error {
	if s.waitFn == nil {
		return waitFor(ctx, duration)
	}
	return s.waitFn(ctx, duration)
}

func (s *Service) normalizeConfig(cfg Config) Config {
	cfg.AccountIDs = normalizeAccountIDs(cfg.AccountIDs)
	cfg.Label = strings.TrimSpace(cfg.Label)
	if cfg.Label == "" {
		cfg.Label = "x"
	}
	cfg.Note = strings.TrimSpace(cfg.Note)
	cfg.CreateChannel = strings.ToLower(strings.TrimSpace(cfg.CreateChannel))
	if cfg.CreateChannel != "apple_account" && cfg.CreateChannel != "icloud_web" {
		cfg.CreateChannel = "auto"
	}
	if cfg.IntervalMinutes <= 0 && cfg.IntervalSeconds <= 0 {
		cfg.IntervalMinutes = 60
	}
	if cfg.RoundIntervalSecond <= 0 {
		cfg.RoundIntervalSecond = 5
	}
	if cfg.Target <= 0 {
		cfg.Target = defaultMailboxTargetPerAccount
	}
	if cfg.Target > defaultMailboxTargetPerAccount {
		cfg.Target = defaultMailboxTargetPerAccount
	}
	if cfg.RoundLimit <= 0 {
		cfg.RoundLimit = defaultRoundLimitPerAccount
	}
	if cfg.RoundLimit > cfg.Target {
		cfg.RoundLimit = cfg.Target
	}
	return cfg
}

func normalizeAccountIDs(accountIDs []string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}
func (s *Service) interval(cfg Config) int {
	if cfg.IntervalSeconds > 0 {
		return cfg.IntervalSeconds
	}
	return cfg.IntervalMinutes * 60
}

func DefaultConfig(state *store.Store) Config {
	settings := state.CreateSettings()
	accountIDs := append([]string(nil), settings.AccountIDs...)
	if len(accountIDs) == 0 {
		for _, account := range state.AppleAccounts() {
			if account.Status == domain.StatusActive {
				accountIDs = append(accountIDs, account.ID)
				break
			}
		}
	}
	return Config{AccountIDs: accountIDs, Label: settings.Label, Note: settings.Note, CreateChannel: settings.SchedulerCreateChannel, IntervalMinutes: settings.SchedulerIntervalMinutes, RoundIntervalSecond: settings.SchedulerRoundIntervalSeconds, RoundLimit: settings.SchedulerRoundLimit, Target: settings.SchedulerTarget}
}
