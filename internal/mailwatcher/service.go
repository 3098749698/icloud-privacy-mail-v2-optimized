package mailwatcher

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

const (
	mailWatcherSyncTimeout      = 90 * time.Second
	mailWatcherActiveTTL        = 20 * time.Minute
	mailWatcherFallbackInterval = 12 * time.Second
	mailWatcherRetryMinBackoff  = 30 * time.Second
	mailWatcherRetryMaxBackoff  = 15 * time.Minute
	mailWatcherIdleMaxBackoff   = 5 * time.Second
	mailWatcherReadySyncWindow  = 15 * time.Second
	mailWatcherIdleFetchMin     = 32
	mailWatcherIdleFetchMax     = 128
)

type Service struct {
	cfg           config.Config
	store         *store.Store
	mailbox       *mailboxservice.Service
	log           *slog.Logger
	wake          chan struct{}
	wakeMu        sync.Mutex
	wakeAll       bool
	wakeMailboxes map[string]struct{}
	activeMu      sync.Mutex
	activeUntil   map[string]time.Time
	statusMu      sync.RWMutex
	status        Status
	readyWorkers  map[string]bool
	workerTokens  map[string]uint64
	nextWorkerID  uint64
	retryMu       sync.Mutex
	retryAt       map[string]time.Time
	retryCount    map[string]int
}

