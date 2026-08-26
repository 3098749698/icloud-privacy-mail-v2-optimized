package store

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// MailboxSyncUpdate is the store-facing representation of one IMAP sync
// result. Messages and the mailbox cursor are persisted together so a round
// touching many aliases only performs one state-file write.
type MailboxSyncUpdate struct {
	MailboxID   string
	Messages    []domain.Message
	SyncedAt    time.Time
	LastSyncUID string
	// SkipIfUnchanged avoids touching LastSyncAt for an IMAP round that only
	// re-read already cached messages (notably QQ Junk fallback polling).
	// Web syncs leave this false because their timestamp is also the search
	// cursor.
	SkipIfUnchanged bool
}

func (s *Store) AllMailboxes() []domain.Mailbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]domain.Mailbox(nil), s.state.Mailboxes...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// ExportAvailableMailboxes atomically selects available mailboxes and marks them used.
// Existing callers keep permanent exports; callers that need automatic remote
// deletion should use ExportAvailableMailboxesWithValidity.
func (s *Store) ExportAvailableMailboxes(accountID string, limit int, exportedAt time.Time) ([]domain.Mailbox, error) {
	return s.ExportAvailableMailboxesWithValidity(accountID, limit, exportedAt, 0)
}

// ExportAvailableMailboxesWithValidity records the export lifecycle in the
// same state-file write that marks mailboxes used. A zero validity means the
// export does not expire.
func (s *Store) ExportAvailableMailboxesWithValidity(accountID string, limit int, exportedAt time.Time, validFor time.Duration) ([]domain.Mailbox, error) {
	if limit <= 0 {
		return nil, errors.New("导出数量必须大于 0")
	}
	if limit > 10000 {
		return nil, errors.New("单次最多导出 10000 个邮箱")
	}
	if exportedAt.IsZero() {
		exportedAt = time.Now()
	}
	if validFor < 0 {
		return nil, errors.New("邮箱有效期不能小于 0")
	}
	accountID = strings.TrimSpace(accountID)

	s.mu.Lock()
	defer s.mu.Unlock()
	indexes := make([]int, 0, limit)
	for index, mailbox := range s.state.Mailboxes {
		if accountID != "" && mailbox.AccountID != accountID {
			continue
		}
		if mailbox.Status != domain.StatusAvailable || !mailbox.APIActive || !mailbox.ICloudActive || mailbox.ActiveLeaseID != "" {
			continue
		}
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool {
		return s.state.Mailboxes[indexes[i]].CreatedAt.Before(s.state.Mailboxes[indexes[j]].CreatedAt)
	})
	if len(indexes) > limit {
		indexes = indexes[:limit]
	}
	if len(indexes) == 0 {
		return nil, errors.New("没有可导出的未使用邮箱")
	}

	result := make([]domain.Mailbox, 0, len(indexes))
	previous := make([]domain.Mailbox, 0, len(indexes))
	previousEventCount := len(s.state.Events)
	previousUpdatedAt := s.state.UpdatedAt
	for _, index := range indexes {
		previous = append(previous, s.state.Mailboxes[index])
		mailbox := &s.state.Mailboxes[index]
		mailbox.Status = domain.StatusUsed
		mailbox.ExportedAt = exportedAt
		mailbox.AutoDeleteAt = time.Time{}
		if validFor > 0 {
			mailbox.AutoDeleteAt = exportedAt.Add(validFor)
		}
		mailbox.DeleteAttempts = 0
		mailbox.DeleteLastAttemptAt = time.Time{}
		mailbox.DeleteRetryAt = time.Time{}
		mailbox.DeleteLastError = ""
		mailbox.UpdatedAt = exportedAt
		result = append(result, *mailbox)
	}
	eventMessage := fmt.Sprintf("已导出并标记为已使用：%d 个邮箱", len(result))
	if validFor > 0 {
		eventMessage += fmt.Sprintf("，到期时间 %s", exportedAt.Add(validFor).Format(time.RFC3339))
	}
	s.appendEventLocked("info", "mailbox_export", eventMessage)
	if err := s.saveLocked(); err != nil {
		for offset, index := range indexes {
			s.state.Mailboxes[index] = previous[offset]
		}
		s.state.Events = s.state.Events[:previousEventCount]
		s.state.UpdatedAt = previousUpdatedAt
		return nil, err
	}
	return result, nil
}

