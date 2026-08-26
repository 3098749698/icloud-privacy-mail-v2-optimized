package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/buildinfo"
	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/scheduler"
	"icloud-privacy-mail-v2/internal/store"
)

const publicCodePageContextKey contextKey = "public-code-page"

func (s *Server) handlePublicHealth(w http.ResponseWriter, r *http.Request) {
	current := buildinfo.Current()
	if s.globalAPIKey() != "" && !s.authorizedGlobalAPI(r) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "API Key 错误")
		return
	}
	active := false
	for _, session := range s.store.ICloudSessions() {
		if session.IsICloudPlus && session.CanCreateHME && len(session.Cookies) > 0 {
			active = true
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"service":                    "icloud-privacy-mail-v2",
			"version":                    current.Version,
			"commit":                     current.Commit,
			"api_active":                 s.store.Settings().EnablePublicMailboxAPI && s.globalAPIKey() != "",
			"icloud_active":              active,
			"lease_api_version":          1,
			"lease_ttl_seconds":          s.cfg.PublicMailboxLeaseTTLMinutes * 60,
			"lease_max_ttl_seconds":      s.cfg.PublicMailboxLeaseMaxTTLMinutes * 60,
			"mailbox_note_api_supported": true,
			"time":                       time.Now().Format(time.RFC3339),
		},
	})
}

