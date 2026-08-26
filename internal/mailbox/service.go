package mailbox

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

type Service struct {
	cfg                config.Config
	store              *store.Store
	client             *protocol.ICloudClient
	deleteClient       remoteMailboxDeleteClient
	deleteLocal        func(string) error
	onCapacityReleased func(string)
	syncRunner         syncGroupRunner
	syncFolderRunner   syncGroupFolderRunner
	createMu           sync.Mutex
	syncMu             sync.Mutex
	syncs              map[string]*syncCall
}

type syncGroupRunner func(context.Context, []domain.Mailbox, time.Time, string, int) (int, error)
type syncGroupFolderRunner func(context.Context, []domain.Mailbox, time.Time, string, int, string) (int, error)

type remoteMailboxDeleteClient interface {
	ListPrivacyMailboxes(context.Context, protocol.ICloudSession) ([]protocol.ICloudRemoteMailbox, error)
	DeletePrivacyMailbox(context.Context, protocol.ICloudSession, string) error
	MoveRemoteMessagesToTrash(context.Context, protocol.ICloudSession, []string) (protocol.ICloudMailCleanupResult, error)
	EmptyTrash(context.Context, protocol.ICloudSession) (int, error)
}

type appleAccountMailboxDeleteClient interface {
	ListPrivacyMailboxesWithAppleAccount(context.Context, protocol.ICloudSession, string) ([]protocol.ICloudRemoteMailbox, protocol.ICloudSession, error)
	DeletePrivacyMailboxWithAppleAccount(context.Context, protocol.ICloudSession, string, string, bool) (protocol.ICloudSession, error)
}

const (
	mailboxCodeFreshWindow = 5 * time.Minute
	// A shared sync must outlive an individual HTTP request. Otherwise a
	// client timeout cancels the IMAP fetch for every other waiter as well.
	mailboxSyncSharedTimeout = 90 * time.Second
	autoDeleteClaimDuration  = 2 * time.Minute
	autoDeleteCallTimeout    = 45 * time.Second
)

type syncCall struct {
	done      chan struct{}
	startedAt time.Time
	folder    string
	count     int
	err       error
}

type CodeResult struct {
	Email           string    `json:"email"`
	Code            string    `json:"code"`
	Subject         string    `json:"subject"`
	From            string    `json:"from"`
	ReceivedAt      time.Time `json:"received_at"`
	DetectedAt      time.Time `json:"detected_at"`
	DeliveryDelayMS int64     `json:"delivery_delay_ms"`
	MessageID       string    `json:"message_id"`
	Folder          string    `json:"folder"`
}

type CodeQuery struct {
	After         time.Time
	Keyword       string
	SkipMessageID string
	IncludeServed bool
	MarkAsServed  bool
	AllowStale    bool
}

type RemoteCleanupOptions struct {
	AccountID  string `json:"account_id,omitempty"`
	MoveSynced bool   `json:"move_synced"`
	EmptyTrash bool   `json:"empty_trash"`
}

type RemoteCleanupFailure struct {
	MailboxID string `json:"mailbox_id"`
	Email     string `json:"email"`
	Error     string `json:"error"`
}

type RemoteCleanupBatchResult struct {
	Cleanup         protocol.ICloudMailCleanupResult `json:"cleanup"`
	Mailboxes       int                              `json:"mailboxes"`
	FailedMailboxes int                              `json:"failed_mailboxes"`
	Failures        []RemoteCleanupFailure           `json:"failures,omitempty"`
}

type AutoDeleteFailure struct {
	MailboxID string    `json:"mailbox_id"`
	Email     string    `json:"email"`
	Error     string    `json:"error"`
	RetryAt   time.Time `json:"retry_at"`
}

type AutoDeleteBatchResult struct {
	Claimed         int                 `json:"claimed"`
	Deleted         int                 `json:"deleted"`
	QuotaReleased   int                 `json:"quota_released"`
	CleanupPending  int                 `json:"cleanup_pending"`
	Failed          int                 `json:"failed"`
	Failures        []AutoDeleteFailure `json:"failures,omitempty"`
	CleanupFailures []AutoDeleteFailure `json:"cleanup_failures,omitempty"`
}

type quotaReleasedCleanupError struct {
	releasedAt time.Time
	err        error
}

func (e *quotaReleasedCleanupError) Error() string { return e.err.Error() }
func (e *quotaReleasedCleanupError) Unwrap() error { return e.err }

func NewService(cfg config.Config, state *store.Store) *Service {
	client := protocol.NewICloudClient()
	return &Service{cfg: cfg, store: state, client: client, deleteClient: client, deleteLocal: state.DeleteMailbox, syncs: make(map[string]*syncCall)}
}

// SetCapacityReleasedHandler installs the scheduler notification used after an
// Apple alias and its local record have both been removed successfully.
func (s *Service) SetCapacityReleasedHandler(handler func(string)) {
	s.onCapacityReleased = handler
}

// ImportLocal 把已有隐私邮箱绑定到 Apple 账号，不调用 Apple 创建接口。
func (s *Service) ImportLocal(accountID, email, label, note string) (domain.Mailbox, bool, error) {
	accountID = strings.TrimSpace(accountID)
	if _, ok := s.store.FindAppleAccount(accountID); !ok {
		return domain.Mailbox{}, false, errors.New("Apple 账号不存在")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(strings.TrimSpace(address.Address), email) {
		return domain.Mailbox{}, false, errors.New("邮箱地址格式不正确")
	}
	mailbox, created, err := s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Email: email, Label: strings.TrimSpace(label), IsActive: true, Origin: "manual",
	}, firstNonEmpty(note, "手动导入本地邮箱"))
	return mailbox, created, err
}