// RollbackMailboxExport releases rows claimed by one exact export transaction.
// It is used when the HTTP response cannot be delivered to the client; later
// mailbox changes are preserved by matching the original export timestamp.
func (s *Store) RollbackMailboxExport(mailboxes []domain.Mailbox, exportedAt time.Time) (int, error) {
	if len(mailboxes) == 0 || exportedAt.IsZero() {
		return 0, nil
	}
	targets := make(map[string]bool, len(mailboxes))
	for _, mailbox := range mailboxes {
		if id := strings.TrimSpace(mailbox.ID); id != "" {
			targets[id] = true
		}
	}
	if len(targets) == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous := make(map[int]domain.Mailbox)
	previousEventCount := len(s.state.Events)
	previousUpdatedAt := s.state.UpdatedAt
	now := time.Now()
	rolledBack := 0
	for index := range s.state.Mailboxes {
		mailbox := &s.state.Mailboxes[index]
		if !targets[mailbox.ID] || mailbox.Status != domain.StatusUsed || !mailbox.ExportedAt.Equal(exportedAt) || mailbox.DeleteAttempts != 0 {
			continue
		}
		previous[index] = *mailbox
		mailbox.Status = domain.StatusAvailable
		mailbox.ExportedAt = time.Time{}
		mailbox.AutoDeleteAt = time.Time{}
		mailbox.DeleteLastAttemptAt = time.Time{}
		mailbox.DeleteRetryAt = time.Time{}
		mailbox.DeleteLastError = ""
		mailbox.UpdatedAt = now
		rolledBack++
	}
	if rolledBack == 0 {
		return 0, nil
	}
	s.appendEventLocked("warning", "mailbox_export", fmt.Sprintf("导出响应未送达，已退回可用池：%d 个邮箱", rolledBack))
	if err := s.saveLocked(); err != nil {
		for index, mailbox := range previous {
			s.state.Mailboxes[index] = mailbox
		}
		s.state.Events = s.state.Events[:previousEventCount]
		s.state.UpdatedAt = previousUpdatedAt
		return 0, err
	}
	return rolledBack, nil
}