func (s *Server) handlePublicClaimMailbox(w http.ResponseWriter, r *http.Request) {
	if !s.store.Settings().EnablePublicMailboxAPI {
		writeError(w, http.StatusForbidden, "public_api_disabled", "公共取号 API 尚未开启")
		return
	}
	if !s.authorizedGlobalAPI(r) {
		writeError(w, http.StatusUnauthorized, "global_api_key_required", "自动取号需要提交全局 API Key")
		return
	}
	var body struct {
		Project    string `json:"project"`
		Purpose    string `json:"purpose"`
		RequestID  string `json:"request_id"`
		Note       string `json:"note"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	mailbox, lease, created, err := s.store.ClaimMailboxLease(
		body.Project,
		body.Purpose,
		body.RequestID,
		body.Note,
		s.mailboxLeaseTTL(body.TTLSeconds),
		time.Now(),
	)
	if err != nil {
		if errors.Is(err, store.ErrNoAvailableMailbox) {
			writeError(w, http.StatusOK, "no_available_mailbox", err.Error())
			return
		}
		writeMailboxLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"mailbox":    s.publicMailbox(r, mailbox, true),
		"lease":      s.publicMailboxLease(lease),
		"created":    created,
		"idempotent": !created,
	}})
}

func (s *Server) handlePublicLookupMailboxes(w http.ResponseWriter, r *http.Request) {
	if !s.store.Settings().EnablePublicMailboxAPI {
		writeError(w, http.StatusForbidden, "public_api_disabled", "公共取号 API 尚未开启")
		return
	}
	if !s.authorizedGlobalAPI(r) {
		writeError(w, http.StatusUnauthorized, "global_api_key_required", "查询邮箱需要提交全局 API Key")
		return
	}
	var body struct {
		Emails []string `json:"emails"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if len(body.Emails) == 0 || len(body.Emails) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_email_count", "邮箱数量必须是 1-500")
		return
	}
	seen := map[string]bool{}
	items := make([]map[string]any, 0, len(body.Emails))
	missing := make([]string, 0)
	for _, raw := range body.Emails {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		mailbox, ok := s.store.FindMailboxByEmail(email)
		if !ok {
			missing = append(missing, email)
			continue
		}
		items = append(items, s.publicMailbox(r, mailbox, true))
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": items, "missing": missing}})
}

func (s *Server) handlePublicMailboxCode(w http.ResponseWriter, r *http.Request) {
	diagnostics := newCodeDiagnostics()
	fromPublicPage, _ := r.Context().Value(publicCodePageContextKey).(bool)
	settings := s.store.Settings()
	if fromPublicPage {
		if !settings.EnablePublicCodePage {
			writeError(w, http.StatusForbidden, "public_code_page_disabled", "公共验证码页面尚未开启")
			return
		}
	} else if !settings.EnablePublicMailboxAPI {
		writeError(w, http.StatusForbidden, "public_api_disabled", "公共取号 API 尚未开启")
		return
	}
	email, err := url.PathUnescape(r.PathValue("email"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_email", "邮箱地址格式不正确")
		return
	}
	mailbox, ok := s.store.FindMailboxByEmail(email)
	if !ok {
		writeError(w, http.StatusNotFound, "mailbox_not_found", "邮箱不存在")
		return
	}
	if !fromPublicPage && !s.authorizedMailboxAPI(r, mailbox) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "API Key 错误")
		return
	}
	if !mailbox.AutoDeleteAt.IsZero() && !time.Now().Before(mailbox.AutoDeleteAt) {
		writeError(w, http.StatusGone, "mailbox_expired", "邮箱有效期已结束，正在回收")
		return
	}
	if !mailbox.APIActive || mailbox.Status == domain.StatusDisabled {
		writeError(w, http.StatusForbidden, "api_disabled", "邮箱 API 已停用")
		return
	}
	if !mailbox.ICloudActive {
		writeError(w, http.StatusForbidden, "icloud_inactive", "邮箱在 iCloud 中已停用")
		return
	}
	after, err := parseRFC3339(r.URL.Query().Get("after"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_after", err.Error())
		return
	}
	waitMS := s.cfg.PublicFastSyncWaitMS
	if raw := strings.TrimSpace(r.URL.Query().Get("wait_ms")); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil {
			waitMS = parsed
		}
	}
	if waitMS < 0 {
		waitMS = 0
	}
	if waitMS > 30000 {
		waitMS = 30000
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	if keyword == "" {
		keyword = "OpenAI"
	}
	allowStale := parseBool(r.URL.Query().Get("allow_stale"))
	cacheOnly := parseBool(r.URL.Query().Get("cache"))
	peekOnly := parseBool(r.URL.Query().Get("peek")) || parseBool(r.URL.Query().Get("preview"))
	query := mailboxservice.CodeQuery{
		After:         after,
		Keyword:       keyword,
		SkipMessageID: mailbox.LastCodeMessageID,
		IncludeServed: cacheOnly || peekOnly,
		MarkAsServed:  !cacheOnly && !peekOnly,
		AllowStale:    false,
	}
	if result, found, lookupErr := s.mailbox.CachedCodeWithQuery(mailbox.ID, query); lookupErr != nil {
		writeServiceError(w, lookupErr)
		return
	} else if found {
		s.writeObservedCodeSuccess(w, result, diagnostics, "cache", mailbox.LastSyncAt)
		return
	}
	if cacheOnly {
		writeCodeError(w, http.StatusOK, "no_code", "本地缓存中暂无验证码", diagnostics.meta("cache", mailbox.LastSyncAt))
		return
	}
	if waitMS == 0 {
		writeCodeError(w, http.StatusOK, "no_code", "暂未收到验证码", diagnostics.meta("cache", mailbox.LastSyncAt))
		return
	}
	if s.observeMailbox != nil {
		s.observeMailbox(mailbox.ID)
	}
	changes := s.mailbox.CodeChanges(mailbox.ID)
	if s.codeChanges != nil {
		changes = s.codeChanges(mailbox.ID)
	}
	// Subscribe before the second lookup so a message committed between the
	// first cache check and subscription cannot be missed.
	if result, found, lookupErr := s.mailbox.CachedCodeWithQuery(mailbox.ID, query); lookupErr != nil {
		writeServiceError(w, lookupErr)
		return
	} else if found {
		s.writeObservedCodeSuccess(w, result, diagnostics, "cache-after-subscribe", mailbox.LastSyncAt)
		return
	}
	idleReady := false
	if s.mailboxIdleReady != nil {
		idleReady = s.mailboxIdleReady(mailbox.ID)
	}
	diagnostics.idleReady = idleReady
	diagnostics.fetchMode = "idle-targeted"
	var fallbackDone <-chan error
	if !idleReady {
		diagnostics.fetchMode = "fallback"
		diagnostics.beginSync()
		fallbackDone = s.startCodeFallback(mailbox)
	}
	deadline := time.NewTimer(time.Duration(waitMS) * time.Millisecond)
	defer deadline.Stop()
	localSafetyCheck := time.NewTicker(100 * time.Millisecond)
	defer localSafetyCheck.Stop()
	var syncErr error
	lookup := func(source string) bool {
		latestMailbox, _ := s.store.FindMailboxByID(mailbox.ID)
		result, found, lookupErr := s.mailbox.CachedCodeWithQuery(mailbox.ID, query)
		if lookupErr != nil {
			writeServiceError(w, lookupErr)
			return true
		}
		if found {
			s.writeObservedCodeSuccess(w, result, diagnostics, source, latestMailbox.LastSyncAt)
			return true
		}
		return false
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			diagnostics.endSync()
			latestMailbox, _ := s.store.FindMailboxByID(mailbox.ID)
			if syncErr != nil && !allowStale {
				writeCodeError(w, http.StatusBadGateway, "mail_sync_failed", "同步验证码邮件失败，请检查 IMAP 或 iCloud 登录态后重试", diagnostics.meta("sync", latestMailbox.LastSyncAt))
				return
			}
			if allowStale && syncErr != nil {
				if result, found, lookupErr := s.staleCachedCode(mailbox.ID, query); lookupErr != nil {
					writeServiceError(w, lookupErr)
					return
				} else if found {
					writeCodeSuccess(w, http.StatusOK, staleCodeData(result), diagnostics.meta("stale-cache", latestMailbox.LastSyncAt))
					return
				}
			}
			if syncErr != nil {
				writeCodeError(w, http.StatusBadGateway, "mail_sync_failed", "同步验证码邮件失败，请检查 IMAP 或 iCloud 登录态后重试", diagnostics.meta("sync", latestMailbox.LastSyncAt))
				return
			}
			writeCodeError(w, http.StatusOK, "no_code", "暂未收到验证码", diagnostics.meta("sync", latestMailbox.LastSyncAt))
			return
		case syncErr = <-fallbackDone:
			diagnostics.endSync()
			fallbackDone = nil
			if lookup("fallback") {
				return
			}
			if syncErr != nil && allowStale {
				latestMailbox, _ := s.store.FindMailboxByID(mailbox.ID)
				if result, found, lookupErr := s.staleCachedCode(mailbox.ID, query); lookupErr != nil {
					writeServiceError(w, lookupErr)
					return
				} else if found {
					writeCodeSuccess(w, http.StatusOK, staleCodeData(result), diagnostics.meta("stale-cache", latestMailbox.LastSyncAt))
					return
				}
			}
			// A direct scan can fail while an already authenticated IDLE worker is
			// still able to cache the incoming message. Keep polling until the
			// caller's deadline instead of turning the first transport error into
			// a terminal result.
		case <-changes:
			if s.codeChanges != nil {
				changes = s.codeChanges(mailbox.ID)
			} else {
				changes = s.mailbox.CodeChanges(mailbox.ID)
			}
			if lookup("idle-targeted") {
				return
			}
		case <-localSafetyCheck.C:
			if lookup(diagnostics.fetchMode) {
				return
			}
		}
	}
}