func (s *Service) Create(ctx context.Context, accountID, label, note, channel string) (domain.Mailbox, error) {
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		return domain.Mailbox{}, errors.New("Apple 账号登录态不存在")
	}
	s.createMu.Lock()
	defer s.createMu.Unlock()
	label = s.store.NextMailboxLabel(label)
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		channel = "auto"
	}
	var remote protocol.ICloudRemoteMailbox
	var err error
	var appleAccountErr error
	appleAccountTried := false
	if channel == "auto" || channel == "apple_account" {
		if _, saved := protocol.LoginStateForKind(session, domain.LoginStateAppleAccount); saved {
			appleAccountTried = true
			var updated domain.ICloudSession
			remote, updated, err = s.client.CreatePrivacyMailboxWithAppleAccount(ctx, session, s.cfg.AppleAccountAPIKey, label, note)
			if err == nil {
				_, _ = s.store.SaveICloudSession(updated)
			}
			appleAccountErr = err
		} else if channel == "apple_account" {
			err = errors.New("该账号没有 Apple Account 新接口登录态")
		}
	}
	shouldTryICloudWeb := channel == "icloud_web" || (channel == "auto" && hasICloudWebCreateState(session))
	if remote.Email == "" && shouldTryICloudWeb {
		var webErr error
		remote, webErr = s.client.CreatePrivacyMailbox(ctx, session, label, note)
		if webErr != nil && appleAccountErr != nil {
			err = fmt.Errorf("Apple Account 新接口创建失败：%v；iCloud Web 回退失败：%w", appleAccountErr, webErr)
		} else {
			err = webErr
		}
	}
	if err != nil {
		return domain.Mailbox{}, err
	}
	if remote.Email == "" {
		if appleAccountTried {
			return domain.Mailbox{}, errors.New("Apple Account 新接口创建未返回隐私邮箱")
		}
		return domain.Mailbox{}, errors.New("该账号没有可用于创建的 Apple Account 新接口或 iCloud Web 登录态")
	}
	mailbox, _, err := s.store.UpsertMailboxFromRemote(accountID, remoteMailbox(remote), note)
	return mailbox, err
}

func hasICloudWebCreateState(session domain.ICloudSession) bool {
	if _, saved := protocol.LoginStateForKind(session, domain.LoginStateICloudWeb); saved {
		return true
	}
	return strings.TrimSpace(session.PremiumMailBaseURL) != "" &&
		strings.TrimSpace(session.DSID) != "" && len(session.Cookies) > 0
}

func (s *Service) SyncRemote(ctx context.Context, accountID string) ([]domain.Mailbox, error) {
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		return nil, errors.New("Apple 账号登录态不存在")
	}
	var appleAccountErr error
	if _, saved := protocol.LoginStateForKind(session, domain.LoginStateAppleAccount); saved {
		remotes, updated, err := s.client.ListPrivacyMailboxesWithAppleAccount(ctx, session, s.cfg.AppleAccountAPIKey)
		if _, saveErr := s.store.SaveICloudSession(updated); saveErr != nil && err == nil {
			return nil, saveErr
		}
		if err == nil {
			return s.saveRemoteMailboxes(accountID, remotes)
		}
		appleAccountErr = err
	}

	remotes, err := s.client.ListPrivacyMailboxes(ctx, session)
	if err == nil {
		return s.saveRemoteMailboxes(accountID, remotes)
	}
	if appleAccountErr != nil {
		return nil, fmt.Errorf("Apple Account 同步失败：%v；iCloud Web 回退失败：%w", appleAccountErr, err)
	}
	return nil, err
}

func (s *Service) saveRemoteMailboxes(accountID string, remotes []protocol.ICloudRemoteMailbox) ([]domain.Mailbox, error) {
	out := make([]domain.Mailbox, 0, len(remotes))
	for _, remote := range remotes {
		mailbox, _, saveErr := s.store.UpsertMailboxFromRemote(accountID, remoteMailbox(remote), "从 Apple 服务器同步")
		if saveErr != nil {
			return out, saveErr
		}
		out = append(out, mailbox)
	}
	return out, nil
}