// ClaimExpiredMailboxes atomically leases due records to one reaper run. The
// retry timestamp also acts as a short crash lease: after a process restart the
// record becomes eligible again without losing its persisted expiry.
func (s *Store) ClaimExpiredMailboxes(now time.Time, limit int, claimFor time.Duration) ([]domain.Mailbox, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if limit <= 0 {
		return nil, errors.New("回收数量必须大于 0")
	}
	if limit > 100 {
		limit = 100
	}
	if claimFor <= 0 {
		claimFor = 2 * time.Minute
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	indexes := make([]int, 0, limit)
	for index, mailbox := range s.state.Mailboxes {
		if mailbox.AutoDeleteAt.IsZero() || mailbox.AutoDeleteAt.After(now) {
			continue
		}
		if !mailbox.DeleteRetryAt.IsZero() && mailbox.DeleteRetryAt.After(now) {
			continue
		}
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool {
		left := s.state.Mailboxes[indexes[i]]
		right := s.state.Mailboxes[indexes[j]]
		if left.AutoDeleteAt.Equal(right.AutoDeleteAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.AutoDeleteAt.Before(right.AutoDeleteAt)
	})
	if len(indexes) > limit {
		indexes = indexes[:limit]
	}
	if len(indexes) == 0 {
		return nil, nil
	}

	previous := make([]domain.Mailbox, 0, len(indexes))
	previousUpdatedAt := s.state.UpdatedAt
	claimed := make([]domain.Mailbox, 0, len(indexes))
	for _, index := range indexes {
		previous = append(previous, s.state.Mailboxes[index])
		mailbox := &s.state.Mailboxes[index]
		mailbox.DeleteAttempts++
		mailbox.DeleteLastAttemptAt = now
		mailbox.DeleteRetryAt = now.Add(claimFor)
		mailbox.UpdatedAt = now
		claimed = append(claimed, *mailbox)
	}
	if err := s.saveLocked(); err != nil {
		for offset, index := range indexes {
			s.state.Mailboxes[index] = previous[offset]
		}
		s.state.UpdatedAt = previousUpdatedAt
		return nil, err
	}
	return claimed, nil
}

// RecordMailboxAutoDeleteFailure keeps the local record and schedules another
// remote deletion attempt. This is deliberately separate from DeleteMailbox:
// an Apple-side failure must never consume the only record capable of retrying.
func (s *Store) RecordMailboxAutoDeleteFailure(id string, attemptedAt, retryAt time.Time, reason string) (domain.Mailbox, error) {
	return s.recordMailboxDeleteFailure(id, time.Time{}, attemptedAt, retryAt, reason, false)
}

// RecordMailboxCleanupFailure persists the fact that Apple has already
// released the alias. A retry can then finish local cleanup without issuing
// another Apple list/delete sequence.
func (s *Store) RecordMailboxCleanupFailure(id string, releasedAt, attemptedAt, retryAt time.Time, reason string) (domain.Mailbox, error) {
	return s.recordMailboxDeleteFailure(id, releasedAt, attemptedAt, retryAt, reason, true)
}

func (s *Store) recordMailboxDeleteFailure(id string, releasedAt, attemptedAt, retryAt time.Time, reason string, cleanupOnly bool) (domain.Mailbox, error) {
	if attemptedAt.IsZero() {
		attemptedAt = time.Now()
	}
	if !retryAt.After(attemptedAt) {
		retryAt = attemptedAt.Add(time.Minute)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.mailboxIndexLocked(id)
	if index < 0 {
		return domain.Mailbox{}, errors.New("邮箱不存在")
	}
	previous := s.state.Mailboxes[index]
	previousEventCount := len(s.state.Events)
	previousUpdatedAt := s.state.UpdatedAt
	mailbox := &s.state.Mailboxes[index]
	if cleanupOnly {
		if releasedAt.IsZero() {
			releasedAt = attemptedAt
		}
		if mailbox.QuotaReleasedAt.IsZero() {
			mailbox.QuotaReleasedAt = releasedAt
		}
		mailbox.ICloudActive = false
	}
	mailbox.DeleteLastAttemptAt = attemptedAt
	mailbox.DeleteRetryAt = retryAt
	mailbox.DeleteLastError = strings.TrimSpace(reason)
	mailbox.UpdatedAt = attemptedAt
	eventMessage := fmt.Sprintf("到期邮箱 %s 远端删除失败，将于 %s 重试：%s", mailbox.Email, retryAt.Format(time.RFC3339), mailbox.DeleteLastError)
	if cleanupOnly {
		eventMessage = fmt.Sprintf("到期邮箱 %s 的 Apple 配额已释放，本地清理将于 %s 重试：%s", mailbox.Email, retryAt.Format(time.RFC3339), mailbox.DeleteLastError)
	}
	s.appendEventLocked("warning", "mailbox_auto_delete", eventMessage)
	if err := s.saveLocked(); err != nil {
		s.state.Mailboxes[index] = previous
		s.state.Events = s.state.Events[:previousEventCount]
		s.state.UpdatedAt = previousUpdatedAt
		return domain.Mailbox{}, err
	}
	return *mailbox, nil
}

// MarkMailboxQuotaReleased records the irreversible remote milestone before
// optional mail cleanup starts. The update is rolled back in memory if it
// cannot be persisted.
func (s *Store) MarkMailboxQuotaReleased(id string, releasedAt time.Time) (domain.Mailbox, error) {
	if releasedAt.IsZero() {
		releasedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.mailboxIndexLocked(id)
	if index < 0 {
		return domain.Mailbox{}, errors.New("邮箱不存在")
	}
	previous := s.state.Mailboxes[index]
	previousEventCount := len(s.state.Events)
	previousUpdatedAt := s.state.UpdatedAt
	mailbox := &s.state.Mailboxes[index]
	if mailbox.QuotaReleasedAt.IsZero() {
		mailbox.QuotaReleasedAt = releasedAt
	}
	mailbox.ICloudActive = false
	mailbox.DeleteLastError = ""
	mailbox.UpdatedAt = releasedAt
	s.appendEventLocked("info", "mailbox_auto_delete", "已确认 Apple 端隐私邮箱不存在，配额已释放："+mailbox.Email)
	if err := s.saveLocked(); err != nil {
		s.state.Mailboxes[index] = previous
		s.state.Events = s.state.Events[:previousEventCount]
		s.state.UpdatedAt = previousUpdatedAt
		return domain.Mailbox{}, err
	}
	return *mailbox, nil
}

func (s *Store) FindMailboxByID(id string) (domain.Mailbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, mailbox := range s.state.Mailboxes {
		if mailbox.ID == strings.TrimSpace(id) {
			return mailbox, true
		}
	}
	return domain.Mailbox{}, false
}

func (s *Store) FindMailboxByEmail(email string) (domain.Mailbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	email = strings.ToLower(strings.TrimSpace(email))
	for _, mailbox := range s.state.Mailboxes {
		if strings.EqualFold(mailbox.Email, email) {
			return mailbox, true
		}
	}
	return domain.Mailbox{}, false
}

func (s *Store) UpsertMailboxFromRemote(accountID string, remote domain.RemoteMailbox, defaultNote string) (domain.Mailbox, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email := strings.ToLower(strings.TrimSpace(remote.Email))
	if email == "" {
		return domain.Mailbox{}, false, errors.New("Apple 返回的隐私邮箱地址为空")
	}
	ownerID := ""
	if s.state.Admin != nil {
		ownerID = s.state.Admin.ID
	}
	now := time.Now()
	for i := range s.state.Mailboxes {
		if !strings.EqualFold(s.state.Mailboxes[i].Email, email) {
			continue
		}
		mailbox := &s.state.Mailboxes[i]
		mailbox.OwnerID = firstNonEmpty(mailbox.OwnerID, ownerID)
		mailbox.AccountID = firstNonEmpty(accountID, mailbox.AccountID)
		mailbox.AnonymousID = firstNonEmpty(remote.AnonymousID, mailbox.AnonymousID)
		mailbox.RemoteOrigin = firstNonEmpty(remote.Origin, mailbox.RemoteOrigin)
		if strings.TrimSpace(remote.Label) != "" {
			mailbox.Label = strings.TrimSpace(remote.Label)
		}
		mailbox.ICloudActive = remote.IsActive
		if strings.TrimSpace(mailbox.Note) == "" {
			mailbox.Note = firstNonEmpty(remote.Note, defaultNote)
		}
		mailbox.UpdatedAt = now
		return *mailbox, false, s.saveLocked()
	}
	token, err := randomAPIToken(24)
	if err != nil {
		return domain.Mailbox{}, false, err
	}
	status := domain.StatusAvailable
	if !remote.IsActive {
		status = domain.StatusDisabled
	}
	mailbox := domain.Mailbox{
		ID:           s.nextIDLocked("mbx"),
		OwnerID:      ownerID,
		AccountID:    strings.TrimSpace(accountID),
		AnonymousID:  strings.TrimSpace(remote.AnonymousID),
		RemoteOrigin: strings.TrimSpace(remote.Origin),
		Label:        firstNonEmpty(remote.Label, "隐私邮箱 "+now.Format("0102-150405")),
		Email:        email,
		APIToken:     token,
		APIActive:    true,
		ICloudActive: remote.IsActive,
		Status:       status,
		Note:         firstNonEmpty(remote.Note, defaultNote),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	s.state.Mailboxes = append(s.state.Mailboxes, mailbox)
	s.appendEventLocked("info", "mailbox", "已导入隐私邮箱 "+mailbox.Email)
	return mailbox, true, s.saveLocked()
}

func (s *Store) SetMailboxRemoteIdentity(id, anonymousID, origin string) (domain.Mailbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.mailboxIndexLocked(id)
	if index < 0 {
		return domain.Mailbox{}, errors.New("邮箱不存在")
	}
	s.state.Mailboxes[index].AnonymousID = strings.TrimSpace(anonymousID)
	s.state.Mailboxes[index].RemoteOrigin = strings.TrimSpace(origin)
	s.state.Mailboxes[index].UpdatedAt = time.Now()
	return s.state.Mailboxes[index], s.saveLocked()
}

func (s *Store) SetMailboxStatus(id string, apiActive, icloudActive *bool, status string, note *string) (domain.Mailbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.mailboxIndexLocked(id)
	if index < 0 {
		return domain.Mailbox{}, errors.New("邮箱不存在")
	}
	mailbox := &s.state.Mailboxes[index]
	now := time.Now()
	if apiActive != nil {
		mailbox.APIActive = *apiActive
	}
	if icloudActive != nil {
		mailbox.ICloudActive = *icloudActive
	}
	desiredStatus := strings.TrimSpace(status)
	if desiredStatus != "" {
		if desiredStatus == domain.StatusReserved && mailbox.ActiveLeaseID == "" {
			return domain.Mailbox{}, errors.New("邮箱没有有效租约，不能手动标记为已预留")
		}
		if mailbox.ActiveLeaseID != "" && desiredStatus != domain.StatusReserved {
			s.completeActiveLeaseFromAdminLocked(mailbox, desiredStatus, now)
		}
		mailbox.Status = desiredStatus
	}
	if note != nil {
		mailbox.Note = strings.TrimSpace(*note)
		s.syncActiveLeaseNoteLocked(*mailbox, mailbox.Note, now)
	}
	mailbox.UpdatedAt = now
	s.appendEventLocked("info", "mailbox", "已更新邮箱状态 "+mailbox.Email)
	return *mailbox, s.saveLocked()
}

func (s *Store) DeleteMailbox(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.mailboxIndexLocked(id)
	if index < 0 {
		return errors.New("邮箱不存在")
	}
	previousMailboxes := append([]domain.Mailbox(nil), s.state.Mailboxes...)
	previousLeases := append([]domain.MailboxLease(nil), s.state.MailboxLeases...)
	previousMessages := append([]domain.Message(nil), s.state.Messages...)
	previousEvents := append([]domain.Event(nil), s.state.Events...)
	previousUpdatedAt := s.state.UpdatedAt
	email := s.state.Mailboxes[index].Email
	s.closeMailboxLeasesForDeletionLocked(id, time.Now(), "邮箱已由管理员删除")
	s.state.Mailboxes = append(s.state.Mailboxes[:index], s.state.Mailboxes[index+1:]...)
	messages := s.state.Messages[:0]
	for _, message := range s.state.Messages {
		if message.MailboxID != id {
			messages = append(messages, message)
		}
	}
	s.state.Messages = messages
	s.appendEventLocked("warning", "mailbox", "已删除本地邮箱 "+email)
	if err := s.saveLocked(); err != nil {
		s.state.Mailboxes = previousMailboxes
		s.state.MailboxLeases = previousLeases
		s.state.Messages = previousMessages
		s.state.Events = previousEvents
		s.state.UpdatedAt = previousUpdatedAt
		return err
	}
	return nil
}

func (s *Store) UpsertMessage(mailboxID, remoteID, source, subject, from, body string, receivedAt time.Time) (domain.Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.mailboxIndexLocked(mailboxID)
	if index < 0 {
		return domain.Message{}, false, errors.New("邮箱不存在")
	}
	for _, message := range s.state.Messages {
		if remoteID != "" && message.MailboxID == mailboxID && message.RemoteID == remoteID {
			return message, false, nil
		}
	}
	now := time.Now()
	if receivedAt.IsZero() {
		receivedAt = now
	}
	message := domain.Message{
		ID:         s.nextIDLocked("msg"),
		OwnerID:    s.state.Mailboxes[index].OwnerID,
		MailboxID:  mailboxID,
		RemoteID:   strings.TrimSpace(remoteID),
		Source:     strings.TrimSpace(source),
		Subject:    strings.TrimSpace(subject),
		From:       strings.TrimSpace(from),
		Body:       body,
		ReceivedAt: receivedAt,
		CreatedAt:  now,
	}
	s.state.Messages = append(s.state.Messages, message)
	s.state.Mailboxes[index].ReceiveCount++
	s.state.Mailboxes[index].LastSyncAt = now
	s.state.Mailboxes[index].UpdatedAt = now
	return message, true, s.saveLocked()
}

func (s *Store) MessagesForMailbox(mailboxID string) []domain.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.Message, 0)
	for _, message := range s.state.Messages {
		if message.MailboxID == strings.TrimSpace(mailboxID) {
			out = append(out, message)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.After(out[j].ReceivedAt) })
	return out
}

// PruneMessagesBefore 清理本地缓存中过期邮件，不触碰远端 IMAP 邮箱。
func (s *Store) PruneMessagesBefore(before time.Time) (int, error) {
	if before.IsZero() {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := before
	removedIDs := make(map[string]bool)
	next := make([]domain.Message, 0, len(s.state.Messages))
	removed := 0
	for _, message := range s.state.Messages {
		at := message.ReceivedAt
		if at.IsZero() {
			at = message.CreatedAt
		}
		if !at.IsZero() && at.Before(cutoff) {
			removed++
			removedIDs[message.ID] = true
			continue
		}
		next = append(next, message)
	}
	if removed == 0 {
		return 0, nil
	}
	s.state.Messages = next
	now := time.Now()
	for index := range s.state.Mailboxes {
		mailbox := &s.state.Mailboxes[index]
		count := 0
		for _, message := range next {
			if message.MailboxID == mailbox.ID {
				count++
			}
		}
		mailbox.ReceiveCount = count
		if removedIDs[mailbox.LastCodeMessageID] {
			mailbox.LastCodeMessageID = ""
			mailbox.LastCodeAt = time.Time{}
		}
		mailbox.UpdatedAt = now
	}
	s.appendEventLocked("info", "mailbox", fmt.Sprintf("清理本地过期邮件：%d 封", removed))
	return removed, s.saveLocked()
}

func (s *Store) DeleteMailboxMessages(mailboxID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mailboxID = strings.TrimSpace(mailboxID)
	index := s.mailboxIndexLocked(mailboxID)
	if index < 0 {
		return 0, errors.New("邮箱不存在")
	}

	mailbox := &s.state.Mailboxes[index]
	next := make([]domain.Message, 0, len(s.state.Messages))
	removed := 0
	for _, message := range s.state.Messages {
		if message.MailboxID == mailboxID {
			removed++
			continue
		}
		next = append(next, message)
	}
	metadataChanged := mailbox.ReceiveCount != 0 || mailbox.LastCodeMessageID != "" || !mailbox.LastCodeAt.IsZero()
	if removed == 0 && !metadataChanged {
		return 0, nil
	}
	s.state.Messages = next
	mailbox.ReceiveCount = 0
	mailbox.LastCodeMessageID = ""
	mailbox.LastCodeAt = time.Time{}
	mailbox.UpdatedAt = time.Now()
	s.appendEventLocked("info", "mailbox", fmt.Sprintf("已清空本地邮件 %s：%d 封", mailbox.Email, removed))
	return removed, s.saveLocked()
}

func (s *Store) DeleteMailboxMessagesByRemoteIDs(mailboxID string, remoteIDs []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mailboxID = strings.TrimSpace(mailboxID)
	index := s.mailboxIndexLocked(mailboxID)
	if index < 0 {
		return 0, errors.New("邮箱不存在")
	}
	targets := make(map[string]bool, len(remoteIDs))
	for _, remoteID := range remoteIDs {
		if remoteID = strings.TrimSpace(remoteID); remoteID != "" {
			targets[remoteID] = true
		}
	}
	if len(targets) == 0 {
		return 0, nil
	}

	mailbox := &s.state.Mailboxes[index]
	next := make([]domain.Message, 0, len(s.state.Messages))
	removed := 0
	remaining := 0
	for _, message := range s.state.Messages {
		if message.MailboxID == mailboxID && targets[strings.TrimSpace(message.RemoteID)] {
			removed++
			if mailbox.LastCodeMessageID == message.ID {
				mailbox.LastCodeMessageID = ""
				mailbox.LastCodeAt = time.Time{}
			}
			continue
		}
		if message.MailboxID == mailboxID {
			remaining++
		}
		next = append(next, message)
	}
	if removed == 0 {
		return 0, nil
	}
	s.state.Messages = next
	mailbox.ReceiveCount = remaining
	mailbox.UpdatedAt = time.Now()
	s.appendEventLocked("info", "mailbox", fmt.Sprintf("已清理本地邮件缓存 %s：%d 封", mailbox.Email, removed))
	return removed, s.saveLocked()
}

func (s *Store) SetMailboxSyncCursor(id string, syncedAt time.Time, lastUID string) (domain.Mailbox, error) {
	if syncedAt.IsZero() {
		syncedAt = time.Now()
	}
	if _, err := s.SetMailboxSyncCursors(map[string]string{id: lastUID}, syncedAt); err != nil {
		return domain.Mailbox{}, err
	}
	mailbox, ok := s.FindMailboxByID(id)
	if !ok {
		return domain.Mailbox{}, errors.New("邮箱不存在")
	}
	return mailbox, nil
}

// SetMailboxSyncCursors updates several mailbox cursors with one state-file
// write. A sync round fetches one IMAP account for many aliases; persisting
// each cursor independently turns a single round into hundreds of fsyncs.
func (s *Store) SetMailboxSyncCursors(cursors map[string]string, syncedAt time.Time) (int, error) {
	if syncedAt.IsZero() {
		syncedAt = time.Now()
	}
	if len(cursors) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	indexes := make(map[string]int, len(cursors))
	for id := range cursors {
		index := s.mailboxIndexLocked(id)
		if index < 0 {
			return 0, errors.New("邮箱不存在")
		}
		indexes[id] = index
	}
	updated := 0
	changed := false
	for id, lastUID := range cursors {
		mailbox := &s.state.Mailboxes[indexes[id]]
		uid := strings.TrimSpace(lastUID)
		if mailbox.LastSyncAt.Equal(syncedAt) && mailbox.LastSyncUID == uid {
			continue
		}
		mailbox.LastSyncAt = syncedAt
		mailbox.LastSyncUID = uid
		mailbox.UpdatedAt = now
		updated++
		changed = true
	}
	if !changed {
		return updated, nil
	}
	return updated, s.saveLocked()
}

// ApplyMailboxSync atomically applies fetched messages and cursors for one or
// more mailboxes. The variadic timestamp keeps compatibility with callers
// that provide one round timestamp while allowing each update to carry its
// own timestamp when needed.
func (s *Store) ApplyMailboxSync(updates []MailboxSyncUpdate, roundTimes ...time.Time) (int, error) {
	if len(updates) == 0 {
		return 0, nil
	}
	var roundAt time.Time
	if len(roundTimes) > 0 {
		roundAt = roundTimes[0]
	}
	if roundAt.IsZero() {
		roundAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	indexes := make(map[string]int, len(updates))
	mailboxIndexes := make(map[string]int, len(s.state.Mailboxes))
	for index, mailbox := range s.state.Mailboxes {
		mailboxIndexes[mailbox.ID] = index
	}
	for _, update := range updates {
		id := strings.TrimSpace(update.MailboxID)
		if id == "" {
			return 0, errors.New("邮箱不存在")
		}
		if _, seen := indexes[id]; seen {
			continue
		}
		index, ok := mailboxIndexes[id]
		if !ok {
			return 0, errors.New("邮箱不存在")
		}
		indexes[id] = index
	}

	// Validate all IDs before mutating. Keep the affected slices for a cheap
	// rollback if the final atomic state-file write fails.
	originalMessagesLen := len(s.state.Messages)
	originalNextID := s.state.NextID
	originalUpdatedAt := s.state.UpdatedAt
	originalMailboxes := make(map[int]domain.Mailbox, len(indexes))
	for _, index := range indexes {
		originalMailboxes[index] = s.state.Mailboxes[index]
	}
	existing := make(map[int]map[string]struct{}, len(indexes))
	for _, message := range s.state.Messages {
		remoteID := strings.TrimSpace(message.RemoteID)
		if remoteID == "" {
			continue
		}
		index, ok := mailboxIndexes[message.MailboxID]
		if !ok {
			continue
		}
		if existing[index] == nil {
			existing[index] = make(map[string]struct{})
		}
		existing[index][remoteID] = struct{}{}
	}

	now := time.Now()
	created := 0
	changed := false
	changedMailboxes := make(map[int]bool, len(indexes))
	for _, update := range updates {
		id := strings.TrimSpace(update.MailboxID)
		index := indexes[id]
		mailbox := &s.state.Mailboxes[index]
		remoteIDs := existing[index]
		if remoteIDs == nil {
			remoteIDs = make(map[string]struct{})
			existing[index] = remoteIDs
		}
		createdForUpdate := 0
		for _, incoming := range update.Messages {
			remoteID := strings.TrimSpace(incoming.RemoteID)
			if remoteID != "" {
				if _, duplicate := remoteIDs[remoteID]; duplicate {
					continue
				}
				remoteIDs[remoteID] = struct{}{}
			}
			receivedAt := incoming.ReceivedAt
			if receivedAt.IsZero() {
				receivedAt = now
			}
			s.state.Messages = append(s.state.Messages, domain.Message{
				ID: s.nextIDLocked("msg"), OwnerID: mailbox.OwnerID, MailboxID: mailbox.ID,
				RemoteID: remoteID, Source: strings.TrimSpace(incoming.Source),
				Subject: strings.TrimSpace(incoming.Subject), From: strings.TrimSpace(incoming.From),
				Body: incoming.Body, ReceivedAt: receivedAt, CreatedAt: now,
			})
			mailbox.ReceiveCount++
			created++
			createdForUpdate++
			changed = true
			changedMailboxes[index] = true
		}
		syncedAt := update.SyncedAt
		if syncedAt.IsZero() {
			syncedAt = roundAt
		}
		lastUID := strings.TrimSpace(update.LastSyncUID)
		if update.SkipIfUnchanged && createdForUpdate == 0 && mailbox.LastSyncUID == lastUID && !mailbox.LastSyncAt.IsZero() {
			// Keep the in-process freshness marker moving for throttling and
			// diagnostics, but avoid marking the state dirty. A later meaningful
			// update will persist the newest marker together with its changes.
			mailbox.LastSyncAt = syncedAt
			continue
		}
		if !mailbox.LastSyncAt.Equal(syncedAt) || mailbox.LastSyncUID != lastUID {
			mailbox.LastSyncAt = syncedAt
			mailbox.LastSyncUID = lastUID
			changed = true
			changedMailboxes[index] = true
		}
	}
	for index := range changedMailboxes {
		s.state.Mailboxes[index].UpdatedAt = now
	}
	if !changed {
		return 0, nil
	}
	if err := s.saveLocked(); err != nil {
		s.state.Messages = s.state.Messages[:originalMessagesLen]
		s.state.NextID = originalNextID
		s.state.UpdatedAt = originalUpdatedAt
		for index, mailbox := range originalMailboxes {
			s.state.Mailboxes[index] = mailbox
		}
		return 0, err
	}
	return created, nil
}

func (s *Store) SetMailboxLastCode(id, messageID string, servedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.mailboxIndexLocked(id)
	if index < 0 {
		return errors.New("邮箱不存在")
	}
	s.state.Mailboxes[index].LastCodeMessageID = strings.TrimSpace(messageID)
	s.state.Mailboxes[index].LastCodeAt = servedAt
	s.state.Mailboxes[index].UpdatedAt = time.Now()
	return s.saveLocked()
}

// ClaimMailboxCode atomically marks a verification message as served only when
// the mailbox cursor still matches the caller's snapshot. Concurrent consumers
// therefore cannot receive the same message.
func (s *Store) ClaimMailboxCode(id, expectedLastMessageID, messageID string, servedAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.mailboxIndexLocked(id)
	if index < 0 {
		return false, errors.New("邮箱不存在")
	}
	expectedLastMessageID = strings.TrimSpace(expectedLastMessageID)
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return false, errors.New("验证码邮件 ID 不能为空")
	}
	mailbox := &s.state.Mailboxes[index]
	currentMessageID := strings.TrimSpace(mailbox.LastCodeMessageID)
	if currentMessageID != expectedLastMessageID || currentMessageID == messageID {
		return false, nil
	}
	messageExists := false
	for _, message := range s.state.Messages {
		if message.ID == messageID && message.MailboxID == mailbox.ID {
			messageExists = true
			break
		}
	}
	if !messageExists {
		return false, errors.New("验证码邮件不存在")
	}
	original := *mailbox
	mailbox.LastCodeMessageID = messageID
	mailbox.LastCodeAt = servedAt
	mailbox.UpdatedAt = time.Now()
	if err := s.saveLocked(); err != nil {
		*mailbox = original
		return false, err
	}
	return true, nil
}

func (s *Store) mailboxIndexLocked(id string) int {
	id = strings.TrimSpace(id)
	for i := range s.state.Mailboxes {
		if s.state.Mailboxes[i].ID == id {
			return i
		}
	}
	return -1
}

func randomAPIToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