func (s *Server) startCodeFallback(mailbox domain.Mailbox) <-chan error {
	key := firstNonEmpty(mailbox.AccountID, mailbox.OwnerID, mailbox.ID)
	now := time.Now()
	s.codeFallbackMu.Lock()
	if running := s.codeFallbacks[key]; running != nil {
		done := running.done
		s.codeFallbackMu.Unlock()
		return done
	}
	if last := s.codeFallbackLast[key]; !last.IsZero() && now.Sub(last) < 10*time.Second {
		done := make(chan error, 1)
		done <- nil
		close(done)
		s.codeFallbackMu.Unlock()
		return done
	}
	call := &codeFallbackCall{done: make(chan error, 1)}
	s.codeFallbacks[key] = call
	s.codeFallbackLast[key] = now
	s.codeFallbackMu.Unlock()
	go func() {
		syncCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		syncFresh := s.syncMailboxFresh
		if syncFresh == nil {
			syncFresh = s.mailbox.SyncMessagesFresh
		}
		_, syncErr := syncFresh(syncCtx, mailbox.ID)
		call.done <- syncErr
		close(call.done)
		s.codeFallbackMu.Lock()
		delete(s.codeFallbacks, key)
		s.codeFallbackMu.Unlock()
	}()
	return call.done
}

func (s *Server) handlePublicCodePageStatus(w http.ResponseWriter, _ *http.Request) {
	settings := s.store.Settings()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"enabled": settings.EnablePublicCodePage,
			"route":   "/verification-code",
		},
	})
}

func (s *Server) handlePublicCodePageLookup(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if email == "" || !strings.Contains(email, "@") {
		writeError(w, http.StatusBadRequest, "invalid_email", "请输入完整的邮箱地址")
		return
	}
	request := r.Clone(context.WithValue(r.Context(), publicCodePageContextKey, true))
	request.SetPathValue("email", email)
	s.handlePublicMailboxCode(w, request)
}