func (s *Service) DeleteRemote(ctx context.Context, mailboxID string) error {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return errors.New("邮箱不存在")
	}
	session, hasSession := s.store.ICloudSessionByAccountID(mailbox.AccountID)
	client := s.deleteClient
	if client == nil {
		client = s.client
	}
	if mailbox.QuotaReleasedAt.IsZero() {
		if !hasSession {
			return errors.New("对应 Apple 账号登录态不存在")
		}
		appleClient, supportsAppleClient := client.(appleAccountMailboxDeleteClient)
		useAppleAccount := supportsAppleClient && protocol.SupportsAppleAccountMailboxManagement(session, s.cfg.AppleAccountAPIKey)
		listRemote := func() ([]protocol.ICloudRemoteMailbox, error) {
			if !useAppleAccount {
				return client.ListPrivacyMailboxes(ctx, session)
			}
			remotes, updated, operationErr := appleClient.ListPrivacyMailboxesWithAppleAccount(ctx, session, s.cfg.AppleAccountAPIKey)
			var saveErr error
			session, saveErr = s.store.SaveICloudSession(updated)
			if saveErr != nil {
				saveErr = fmt.Errorf("保存 Apple Account 删除登录态失败：%w", saveErr)
				if operationErr != nil {
					return nil, errors.Join(operationErr, saveErr)
				}
				return nil, saveErr
			}
			return remotes, operationErr
		}

		remotes, err := listRemote()
		if err != nil && useAppleAccount && protocol.SupportsICloudWebMailboxManagement(session) {
			useAppleAccount = false
			remotes, err = listRemote()
		}
		if err != nil {
			return err
		}
		remote, exists := matchRemote(remotes, mailbox.AnonymousID, mailbox.Email)
		if exists {
			if useAppleAccount {
				updated, operationErr := appleClient.DeletePrivacyMailboxWithAppleAccount(ctx, session, s.cfg.AppleAccountAPIKey, remote.AnonymousID, remote.IsActive)
				var saveErr error
				session, saveErr = s.store.SaveICloudSession(updated)
				if saveErr != nil {
					saveErr = fmt.Errorf("保存 Apple Account 删除登录态失败：%w", saveErr)
					if operationErr != nil {
						return errors.Join(operationErr, saveErr)
					}
					return saveErr
				}
				if operationErr != nil {
					return operationErr
				}
			} else if err := client.DeletePrivacyMailbox(ctx, session, remote.AnonymousID); err != nil {
				return err
			}
		}
		confirmed, err := listRemote()
		if err != nil {
			return fmt.Errorf("Apple 删除后确认失败：%w", err)
		}
		if _, stillExists := matchRemote(confirmed, firstNonEmpty(remote.AnonymousID, mailbox.AnonymousID), mailbox.Email); stillExists {
			return errors.New("Apple 服务器仍然存在该隐私邮箱，本地记录已保留")
		}
		releasedAt := time.Now()
		mailbox, err = s.store.MarkMailboxQuotaReleased(mailboxID, releasedAt)
		if err != nil {
			return &quotaReleasedCleanupError{
				releasedAt: releasedAt,
				err:        fmt.Errorf("Apple 配额已确认释放，但保存本地释放状态失败：%w", err),
			}
		}
	}

	var cleanupErr error
	if hasSession {
		cleanupErr = s.cleanupMailboxMessagesAfterDelete(ctx, mailboxID, session)
	} else {
		cleanupErr = errors.New("Apple 配额已释放，但登录态不存在，已跳过远端邮件移动")
	}
	deleteLocal := s.deleteLocal
	if deleteLocal == nil {
		deleteLocal = s.store.DeleteMailbox
	}
	if err := deleteLocal(mailboxID); err != nil {
		if cleanupErr != nil {
			err = errors.Join(cleanupErr, err)
		}
		return &quotaReleasedCleanupError{
			releasedAt: mailbox.QuotaReleasedAt,
			err:        fmt.Errorf("Apple 配额已释放，但删除本地邮箱记录失败：%w", err),
		}
	}
	if cleanupErr != nil {
		_ = s.store.RecordEvent("warning", "mailbox_auto_delete", fmt.Sprintf("Apple 配额已释放并删除本地邮箱 %s；远端邮件移动未完成：%v", mailbox.Email, cleanupErr))
	}
	if s.onCapacityReleased != nil {
		s.onCapacityReleased(mailbox.AccountID)
	}
	return nil
}

// ValidateAutoDeleteAccount ensures an expiring export can later delete the
// Apple-side alias and actually release one of the account's 750 HME slots.
func (s *Service) ValidateAutoDeleteAccount(accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("限时导出必须选择一个 Apple 账号")
	}
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		return errors.New("Apple 账号登录态不存在")
	}
	if !protocol.SupportsAppleAccountMailboxManagement(session, s.cfg.AppleAccountAPIKey) &&
		!protocol.SupportsICloudWebMailboxManagement(session) {
		return errors.New("该账号缺少可用的 Apple 远端删除登录态，请重新完成协议登录后再使用限时导出")
	}
	return nil
}

// DeleteExpiredMailboxes processes at most limit due exports. Records are
// claimed one at a time so a restart only delays the in-flight deletion; all
// remaining expired records are immediately available to the new process.
func (s *Service) DeleteExpiredMailboxes(ctx context.Context, now time.Time, limit int) (AutoDeleteBatchResult, error) {
	result := AutoDeleteBatchResult{}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	if limit <= 0 {
		return result, errors.New("回收数量必须大于 0")
	}
	for result.Claimed < limit {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		claimed, err := s.store.ClaimExpiredMailboxes(now, 1, autoDeleteClaimDuration)
		if err != nil {
			return result, err
		}
		if len(claimed) == 0 {
			break
		}
		mailbox := claimed[0]
		wasReleased := !mailbox.QuotaReleasedAt.IsZero()
		result.Claimed++

		deleteCtx, cancel := context.WithTimeout(ctx, autoDeleteCallTimeout)
		deleteErr := s.DeleteRemote(deleteCtx, mailbox.ID)
		cancel()
		if deleteErr == nil {
			result.Deleted++
			if !wasReleased {
				result.QuotaReleased++
			}
			continue
		}
		var cleanupErr *quotaReleasedCleanupError
		errors.As(deleteErr, &cleanupErr)
		// A concurrent manual deletion has already achieved the desired state.
		current, exists := s.store.FindMailboxByID(mailbox.ID)
		if !exists {
			result.Deleted++
			if !wasReleased && cleanupErr != nil {
				result.QuotaReleased++
			}
			continue
		}

		attemptedAt := time.Now()
		retryAt := attemptedAt.Add(autoDeleteRetryDelay(mailbox.DeleteAttempts))
		releasedAt := current.QuotaReleasedAt
		if cleanupErr != nil && releasedAt.IsZero() {
			releasedAt = cleanupErr.releasedAt
		}
		if !releasedAt.IsZero() {
			if _, err := s.store.RecordMailboxCleanupFailure(mailbox.ID, releasedAt, attemptedAt, retryAt, deleteErr.Error()); err != nil {
				return result, fmt.Errorf("保存到期邮箱 %s 的本地清理重试状态失败：%w", mailbox.Email, err)
			}
			if !wasReleased {
				result.QuotaReleased++
			}
			result.CleanupPending++
			result.CleanupFailures = append(result.CleanupFailures, AutoDeleteFailure{
				MailboxID: mailbox.ID,
				Email:     mailbox.Email,
				Error:     deleteErr.Error(),
				RetryAt:   retryAt,
			})
			continue
		}
		if _, err := s.store.RecordMailboxAutoDeleteFailure(mailbox.ID, attemptedAt, retryAt, deleteErr.Error()); err != nil {
			return result, fmt.Errorf("保存到期邮箱 %s 的删除重试状态失败：%w", mailbox.Email, err)
		}
		result.Failed++
		result.Failures = append(result.Failures, AutoDeleteFailure{
			MailboxID: mailbox.ID,
			Email:     mailbox.Email,
			Error:     deleteErr.Error(),
			RetryAt:   retryAt,
		})
	}
	return result, nil
}