// Status 是后台监听的真实运行快照，避免仅根据配置开关误报“运行中”。
type Status struct {
	Running              bool      `json:"running"`
	Enabled              bool      `json:"enabled"`
	GroupCount           int       `json:"group_count"`
	WorkerCount          int       `json:"worker_count"`
	ConnectedWorkerCount int       `json:"connected_worker_count"`
	SyncedMessages       int       `json:"synced_messages"`
	IdleEvents           int       `json:"idle_events"`
	StartedAt            time.Time `json:"started_at,omitempty"`
	LastCycleAt          time.Time `json:"last_cycle_at,omitempty"`
	LastSuccessAt        time.Time `json:"last_success_at,omitempty"`
	LastIdleConnectedAt  time.Time `json:"last_idle_connected_at,omitempty"`
	LastIdleEventAt      time.Time `json:"last_idle_event_at,omitempty"`
	LastErrorAt          time.Time `json:"last_error_at,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
	LastIdleErrorAt      time.Time `json:"last_idle_error_at,omitempty"`
	LastIdleError        string    `json:"last_idle_error,omitempty"`
}

type watchGroup struct {
	key       string
	session   domain.ICloudSession
	state     domain.LoginState
	mailboxes []domain.Mailbox
	signature string
}

type idleWorker struct {
	cancel    context.CancelFunc
	signature string
	token     uint64
}

type idleFolder string

const (
	idleFolderInbox idleFolder = "inbox"
	idleFolderJunk  idleFolder = "junk"
)

type idleWorkerTarget struct {
	key    string
	folder idleFolder
}

func NewService(cfg config.Config, state *store.Store, mailbox *mailboxservice.Service, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg:           cfg,
		store:         state,
		mailbox:       mailbox,
		log:           logger,
		wake:          make(chan struct{}, 1),
		wakeMailboxes: make(map[string]struct{}),
		activeUntil:   make(map[string]time.Time),
		readyWorkers:  make(map[string]bool),
		workerTokens:  make(map[string]uint64),
		retryAt:       make(map[string]time.Time),
		retryCount:    make(map[string]int),
	}
}

func (s *Service) Run(ctx context.Context) {
	interval := time.Duration(s.cfg.MailWatcherPollMS) * time.Millisecond
	if interval < time.Second {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	workers := make(map[string]idleWorker)
	defer stopIdleWorkers(workers)
	defer s.resetReadyWorkers()
	s.log.Info("后台邮件监听已启动", "状态检查间隔", interval, "模式", "IMAP IDLE 实时监听优先")
	s.updateStatus(func(status *Status) {
		status.Running = true
		status.StartedAt = time.Now()
	})
	defer s.updateStatus(func(status *Status) {
		status.Running = false
		status.WorkerCount = 0
		status.ConnectedWorkerCount = 0
	})

	started := false
	lastFallbackSync := time.Time{}
	cycle := func(forceSync bool, wakeAll bool, wakeMailboxes map[string]struct{}) {
		enabled := s.cfg.MailWatcherEnabled && s.store.Settings().EnableMailWatcher
		groups := s.groups()
		s.updateStatus(func(status *Status) {
			status.Enabled = enabled
			status.GroupCount = len(groups)
			status.LastCycleAt = time.Now()
		})
		if !enabled {
			if started {
				stopIdleWorkers(workers)
				started = false
			}
			s.resetReadyWorkers()
			s.updateStatus(func(status *Status) { status.WorkerCount = 0 })
			return
		}
		initial := !started
		syncGroups := groups
		shouldSync := initial
		if forceSync && !initial {
			if wakeAll || len(wakeMailboxes) == 0 {
				syncGroups = groups
			} else {
				syncGroups = groupsForMailboxes(groups, wakeMailboxes)
			}
			shouldSync = len(syncGroups) > 0
		}
		// QQ gets a dedicated Junk IDLE worker, but retain a low-frequency full
		// scan as protection against reconnect gaps and servers that omit EXISTS.
		// Other providers use fallback only while their INBOX worker is unready.
		if !shouldSync && (lastFallbackSync.IsZero() || time.Since(lastFallbackSync) >= mailWatcherFallbackInterval) {
			syncGroups = s.scheduledFallbackGroups(groups)
			shouldSync = len(syncGroups) > 0
		}
		if shouldSync {
			synced, syncErr := s.syncRound(ctx, syncGroups, initial)
			s.recordSyncResult(synced, syncErr)
			lastFallbackSync = time.Now()
		}
		if !started {
			started = true
		}
		s.ensureIdleWorkers(ctx, workers, groups)
		s.updateStatus(func(status *Status) { status.WorkerCount = len(workers) })
	}
	cycle(true, true, nil)
	for {
		select {
		case <-ctx.Done():
			s.log.Info("后台邮件监听已停止")
			return
		case <-s.wake:
			wakeAll, wakeMailboxes := s.consumeWake()
			cycle(true, wakeAll, wakeMailboxes)
		case <-ticker.C:
			cycle(false, false, nil)
		}
	}
}

// Wake 把主动取码的邮箱提升到本轮同步前面，并立即唤醒监听器。
func (s *Service) Wake(mailboxID string) {
	mailboxID = strings.TrimSpace(mailboxID)
	if mailboxID != "" {
		s.activeMu.Lock()
		s.activeUntil[mailboxID] = time.Now().Add(mailWatcherActiveTTL)
		s.activeMu.Unlock()
	}
	s.wakeMu.Lock()
	if s.wakeMailboxes == nil {
		s.wakeMailboxes = make(map[string]struct{})
	}
	if mailboxID == "" {
		s.wakeAll = true
		s.wakeMailboxes = make(map[string]struct{})
	} else if !s.wakeAll {
		s.wakeMailboxes[mailboxID] = struct{}{}
	}
	s.wakeMu.Unlock()
	s.clearAllSyncRetries()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) consumeWake() (bool, map[string]struct{}) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	wakeAll := s.wakeAll
	targets := make(map[string]struct{}, len(s.wakeMailboxes))
	for mailboxID := range s.wakeMailboxes {
		targets[mailboxID] = struct{}{}
	}
	s.wakeAll = false
	s.wakeMailboxes = make(map[string]struct{})
	return wakeAll, targets
}

func groupsForMailboxes(groups []watchGroup, mailboxIDs map[string]struct{}) []watchGroup {
	if len(mailboxIDs) == 0 {
		return nil
	}
	out := make([]watchGroup, 0, len(groups))
	for _, group := range groups {
		for _, mailbox := range group.mailboxes {
			if _, ok := mailboxIDs[mailbox.ID]; ok {
				out = append(out, group)
				break
			}
		}
	}
	return out
}

func (s *Service) unreadyGroups(groups []watchGroup) []watchGroup {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	out := make([]watchGroup, 0, len(groups))
	for _, group := range groups {
		if !s.readyWorkers[group.key] {
			out = append(out, group)
		}
	}
	return out
}

// scheduledFallbackGroups returns groups that need a periodic non-IDLE sync.
// QQ retains this safety net alongside its Junk IDLE worker; other groups are
// included only while their INBOX worker is not ready.
func (s *Service) scheduledFallbackGroups(groups []watchGroup) []watchGroup {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	out := make([]watchGroup, 0, len(groups))
	for _, group := range groups {
		if !s.readyWorkers[group.key] || groupNeedsFolderFallback(group) {
			out = append(out, group)
		}
	}
	return out
}

func groupNeedsFolderFallback(group watchGroup) bool {
	host := strings.TrimSuffix(strings.TrimSpace(group.state.IMAPHost), ".")
	return strings.EqualFold(host, "imap.qq.com")
}

func (s *Service) syncRound(ctx context.Context, groups []watchGroup, initial bool) (int, error) {
	if len(groups) == 0 {
		return 0, nil
	}
	after := time.Time{}
	limit := s.cfg.MailWatcherFetchLimit
	if initial {
		limit = s.cfg.MailWatcherInitialFetchLimit
		if s.cfg.MailWatcherLookbackHours > 0 {
			after = time.Now().Add(-time.Duration(s.cfg.MailWatcherLookbackHours) * time.Hour)
		}
	}
	total := 0
	var lastErr error
	for _, group := range groups {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		if !s.syncRetryReady(group.key, time.Now()) {
			continue
		}
		syncCtx, cancel := context.WithTimeout(ctx, mailWatcherSyncTimeout)
		count, err := s.mailbox.SyncMailboxBatch(syncCtx, group.mailboxes, after, "OpenAI", limit)
		cancel()
		total += count
		if err != nil && ctx.Err() == nil {
			retryAfter := s.recordSyncRetryFailure(group.key, time.Now())
			lastErr = err
			s.log.Warn("后台批量同步邮箱失败", "账号", group.session.AppleID, "邮箱数", len(group.mailboxes), "首次同步", initial, "下次重试", retryAfter, "错误", err)
		} else if err == nil {
			s.clearSyncRetry(group.key)
		}
	}
	return total, lastErr
}

func (s *Service) ensureIdleWorkers(ctx context.Context, workers map[string]idleWorker, groups []watchGroup) {
	seen := make(map[string]bool, len(groups))
	for _, group := range groups {
		for _, target := range idleWorkerTargets(group) {
			seen[target.key] = true
			if worker, ok := workers[target.key]; ok && worker.signature == group.signature {
				continue
			}
			if worker, ok := workers[target.key]; ok {
				worker.cancel()
				s.unregisterIdleWorker(target.key, worker.token)
				delete(workers, target.key)
			}
			workerCtx, cancel := context.WithCancel(ctx)
			token := s.registerIdleWorker(target.key)
			workers[target.key] = idleWorker{cancel: cancel, signature: group.signature, token: token}
			go s.runIdleWorker(workerCtx, group, target, token)
		}
	}
	for key, worker := range workers {
		if seen[key] {
			continue
		}
		worker.cancel()
		s.unregisterIdleWorker(key, worker.token)
		delete(workers, key)
	}
}

func idleWorkerTargets(group watchGroup) []idleWorkerTarget {
	targets := []idleWorkerTarget{{key: group.key, folder: idleFolderInbox}}
	if groupNeedsFolderFallback(group) {
		targets = append(targets, idleWorkerTarget{key: group.key + "#junk", folder: idleFolderJunk})
	}
	return targets
}

func (s *Service) runIdleWorker(ctx context.Context, group watchGroup, target idleWorkerTarget, token uint64) {
	workerCtx, cancelWorker := context.WithCancel(ctx)
	syncRequests := make(chan struct{}, 1)
	readySignals := make(chan struct{}, 1)
	groupKey := group.key
	appleID := group.session.AppleID
	var syncWorker sync.WaitGroup
	syncWorker.Add(2)
	go func() {
		defer syncWorker.Done()
		runCoalescedSyncLoop(workerCtx, syncRequests, func(syncCtx context.Context) {
			s.syncIdleGroupFolder(syncCtx, groupKey, appleID, target.folder)
		})
	}()
	go func() {
		defer syncWorker.Done()
		runIdleReadySyncLoop(workerCtx, readySignals, mailWatcherReadySyncWindow, func() {
			requestIdleSync(syncRequests)
		})
	}()
	defer func() {
		cancelWorker()
		syncWorker.Wait()
	}()

	backoff := time.Second
	for workerCtx.Err() == nil {
		prepared, prepareErr := protocol.PrepareIMAPLoginState(workerCtx, group.state)
		if prepareErr != nil {
			s.recordWatcherError(prepareErr)
			s.log.Warn("IMAP 授权准备失败", "账号", group.session.AppleID, "错误", prepareErr)
		} else {
			// PrepareIMAPLoginState normalizes password states and refreshes OAuth
			// only when needed. Avoid a state-file write on every reconnect when
			// the prepared state is byte-for-byte unchanged.
			if !reflect.DeepEqual(group.state, prepared) {
				updated, err := s.store.UpdateICloudSession(group.session.AccountID, func(current *domain.ICloudSession) error {
					*current = protocol.WithLoginState(*current, prepared)
					return nil
				})
				if err != nil {
					s.log.Warn("保存 IMAP OAuth2 Token 失败", "账号", group.session.AppleID, "错误", err)
				} else {
					// Keep the worker's cached copy aligned without replacing Apple
					// login states that another background worker just refreshed.
					group.session = updated
				}
			}
			group.state = prepared
		}
		if prepareErr != nil {
			timer := time.NewTimer(backoff)
			select {
			case <-workerCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < mailWatcherIdleMaxBackoff {
				backoff *= 2
				if backoff > mailWatcherIdleMaxBackoff {
					backoff = mailWatcherIdleMaxBackoff
				}
			}
			continue
		}
		watch := protocol.WatchICloudIMAPExists
		if target.folder == idleFolderJunk {
			watch = protocol.WatchICloudIMAPJunkExists
		}
		err := watch(workerCtx, group.state, func() {
			s.markWorkerReady(target.key, token, true)
			s.updateStatus(func(status *Status) {
				status.LastIdleConnectedAt = time.Now()
				status.LastIdleError = ""
			})
			// SELECT may observe mail that arrived before the server enters IDLE.
			// INBOX queues a gap sync. Startup already scans Junk before workers
			// launch, and the 12-second fallback covers a Junk reconnect gap without
			// doubling every normal startup sync.
			if target.folder == idleFolderInbox {
				requestIdleSync(readySignals)
			}
		}, func() {
			if workerCtx.Err() != nil {
				return
			}
			s.updateStatus(func(status *Status) {
				status.IdleEvents++
				status.LastIdleEventAt = time.Now()
			})
			// Return immediately so the IMAP connection re-enters IDLE while the
			// per-worker sync loop fetches mail on its own connection. A buffered
			// request preserves one trailing sync when EXISTS arrives mid-sync.
			requestIdleSync(syncRequests)
		})
		s.markWorkerReady(target.key, token, false)
		if workerCtx.Err() != nil {
			return
		}
		if err == nil {
			// 收到 EXISTS 后连接会退出当前 IDLE，立即重新建立监听，不进入故障退避。
			backoff = time.Second
			continue
		}
		s.recordWatcherError(err)
		s.log.Warn("IMAP IDLE 已断开，准备重连", "账号", group.session.AppleID, "目录", target.folder, "邮箱数", len(group.mailboxes), "等待", backoff, "错误", err)
		timer := time.NewTimer(backoff)
		select {
		case <-workerCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < mailWatcherIdleMaxBackoff {
			backoff *= 2
			if backoff > mailWatcherIdleMaxBackoff {
				backoff = mailWatcherIdleMaxBackoff
			}
		}
	}
}

// runCoalescedSyncLoop serializes a worker's syncs. The one-slot queue folds a
// burst of duplicate EXISTS events together while retaining one event that
// arrives during an in-flight sync, so the worker always performs a trailing
// pass before going quiet again.
func runCoalescedSyncLoop(ctx context.Context, requests <-chan struct{}, syncFn func(context.Context)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-requests:
			if ctx.Err() != nil {
				return
			}
			syncFn(ctx)
		}
	}
}

type idleReadySyncGate struct {
	nextAllowed time.Time
	pending     bool
}

func (g *idleReadySyncGate) ready(now time.Time, interval time.Duration) (bool, time.Duration) {
	if interval <= 0 || g.nextAllowed.IsZero() || !now.Before(g.nextAllowed) {
		g.nextAllowed = now.Add(interval)
		g.pending = false
		return true, 0
	}
	g.pending = true
	return false, g.nextAllowed.Sub(now)
}

func (g *idleReadySyncGate) due(now time.Time, interval time.Duration) (bool, time.Duration) {
	if !g.pending {
		return false, 0
	}
	if now.Before(g.nextAllowed) {
		return false, g.nextAllowed.Sub(now)
	}
	g.pending = false
	g.nextAllowed = now.Add(interval)
	return true, 0
}

// runIdleReadySyncLoop performs the first reconnect gap sync immediately, then
// throttles a flapping connection to one sync per window while retaining one
// trailing request. EXISTS events bypass this gate and still wake directly.
func runIdleReadySyncLoop(ctx context.Context, ready <-chan struct{}, interval time.Duration, request func()) {
	var gate idleReadySyncGate
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		timerC = nil
	}
	resetTimer := func(delay time.Duration) {
		stopTimer()
		if delay < 0 {
			delay = 0
		}
		timer = time.NewTimer(delay)
		timerC = timer.C
	}
	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ready:
			fire, wait := gate.ready(time.Now(), interval)
			if fire {
				stopTimer()
				request()
				continue
			}
			resetTimer(wait)
		case now := <-timerC:
			timer = nil
			timerC = nil
			fire, wait := gate.due(now, interval)
			if fire {
				request()
			}
			if wait > 0 {
				resetTimer(wait)
			}
		}
	}
}

func requestIdleSync(requests chan<- struct{}) {
	select {
	case requests <- struct{}{}:
	default:
	}
}

func (s *Service) syncIdleGroup(ctx context.Context, groupKey, appleID string) {
	s.syncIdleGroupFolder(ctx, groupKey, appleID, "")
}

func (s *Service) syncIdleGroupFolder(ctx context.Context, groupKey, appleID string, folder idleFolder) {
	currentGroup, ok := s.groupByKey(groupKey)
	if !ok || ctx.Err() != nil {
		return
	}
	syncCtx, cancel := context.WithTimeout(ctx, mailWatcherSyncTimeout)
	var count int
	var syncErr error
	if folder == "" {
		count, syncErr = s.mailbox.SyncMailboxBatchFresh(syncCtx, currentGroup.mailboxes, time.Time{}, "OpenAI", mailWatcherIdleFetchLimit(s.cfg.MailWatcherFetchLimit))
	} else {
		count, syncErr = s.mailbox.SyncMailboxBatchFolderFresh(syncCtx, currentGroup.mailboxes, time.Time{}, "OpenAI", mailWatcherIdleFetchLimit(s.cfg.MailWatcherFetchLimit), string(folder))
	}
	cancel()
	if syncErr != nil && ctx.Err() != nil {
		return
	}
	s.recordSyncResult(count, syncErr)
	if syncErr != nil {
		s.log.Warn("IMAP IDLE 异步同步失败", "账号", appleID, "目录", folder, "邮箱数", len(currentGroup.mailboxes), "错误", syncErr)
	}
}

func mailWatcherIdleFetchLimit(configured int) int {
	if configured < mailWatcherIdleFetchMin {
		return mailWatcherIdleFetchMin
	}
	if configured > mailWatcherIdleFetchMax {
		return mailWatcherIdleFetchMax
	}
	return configured
}

func (s *Service) syncRetryReady(key string, now time.Time) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return !now.Before(s.retryAt[key])
}

func (s *Service) recordSyncRetryFailure(key string, now time.Time) time.Time {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.retryCount[key]++
	delay := mailWatcherSyncRetryDelay(s.retryCount[key])
	s.retryAt[key] = now.Add(delay)
	return s.retryAt[key]
}

func (s *Service) clearSyncRetry(key string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	delete(s.retryAt, key)
	delete(s.retryCount, key)
}

func (s *Service) clearAllSyncRetries() {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.retryAt = make(map[string]time.Time)
	s.retryCount = make(map[string]int)
}

func mailWatcherSyncRetryDelay(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	delay := mailWatcherRetryMinBackoff
	for attempt := 1; attempt < failures && delay < mailWatcherRetryMaxBackoff; attempt++ {
		delay *= 2
	}
	if delay > mailWatcherRetryMaxBackoff {
		return mailWatcherRetryMaxBackoff
	}
	return delay
}

// Snapshot 返回并发安全的后台监听运行快照。
func (s *Service) Snapshot() Status {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.status
}

func (s *Service) updateStatus(update func(*Status)) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	update(&s.status)
}

func (s *Service) recordSyncResult(count int, err error) {
	now := time.Now()
	s.updateStatus(func(status *Status) {
		if count > 0 {
			status.SyncedMessages += count
		}
		if err != nil {
			status.LastError = err.Error()
			status.LastErrorAt = now
			return
		}
		status.LastSuccessAt = now
		status.LastError = ""
	})
}

func (s *Service) recordWatcherError(err error) {
	if err == nil {
		return
	}
	s.updateStatus(func(status *Status) {
		status.LastIdleError = err.Error()
		status.LastIdleErrorAt = time.Now()
	})
}

func (s *Service) registerIdleWorker(key string) uint64 {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.workerTokens == nil {
		s.workerTokens = make(map[string]uint64)
	}
	if s.readyWorkers == nil {
		s.readyWorkers = make(map[string]bool)
	}
	s.nextWorkerID++
	if s.nextWorkerID == 0 {
		s.nextWorkerID++
	}
	token := s.nextWorkerID
	s.workerTokens[key] = token
	delete(s.readyWorkers, key)
	s.status.ConnectedWorkerCount = len(s.readyWorkers)
	return token
}

func (s *Service) unregisterIdleWorker(key string, token uint64) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.workerTokens[key] != token {
		return
	}
	delete(s.workerTokens, key)
	delete(s.readyWorkers, key)
	s.status.ConnectedWorkerCount = len(s.readyWorkers)
}

func (s *Service) markWorkerReady(key string, token uint64, ready bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.workerTokens[key] != token {
		return
	}
	if ready {
		s.readyWorkers[key] = true
	} else {
		delete(s.readyWorkers, key)
	}
	s.status.ConnectedWorkerCount = len(s.readyWorkers)
}

func (s *Service) resetReadyWorkers() {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.readyWorkers = make(map[string]bool)
	s.workerTokens = make(map[string]uint64)
	s.status.ConnectedWorkerCount = 0
}

func (s *Service) groups() []watchGroup {
	active := s.activeMailboxIDs(time.Now())
	type bucket struct {
		session   domain.ICloudSession
		state     domain.LoginState
		mailboxes []domain.Mailbox
	}
	buckets := make(map[string]*bucket)
	for _, mailbox := range s.store.AllMailboxes() {
		if !mailbox.APIActive || !mailbox.ICloudActive || mailbox.Status == domain.StatusDisabled {
			continue
		}
		session, ok := s.store.ICloudSessionByAccountID(mailbox.AccountID)
		if !ok {
			continue
		}
		imapState, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP)
		if !ok || !imapStateConfigured(imapState) {
			continue
		}
		key := firstNonEmpty(session.AccountID, mailbox.AccountID, mailbox.OwnerID, "__imap__")
		item := buckets[key]
		if item == nil {
			item = &bucket{session: session, state: imapState}
			buckets[key] = item
		}
		item.mailboxes = append(item.mailboxes, mailbox)
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	groups := make([]watchGroup, 0, len(keys))
	for _, key := range keys {
		item := buckets[key]
		sort.Slice(item.mailboxes, func(i, j int) bool {
			leftActive := active[item.mailboxes[i].ID]
			rightActive := active[item.mailboxes[j].ID]
			if leftActive != rightActive {
				return leftActive
			}
			return item.mailboxes[i].Email < item.mailboxes[j].Email
		})
		groups = append(groups, watchGroup{
			key:       key,
			session:   item.session,
			state:     item.state,
			mailboxes: item.mailboxes,
			signature: groupSignature(item.state),
		})
	}
	return groups
}

func (s *Service) activeMailboxIDs(now time.Time) map[string]bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	active := make(map[string]bool)
	for mailboxID, until := range s.activeUntil {
		if until.After(now) {
			active[mailboxID] = true
			continue
		}
		delete(s.activeUntil, mailboxID)
	}
	return active
}

func groupSignature(state domain.LoginState) string {
	parts := []string{state.IMAPEmail, state.IMAPUsername, state.IMAPHost, fmt.Sprint(state.IMAPPort), state.IMAPAuthMethod, state.IMAPAppPassword, state.OAuthClientID, state.OAuthAccessToken, state.OAuthRefreshToken}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "|"))))
}

func (s *Service) groupByKey(key string) (watchGroup, bool) {
	for _, group := range s.groups() {
		if group.key == key {
			return group, true
		}
	}
	return watchGroup{}, false
}

func (s *Service) allWorkersReady(groups []watchGroup) bool {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	for _, group := range groups {
		if !s.readyWorkers[group.key] {
			return false
		}
	}
	return true
}

func imapStateConfigured(state domain.LoginState) bool {
	if strings.EqualFold(state.IMAPHost, "outlook.office365.com") || strings.EqualFold(state.IMAPAuthMethod, "xoauth2") {
		return strings.TrimSpace(state.IMAPEmail) != "" && strings.TrimSpace(state.OAuthClientID) != "" && strings.TrimSpace(state.OAuthRefreshToken) != ""
	}
	return strings.TrimSpace(state.IMAPAppPassword) != ""
}

func stopIdleWorkers(workers map[string]idleWorker) {
	for key, worker := range workers {
		worker.cancel()
		delete(workers, key)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