func (s *Server) staleCachedCode(mailboxID string, query mailboxservice.CodeQuery) (mailboxservice.CodeResult, bool, error) {
	query.IncludeServed = true
	query.MarkAsServed = false
	query.SkipMessageID = ""
	query.AllowStale = true
	return s.mailbox.CachedCodeWithQuery(mailboxID, query)
}

func staleCodeData(result mailboxservice.CodeResult) map[string]any {
	return map[string]any{
		"email": result.Email, "code": result.Code, "subject": result.Subject, "from": result.From,
		"received_at": result.ReceivedAt, "message_date_at": result.MessageDateAt,
		"provider_received_at": result.ProviderReceivedAt, "detected_at": result.DetectedAt, "delivery_delay_ms": result.DeliveryDelayMS,
		"api_returned_at": time.Now(),
		"message_id":      result.MessageID, "folder": result.Folder, "stale_cache": true,
		"sync_error": "同步验证码邮件失败，当前验证码来自本地缓存",
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (s *Server) handleSchedulerStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"scheduler": s.scheduler.Snapshot(),
		"defaults":  scheduler.DefaultConfig(s.store),
	}})
}

func (s *Server) handleSchedulerStart(w http.ResponseWriter, r *http.Request) {
	var cfg scheduler.Config
	if err := decodeJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	state, err := s.scheduler.Start(s.runtimeCtx, cfg)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": state}})
}

func (s *Server) handleSchedulerStop(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": s.scheduler.Stop("已手动停止定时创建")}})
}

func (s *Server) handleSchedulerAddAccounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AccountIDs []string `json:"account_ids"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	state, err := s.scheduler.AddAccounts(request.AccountIDs)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": state}})
}

func (s *Server) handleSchedulerRemoveAccounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AccountIDs []string `json:"account_ids"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	state, err := s.scheduler.RemoveAccounts(request.AccountIDs)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": state}})
}

func (s *Server) handleSchedulerTaskStart(w http.ResponseWriter, r *http.Request) {
	var cfg scheduler.Config
	if err := decodeJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	task, err := s.scheduler.StartAccount(s.runtimeCtx, r.PathValue("account_id"), cfg)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"task": task, "scheduler": s.scheduler.Snapshot()}})
}

func (s *Server) handleSchedulerTaskPause(w http.ResponseWriter, r *http.Request) {
	task, err := s.scheduler.StopAccount(r.PathValue("account_id"), "账号创建任务已手动暂停")
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"task": task, "scheduler": s.scheduler.Snapshot()}})
}

func (s *Server) handleSchedulerTaskResume(w http.ResponseWriter, r *http.Request) {
	task, err := s.scheduler.ResumeAccount(s.runtimeCtx, r.PathValue("account_id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"task": task, "scheduler": s.scheduler.Snapshot()}})
}

func (s *Server) handleSchedulerTaskDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.scheduler.DeleteAccountTask(r.PathValue("account_id")); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": s.scheduler.Snapshot()}})
}

func (s *Server) handleSchedulerClearLogs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": s.scheduler.ClearEvents()}})
}

func (s *Server) handleExportRuntime(w http.ResponseWriter, r *http.Request) {
	state := s.store.Snapshot()
	payload := struct {
		SchemaVersion  int                    `json:"schema_version"`
		ExportedAt     time.Time              `json:"exported_at"`
		AppleAccounts  []domain.AppleAccount  `json:"apple_accounts"`
		Mailboxes      []domain.Mailbox       `json:"mailboxes"`
		MailboxLeases  []domain.MailboxLease  `json:"mailbox_leases"`
		SchedulerTasks []domain.SchedulerTask `json:"scheduler_tasks"`
		ICloudSessions []domain.ICloudSession `json:"icloud_sessions"`
		CreateSettings domain.CreateSettings  `json:"create_settings"`
		Settings       domain.Settings        `json:"settings"`
		MessageCount   int                    `json:"message_count"`
		Messages       []domain.Message       `json:"messages,omitempty"`
	}{
		SchemaVersion: state.SchemaVersion, ExportedAt: time.Now(), AppleAccounts: state.AppleAccounts,
		Mailboxes: state.Mailboxes, MailboxLeases: state.MailboxLeases, SchedulerTasks: state.SchedulerTasks, ICloudSessions: state.ICloudSessions, CreateSettings: state.CreateSettings,
		Settings: state.Settings, MessageCount: len(state.Messages),
	}
	if parseBool(r.URL.Query().Get("include_messages")) {
		payload.Messages = state.Messages
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "export_failed", err.Error())
		return
	}
	if err := writeDownload(w, "application/json; charset=utf-8", "icloud-privacy-mail-state-"+time.Now().Format("20060102-150405")+".json", append(data, '\n')); err != nil {
		s.log.Warn("运行数据导出响应写入失败", "错误", err)
	}
}