func autoDeleteRetryDelay(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return time.Minute
	case attempt == 2:
		return 5 * time.Minute
	case attempt == 3:
		return 15 * time.Minute
	case attempt == 4:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}

func (s *Service) DeleteLocal(mailboxID string) error {
	if _, err := s.store.DeleteMailboxMessages(mailboxID); err != nil {
		return fmt.Errorf("删除邮箱前清理本地邮件失败：%w", err)
	}
	return s.store.DeleteMailbox(mailboxID)
}

func (s *Service) cleanupMailboxMessagesAfterDelete(ctx context.Context, mailboxID string, session protocol.ICloudSession) error {
	client := s.deleteClient
	if client == nil {
		client = s.client
	}
	remoteIDs := remoteMessageIDs(s.store.MessagesForMailbox(mailboxID))
	if len(remoteIDs) > 0 {
		if _, err := client.MoveRemoteMessagesToTrash(ctx, session, remoteIDs); err != nil {
			return fmt.Errorf("删除隐私邮箱后移动关联邮件失败：%w", err)
		}
	}
	return nil
}

func (s *Service) CleanRemoteMessages(ctx context.Context, mailboxID string, options RemoteCleanupOptions) (protocol.ICloudMailCleanupResult, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return protocol.ICloudMailCleanupResult{}, errors.New("邮箱不存在")
	}
	session, ok := s.store.ICloudSessionByAccountID(mailbox.AccountID)
	if !ok {
		return protocol.ICloudMailCleanupResult{}, errors.New("对应 Apple 账号登录态不存在")
	}
	options = normalizeRemoteCleanupOptions(options)
	result := protocol.ICloudMailCleanupResult{}
	if options.MoveSynced {
		moved, err := s.client.MoveRemoteMessagesToTrash(ctx, session, remoteMessageIDs(s.store.MessagesForMailbox(mailboxID)))
		result.MovedToTrash += moved.MovedToTrash
		result.Skipped += moved.Skipped
		if err != nil {
			return result, err
		}
		localIDs := append(append([]string(nil), moved.MovedRemoteIDs...), moved.AbsentRemoteIDs...)
		removed, err := s.store.DeleteMailboxMessagesByRemoteIDs(mailboxID, localIDs)
		if err != nil {
			return result, err
		}
		result.LocalRemoved += removed
	}
	if options.EmptyTrash {
		destroyed, err := s.client.EmptyTrash(ctx, session)
		result.Destroyed += destroyed
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) CleanRemoteMailboxes(ctx context.Context, options RemoteCleanupOptions) RemoteCleanupBatchResult {
	options = normalizeRemoteCleanupOptions(options)
	options.AccountID = strings.TrimSpace(options.AccountID)
	result := RemoteCleanupBatchResult{}
	cleanedTrash := make(map[string]bool)
	for _, mailbox := range s.store.AllMailboxes() {
		if ctx.Err() != nil {
			break
		}
		if options.AccountID != "" && mailbox.AccountID != options.AccountID {
			continue
		}
		if !mailbox.ICloudActive || mailbox.Status == domain.StatusDisabled {
			result.Cleanup.Skipped++
			continue
		}
		session, ok := s.store.ICloudSessionByAccountID(mailbox.AccountID)
		if !ok {
			result.Cleanup.Skipped++
			continue
		}
		if options.MoveSynced {
			moved, err := s.client.MoveRemoteMessagesToTrash(ctx, session, remoteMessageIDs(s.store.MessagesForMailbox(mailbox.ID)))
			result.Cleanup.MovedToTrash += moved.MovedToTrash
			result.Cleanup.Skipped += moved.Skipped
			if err != nil {
				result.FailedMailboxes++
				result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				continue
			}
			localIDs := append(append([]string(nil), moved.MovedRemoteIDs...), moved.AbsentRemoteIDs...)
			removed, err := s.store.DeleteMailboxMessagesByRemoteIDs(mailbox.ID, localIDs)
			if err != nil {
				result.FailedMailboxes++
				result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				continue
			}
			result.Cleanup.LocalRemoved += removed
		}
		result.Mailboxes++
		sessionKey := firstNonEmpty(session.AccountID, session.DSID, session.AppleID, mailbox.AccountID)
		if options.EmptyTrash && !cleanedTrash[sessionKey] {
			destroyed, err := s.client.EmptyTrash(ctx, session)
			result.Cleanup.Destroyed += destroyed
			if err != nil {
				result.FailedMailboxes++
				result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				continue
			}
			cleanedTrash[sessionKey] = true
		}
	}
	return result
}

func (s *Service) SyncMessages(ctx context.Context, mailboxID string) (int, error) {
	return s.syncMessages(ctx, mailboxID, false)
}

// SyncMessagesFresh waits for any sync that was already running when this call
// started, then guarantees that the result includes a newer scan round.
func (s *Service) SyncMessagesFresh(ctx context.Context, mailboxID string) (int, error) {
	return s.syncMessages(ctx, mailboxID, true)
}

func (s *Service) syncMessages(ctx context.Context, mailboxID string, requireFresh bool) (int, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return 0, errors.New("邮箱不存在")
	}
	mailboxes := s.mailboxesForAccount(mailbox.AccountID)
	found := false
	for _, item := range mailboxes {
		if item.ID == mailbox.ID {
			found = true
			break
		}
	}
	if !found {
		mailboxes = append(mailboxes, mailbox)
	}
	if requireFresh {
		return s.SyncMailboxBatchFresh(ctx, mailboxes, time.Time{}, "", s.cfg.MailWatcherFetchLimit)
	}
	return s.SyncMailboxBatch(ctx, mailboxes, time.Time{}, "", s.cfg.MailWatcherFetchLimit)
}

// SyncMailboxBatch 按 Apple 账号批量拉取一次收件箱，再把邮件分发给对应隐私邮箱。
func (s *Service) SyncMailboxBatch(ctx context.Context, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int) (int, error) {
	return s.syncMailboxBatch(ctx, mailboxes, after, keyword, maxMessages, false)
}

// SyncMailboxBatchFresh guarantees that every account gets a round that starts
// after this call begins. An older shared round is allowed to finish, but its
// result is not reused as the requested refresh.
func (s *Service) SyncMailboxBatchFresh(ctx context.Context, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int) (int, error) {
	return s.syncMailboxBatch(ctx, mailboxes, after, keyword, maxMessages, true)
}

// SyncMailboxBatchFolderFresh performs a new IMAP scan of only the folder that
// produced an IDLE event. Scheduled fallbacks continue to use the full scan.
func (s *Service) SyncMailboxBatchFolderFresh(ctx context.Context, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int, folder string) (int, error) {
	if len(mailboxes) == 0 {
		return 0, nil
	}
	if maxMessages <= 0 {
		maxMessages = s.cfg.MailWatcherFetchLimit
	}
	groups := make(map[string][]domain.Mailbox)
	order := make([]string, 0)
	for _, mailbox := range mailboxes {
		key := firstNonEmpty(mailbox.AccountID, mailbox.OwnerID, "__legacy__")
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], mailbox)
	}
	total := 0
	for _, key := range order {
		count, err := s.syncGroupFolderFresh(ctx, key, groups[key], after, keyword, maxMessages, folder, time.Now())
		total += count
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *Service) syncMailboxBatch(ctx context.Context, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int, requireFresh bool) (int, error) {
	if len(mailboxes) == 0 {
		return 0, nil
	}
	if maxMessages <= 0 {
		maxMessages = s.cfg.MailWatcherFetchLimit
	}
	groups := make(map[string][]domain.Mailbox)
	freshAfter := time.Time{}
	if requireFresh {
		freshAfter = time.Now()
	}
	order := make([]string, 0)
	for _, mailbox := range mailboxes {
		key := firstNonEmpty(mailbox.AccountID, mailbox.OwnerID, "__legacy__")
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], mailbox)
	}
	total := 0
	for _, key := range order {
		var count int
		var err error
		if requireFresh {
			count, err = s.syncGroupFresh(ctx, key, groups[key], after, keyword, maxMessages, freshAfter)
		} else {
			count, err = s.syncGroup(ctx, key, groups[key], after, keyword, maxMessages)
		}
		total += count
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *Service) syncGroup(ctx context.Context, key string, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int) (int, error) {
	return s.syncGroupFolder(ctx, key, mailboxes, after, keyword, maxMessages, protocol.IMAPSyncFolderAll, time.Time{})
}

func (s *Service) syncGroupFolderFresh(ctx context.Context, key string, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int, folder string, freshAfter time.Time) (int, error) {
	return s.syncGroupFolder(ctx, key, mailboxes, after, keyword, maxMessages, folder, freshAfter)
}