func (s *Server) handleExportMailboxAPIs(w http.ResponseWriter, r *http.Request) {
	s.writeMailboxExport(w, r, true)
}

func (s *Server) handleExportMailboxEmails(w http.ResponseWriter, r *http.Request) {
	s.writeMailboxExport(w, r, false)
}

func (s *Server) writeMailboxExport(w http.ResponseWriter, r *http.Request, includeAPI bool) {
	request := struct {
		Format    string `json:"format"`
		AccountID string `json:"account_id"`
		Count     int    `json:"count"`
		ValidDays int    `json:"valid_days"`
	}{
		Format:    r.URL.Query().Get("format"),
		AccountID: r.URL.Query().Get("account_id"),
	}
	if rawCount := strings.TrimSpace(r.URL.Query().Get("count")); rawCount != "" {
		parsed, err := strconv.Atoi(rawCount)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_export_count", "导出数量必须是整数")
			return
		}
		request.Count = parsed
	}
	if rawValidDays := strings.TrimSpace(r.URL.Query().Get("valid_days")); rawValidDays != "" {
		parsed, err := strconv.Atoi(rawValidDays)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_valid_days", "有效天数必须是整数")
			return
		}
		request.ValidDays = parsed
	}
	if r.Method == http.MethodPost {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	format := strings.ToLower(strings.TrimSpace(request.Format))
	if format == "" {
		format = "txt"
	}
	switch format {
	case "txt", "csv", "tsv", "jsonl":
	default:
		writeError(w, http.StatusBadRequest, "invalid_export_format", "导出格式只支持 txt、csv、tsv、jsonl")
		return
	}
	if request.Count <= 0 || request.Count > 10000 {
		writeError(w, http.StatusBadRequest, "invalid_export_count", "导出数量范围为 1 到 10000")
		return
	}
	if request.ValidDays < 0 || request.ValidDays > 3650 {
		writeError(w, http.StatusBadRequest, "invalid_valid_days", "有效天数范围为 0 到 3650，0 表示永久有效")
		return
	}
	if request.ValidDays > 0 {
		if strings.TrimSpace(request.AccountID) == "" {
			writeError(w, http.StatusBadRequest, "auto_delete_account_required", "限时导出必须选择一个 Apple 账号，不能使用全部账号")
			return
		}
		if err := s.mailbox.ValidateAutoDeleteAccount(request.AccountID); err != nil {
			writeError(w, http.StatusConflict, "auto_delete_unavailable", err.Error())
			return
		}
	}
	exportedAt := time.Now()
	validFor := time.Duration(request.ValidDays) * 24 * time.Hour
	mailboxes, err := s.store.ExportAvailableMailboxesWithValidity(request.AccountID, request.Count, exportedAt, validFor)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	rows := make([][]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		row := []string{mailbox.Email}
		if includeAPI {
			row = append(row, s.mailboxAPIURL(r, mailbox))
		}
		rows = append(rows, row)
	}
	var body strings.Builder
	ext := "txt"
	contentType := "text/plain; charset=utf-8"
	switch format {
	case "txt":
		separator := "\n"
		if includeAPI {
			separator = "----"
		}
		for _, row := range rows {
			body.WriteString(strings.Join(row, separator))
			body.WriteByte('\n')
		}
	case "csv", "tsv":
		ext = format
		contentType = "text/csv; charset=utf-8"
		writer := csv.NewWriter(&body)
		if format == "tsv" {
			writer.Comma = '\t'
			contentType = "text/tab-separated-values; charset=utf-8"
		}
		for _, row := range rows {
			_ = writer.Write(row)
		}
		writer.Flush()
	case "jsonl":
		ext = "jsonl"
		contentType = "application/x-ndjson; charset=utf-8"
		for _, row := range rows {
			record := map[string]string{"email": row[0]}
			if includeAPI {
				record["api"] = row[1]
			}
			data, _ := json.Marshal(record)
			body.Write(data)
			body.WriteByte('\n')
		}
	}
	prefix := "icloud-mailbox-emails"
	if includeAPI {
		prefix = "icloud-mailbox-apis"
	}
	w.Header().Set("X-Export-Count", strconv.Itoa(len(rows)))
	w.Header().Set("X-Export-Valid-Days", strconv.Itoa(request.ValidDays))
	if request.ValidDays > 0 {
		w.Header().Set("X-Export-Expires-At", exportedAt.Add(validFor).Format(time.RFC3339))
	}
	if err := writeDownload(w, contentType, fmt.Sprintf("%s-%d-%s.%s", prefix, len(rows), time.Now().Format("20060102-150405"), ext), []byte(body.String())); err != nil {
		rolledBack, rollbackErr := s.store.RollbackMailboxExport(mailboxes, exportedAt)
		if rollbackErr != nil {
			s.log.Error("导出响应失败且邮箱库存回滚失败", "错误", err, "回滚错误", rollbackErr)
			return
		}
		s.log.Warn("导出响应写入失败，邮箱已退回可用池", "数量", rolledBack, "错误", err)
	}
}