func (s *Service) syncGroupFolder(ctx context.Context, key string, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int, folder string, freshAfter time.Time) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	folder = normalizeSyncFolder(folder)
	for {
		s.syncMu.Lock()
		if running := s.syncs[key]; running != nil {
			canJoin := syncFolderCovers(running.folder, folder) && (freshAfter.IsZero() || !running.startedAt.Before(freshAfter))
			s.syncMu.Unlock()
			count, err := waitSyncCall(ctx, running)
			if canJoin || ctx.Err() != nil {
				return count, err
			}
			// The completed round either started before this fresh request or did
			// not scan the requested folder. Compete for exactly one trailing
			// round; other waiters will join the winner.
			continue
		}
		call := &syncCall{done: make(chan struct{}), startedAt: time.Now(), folder: folder}
		s.syncs[key] = call
		s.syncMu.Unlock()

		// Keep the network operation independent from an individual HTTP
		// deadline. All callers for this account share this one round.
		mailboxesCopy := append([]domain.Mailbox(nil), mailboxes...)
		go func() {
			sharedCtx, cancel := context.WithTimeout(context.Background(), mailboxSyncSharedTimeout)
			defer cancel()
			var count int
			var err error
			if s.syncFolderRunner != nil {
				count, err = s.syncFolderRunner(sharedCtx, mailboxesCopy, after, keyword, maxMessages, folder)
			} else if folder == protocol.IMAPSyncFolderAll && s.syncRunner != nil {
				count, err = s.syncRunner(sharedCtx, mailboxesCopy, after, keyword, maxMessages)
			} else {
				count, err = s.syncGroupNowFolder(sharedCtx, mailboxesCopy, after, keyword, maxMessages, folder)
			}
			s.syncMu.Lock()
			call.count = count
			call.err = err
			delete(s.syncs, key)
			close(call.done)
			s.syncMu.Unlock()
		}()
		return waitSyncCall(ctx, call)
	}
}

func (s *Service) syncGroupFresh(ctx context.Context, key string, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int, freshAfter time.Time) (int, error) {
	return s.syncGroupFolder(ctx, key, mailboxes, after, keyword, maxMessages, protocol.IMAPSyncFolderAll, freshAfter)
}

func normalizeSyncFolder(folder string) string {
	switch strings.ToLower(strings.TrimSpace(folder)) {
	case protocol.IMAPSyncFolderInbox:
		return protocol.IMAPSyncFolderInbox
	case protocol.IMAPSyncFolderJunk:
		return protocol.IMAPSyncFolderJunk
	default:
		return protocol.IMAPSyncFolderAll
	}
}

func syncFolderCovers(running, requested string) bool {
	running = normalizeSyncFolder(running)
	requested = normalizeSyncFolder(requested)
	return running == protocol.IMAPSyncFolderAll || running == requested
}

func waitSyncCall(ctx context.Context, call *syncCall) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-call.done:
		return call.count, call.err
	}
}

func (s *Service) syncGroupNow(ctx context.Context, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int) (int, error) {
	return s.syncGroupNowFolder(ctx, mailboxes, after, keyword, maxMessages, protocol.IMAPSyncFolderAll)
}

func (s *Service) syncGroupNowFolder(ctx context.Context, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int, folder string) (int, error) {
	refreshed := make([]domain.Mailbox, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		if current, ok := s.store.FindMailboxByID(mailbox.ID); ok {
			refreshed = append(refreshed, current)
		}
	}
	if len(refreshed) == 0 {
		return 0, nil
	}
	session, ok := s.store.ICloudSessionByAccountID(refreshed[0].AccountID)
	if !ok {
		return 0, errors.New("对应 Apple 账号登录态不存在")
	}

	messagesByMailbox := make(map[string][]protocol.ICloudSyncedMessage)
	lastUID := ""
	lastJunkUID := ""
	source := "icloud"
	var imapCursorState domain.LoginState
	var imapSyncErr error
	if imapState, saved := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP); saved {
		originalIMAPState := imapState
		prepared, err := protocol.PrepareIMAPLoginState(ctx, imapState)
		if err != nil {
			return 0, err
		}
		imapState = prepared
		updatedSession := protocol.WithLoginState(session, imapState)
		// Normalizing an already-valid IMAP state is a no-op. Avoid writing the
		// full JSON state (and an event) on every poll when nothing changed.
		if !reflect.DeepEqual(session, updatedSession) {
			// Update only if the state we read is still current. A watcher or
			// OAuth refresher may have changed the IMAP token while this request
			// was preparing; in that case keep the concurrently refreshed state.
			updated, updateErr := s.updatePreparedIMAPState(refreshed[0].AccountID, originalIMAPState, imapState)
			if updateErr != nil {
				return 0, updateErr
			}
			session = updated
			if current, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP); ok {
				imapState = current
			}
		} else {
			session = updatedSession
		}
		result, syncErr := protocol.SyncICloudIMAPMessagesFolderDetailed(ctx, imapState, refreshed, after, keyword, maxMessages, folder)
		if syncErr != nil && result.MessagesByMailbox == nil {
			return 0, syncErr
		}
		messagesByMailbox = result.MessagesByMailbox
		lastUID = strings.TrimSpace(result.LastUID)
		lastJunkUID = strings.TrimSpace(result.LastJunkUID)
		imapCursorState = imapState
		imapSyncErr = syncErr
		source = "imap"
	} else {
		var err error
		messagesByMailbox, err = s.client.SyncMailboxMessagesBatch(ctx, session, refreshed, after, keyword, maxMessages)
		if err != nil {
			return 0, err
		}
	}

	created, applyErr := s.applyMailboxSync(refreshed, messagesByMailbox, lastUID, source)
	if applyErr != nil {
		return created, applyErr
	}
	// Persist the account-level cursors only after messages are durable. If the
	// cursor write fails, the next round may fetch duplicates, which RemoteID
	// de-duplicates; advancing first could permanently skip an uncommitted mail.
	if source == "imap" && (shouldAdvanceIMAPCursor(imapCursorState.IMAPLastSyncUID, lastUID) || shouldAdvanceIMAPCursor(imapCursorState.IMAPJunkLastSyncUID, lastJunkUID)) {
		cursorAt := time.Now()
		if _, updateErr := s.store.UpdateICloudSession(refreshed[0].AccountID, func(current *domain.ICloudSession) error {
			*current = mergeIMAPSyncCursors(*current, imapCursorState, lastUID, lastJunkUID, cursorAt)
			return nil
		}); updateErr != nil {
			return created, updateErr
		}
	}
	if imapSyncErr != nil {
		return created, imapSyncErr
	}
	return created, nil
}

func (s *Service) applyMailboxSync(refreshed []domain.Mailbox, messagesByMailbox map[string][]protocol.ICloudSyncedMessage, lastUID, source string) (int, error) {
	syncedAt := time.Now()
	updates := make([]store.MailboxSyncUpdate, 0, len(refreshed))
	for _, mailbox := range refreshed {
		mailboxUID := maxIMAPCursor(lastUID, mailbox.LastSyncUID)
		update := store.MailboxSyncUpdate{MailboxID: mailbox.ID, LastSyncUID: mailboxUID, SyncedAt: syncedAt, SkipIfUnchanged: source == "imap"}
		for _, message := range messagesByMailbox[mailbox.ID] {
			remoteID := firstNonEmpty(message.RemoteID, message.UID)
			update.Messages = append(update.Messages, domain.Message{
				RemoteID: remoteID, Source: source, Subject: message.Subject,
				From: message.From, Body: message.Body, ReceivedAt: message.ReceivedAt,
			})
		}
		updates = append(updates, update)
	}
	return s.store.ApplyMailboxSync(updates)
}

// updatePreparedIMAPState performs a compare-and-apply update for the
// normalization/OAuth preparation step. It prevents a stale snapshot from
// replacing a token refreshed by another watcher between the read and write.
func (s *Service) updatePreparedIMAPState(accountID string, original, prepared domain.LoginState) (domain.ICloudSession, error) {
	return s.store.UpdateICloudSession(accountID, func(current *domain.ICloudSession) error {
		currentState, exists := protocol.LoginStateForKind(*current, domain.LoginStateICloudIMAP)
		if !exists || reflect.DeepEqual(currentState, original) {
			*current = protocol.WithLoginState(*current, prepared)
		}
		return nil
	})
}

// mergeIMAPCursor updates only the IMAP cursor on the current session. The
// current token/password and all other login states remain untouched. Cursor
// values are monotonic so a slower request cannot move the cursor backwards.
func mergeIMAPCursor(current domain.ICloudSession, fallback domain.LoginState, nextUID string, syncedAt time.Time) domain.ICloudSession {
	return mergeIMAPSyncCursors(current, fallback, nextUID, "", syncedAt)
}

func mergeIMAPSyncCursors(current domain.ICloudSession, fallback domain.LoginState, nextUID, nextJunkUID string, syncedAt time.Time) domain.ICloudSession {
	state, exists := protocol.LoginStateForKind(current, domain.LoginStateICloudIMAP)
	if !exists {
		state = fallback
	}
	changed := false
	if shouldAdvanceIMAPCursor(state.IMAPLastSyncUID, nextUID) {
		state.IMAPLastSyncUID = strings.TrimSpace(nextUID)
		state.IMAPLastSyncAt = syncedAt
		changed = true
	}
	if shouldAdvanceIMAPCursor(state.IMAPJunkLastSyncUID, nextJunkUID) {
		state.IMAPJunkLastSyncUID = strings.TrimSpace(nextJunkUID)
		state.IMAPJunkLastSyncAt = syncedAt
		changed = true
	}
	if changed {
		current = protocol.WithLoginState(current, state)
	}
	return current
}

func maxIMAPCursor(left, right string) string {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if shouldAdvanceIMAPCursor(right, left) {
		return left
	}
	return right
}

func shouldAdvanceIMAPCursor(current, next string) bool {
	current = strings.TrimSpace(current)
	next = strings.TrimSpace(next)
	if next == "" {
		return false
	}
	if current == "" {
		return true
	}
	currentNumber, currentOK := imapCursorNumber(current)
	nextNumber, nextOK := imapCursorNumber(next)
	if currentOK && nextOK {
		return nextNumber > currentNumber
	}
	return !strings.EqualFold(current, next)
}

func imapCursorNumber(value string) (int, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "imap:")
	number, err := strconv.Atoi(value)
	return number, err == nil && number > 0
}

func (s *Service) Code(ctx context.Context, mailboxID string, after time.Time, keyword string, allowStale bool) (CodeResult, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return CodeResult{}, errors.New("邮箱不存在")
	}
	query := CodeQuery{After: after, Keyword: keyword, SkipMessageID: mailbox.LastCodeMessageID, MarkAsServed: true, AllowStale: allowStale}
	if result, found, err := s.findCode(mailbox, query); found || err != nil {
		return result, err
	}
	if _, err := s.SyncMessages(ctx, mailboxID); err != nil && !allowStale {
		return CodeResult{}, err
	}
	mailbox, _ = s.store.FindMailboxByID(mailboxID)
	if result, found, err := s.findCode(mailbox, query); found || err != nil {
		return result, err
	}
	return CodeResult{}, errors.New("暂未收到验证码")
}