func writeDownload(w http.ResponseWriter, contentType, filename string, body []byte) error {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(body)
	return err
}

func (s *Server) publicMailbox(r *http.Request, mailbox domain.Mailbox, includeAPI bool) map[string]any {
	out := map[string]any{
		"id": mailbox.ID, "account_id": mailbox.AccountID, "label": mailbox.Label, "email": mailbox.Email,
		"api_active": mailbox.APIActive, "icloud_active": mailbox.ICloudActive, "status": mailbox.Status,
		"note": mailbox.Note, "active_lease_id": mailbox.ActiveLeaseID, "receive_count": mailbox.ReceiveCount, "last_sync_at": mailbox.LastSyncAt,
	}
	if !mailbox.ExportedAt.IsZero() {
		out["exported_at"] = formatTime(mailbox.ExportedAt)
	}
	if !mailbox.AutoDeleteAt.IsZero() {
		out["auto_delete_at"] = formatTime(mailbox.AutoDeleteAt)
	}
	if includeAPI {
		out["api_url"] = s.mailboxAPIURL(r, mailbox)
		out["api_token_mask"] = maskSecret(mailbox.APIToken)
	}
	return out
}

func (s *Server) mailboxAPIURL(r *http.Request, mailbox domain.Mailbox) string {
	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/")
	if baseURL == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	// Exported links are normally opened by a client that has no separate
	// retry loop. Give the server enough time to complete one IMAP sync and use
	// preview mode so refreshing the same link can read the cached code again.
	// Stale-cache fallback remains opt-in; a previously issued code should not
	// silently replace a newer code for a fresh request.
	return fmt.Sprintf("%s/api/v1/mailboxes/%s/code?key=%s&wait_ms=30000&preview=1", baseURL, url.PathEscape(mailbox.Email), url.QueryEscape(mailbox.APIToken))
}

func (s *Server) authorizedGlobalAPI(r *http.Request) bool {
	want := s.globalAPIKey()
	if want == "" {
		return false
	}
	return anySecretEqual(want, r.URL.Query().Get("key"), r.Header.Get("X-API-Key"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func (s *Server) authorizedMailboxAPI(r *http.Request, mailbox domain.Mailbox) bool {
	candidates := []string{r.URL.Query().Get("key"), r.Header.Get("X-API-Key"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	if anySecretEqual(mailbox.APIToken, candidates...) {
		return true
	}
	globalKey := s.globalAPIKey()
	return globalKey != "" && anySecretEqual(globalKey, candidates...)
}

func (s *Server) globalAPIKey() string {
	if value := strings.TrimSpace(s.store.Settings().PublicAPIKey); value != "" {
		return value
	}
	return strings.TrimSpace(s.cfg.APIKey)
}

func anySecretEqual(want string, candidates ...string) bool {
	want = strings.TrimSpace(want)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if want != "" && len(candidate) == len(want) && subtle.ConstantTimeCompare([]byte(candidate), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

func maskSecret(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return strings.Repeat("*", len(value))
	}
	return value[:4] + strings.Repeat("*", len(value)-8) + value[len(value)-4:]
}