// CachedCode 只读取本地邮件缓存，不触发 Apple 或 IMAP 网络请求。
func (s *Service) CachedCode(mailboxID string, after time.Time, keyword string, allowStale bool) (CodeResult, bool, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return CodeResult{}, false, errors.New("邮箱不存在")
	}
	query := CodeQuery{After: after, Keyword: keyword, SkipMessageID: mailbox.LastCodeMessageID, MarkAsServed: true, AllowStale: allowStale}
	return s.findCode(mailbox, query)
}

// CachedCodeWithQuery 支持预览、缓存读取和固定请求起点，便于合并并发取码请求。
func (s *Service) CachedCodeWithQuery(mailboxID string, query CodeQuery) (CodeResult, bool, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return CodeResult{}, false, errors.New("邮箱不存在")
	}
	return s.findCode(mailbox, query)
}

func (s *Service) findCode(mailbox domain.Mailbox, query CodeQuery) (CodeResult, bool, error) {
	keyword := strings.TrimSpace(query.Keyword)
	if keyword == "" {
		keyword = "OpenAI"
	}
	after := codeAfter(query.After, time.Now(), query.AllowStale)
	skipMessageID := strings.TrimSpace(query.SkipMessageID)
	if query.IncludeServed {
		skipMessageID = ""
	}
	for _, message := range s.store.MessagesForMailbox(mailbox.ID) {
		messageTime := message.ReceivedAt
		if messageTime.IsZero() {
			messageTime = message.CreatedAt
		}
		if messageTime.IsZero() || messageTime.Before(after) {
			continue
		}
		text := message.Subject + " " + message.From + " " + message.Body
		if !strings.EqualFold(keyword, "OpenAI") && !strings.Contains(strings.ToLower(text), strings.ToLower(keyword)) {
			continue
		}
		if skipMessageID != "" && message.ID == skipMessageID {
			continue
		}
		code := protocol.ExtractOTP(message.Subject + "\n" + message.Body)
		if code == "" {
			continue
		}
		if query.MarkAsServed {
			claimed, err := s.store.ClaimMailboxCode(mailbox.ID, mailbox.LastCodeMessageID, message.ID, time.Now())
			if err != nil {
				return CodeResult{}, false, err
			}
			if !claimed {
				return CodeResult{}, false, nil
			}
		}
		detectedAt := message.CreatedAt
		if detectedAt.IsZero() {
			detectedAt = messageTime
		}
		deliveryDelay := detectedAt.Sub(messageTime)
		if deliveryDelay < 0 {
			deliveryDelay = 0
		}
		return CodeResult{
			Email: mailbox.Email, Code: code, Subject: message.Subject, From: message.From,
			ReceivedAt: messageTime, DetectedAt: detectedAt, DeliveryDelayMS: deliveryDelay.Milliseconds(),
			MessageID: message.ID, Folder: codeMessageFolder(message),
		}, true, nil
	}
	return CodeResult{}, false, nil
}

func codeMessageFolder(message domain.Message) string {
	remoteID := strings.ToLower(strings.TrimSpace(message.RemoteID))
	if strings.HasPrefix(remoteID, "imap:junk:") {
		return "junk"
	}
	if rest, ok := strings.CutPrefix(remoteID, "icloud:"); ok {
		folder, _, _ := strings.Cut(rest, ":")
		folder = strings.TrimSpace(folder)
		if strings.Contains(folder, "junk") || strings.Contains(folder, "spam") || strings.Contains(folder, "垃圾") {
			return "junk"
		}
	}
	return "inbox"
}

func codeAfter(after, now time.Time, allowStale bool) time.Time {
	if allowStale {
		return after
	}
	cutoff := now.Add(-mailboxCodeFreshWindow)
	if after.After(cutoff) {
		return after
	}
	return cutoff
}

func (s *Service) mailboxesForAccount(accountID string) []domain.Mailbox {
	accountID = strings.TrimSpace(accountID)
	out := make([]domain.Mailbox, 0)
	for _, mailbox := range s.store.AllMailboxes() {
		if mailbox.AccountID != accountID || !mailbox.ICloudActive || mailbox.Status == domain.StatusDisabled {
			continue
		}
		out = append(out, mailbox)
	}
	return out
}

func normalizeRemoteCleanupOptions(options RemoteCleanupOptions) RemoteCleanupOptions {
	if !options.MoveSynced && !options.EmptyTrash {
		options.MoveSynced = true
		options.EmptyTrash = true
	}
	return options
}

func remoteMessageIDs(messages []domain.Message) []string {
	out := make([]string, 0, len(messages))
	seen := make(map[string]bool)
	for _, message := range messages {
		remoteID := strings.TrimSpace(message.RemoteID)
		if remoteID == "" || seen[remoteID] {
			continue
		}
		seen[remoteID] = true
		out = append(out, remoteID)
	}
	return out
}

func remoteMailbox(remote protocol.ICloudRemoteMailbox) domain.RemoteMailbox {
	return domain.RemoteMailbox{AnonymousID: remote.AnonymousID, Email: remote.Email, Label: remote.Label, Note: remote.Note, IsActive: remote.IsActive, Origin: remote.Origin}
}

func matchRemote(remotes []protocol.ICloudRemoteMailbox, anonymousID, email string) (protocol.ICloudRemoteMailbox, bool) {
	for _, remote := range remotes {
		if strings.TrimSpace(anonymousID) != "" && remote.AnonymousID == strings.TrimSpace(anonymousID) {
			return remote, true
		}
		if strings.EqualFold(remote.Email, strings.TrimSpace(email)) {
			return remote, true
		}
	}
	return protocol.ICloudRemoteMailbox{}, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
