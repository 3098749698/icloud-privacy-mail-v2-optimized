package httpapi

import (
	"net/http"
	"time"

	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
)

// codeDiagnostics is deliberately kept at the HTTP boundary. It exposes
// timing/source information useful to the UI without exposing IMAP credentials
// or provider error payloads.
type codeDiagnostics struct {
	startedAt     time.Time
	syncStartedAt time.Time
	syncElapsed   time.Duration
}

func newCodeDiagnostics() codeDiagnostics {
	return codeDiagnostics{startedAt: time.Now()}
}

func (d *codeDiagnostics) beginSync() {
	if d.syncStartedAt.IsZero() {
		d.syncStartedAt = time.Now()
	}
}

func (d *codeDiagnostics) endSync() {
	if d.syncStartedAt.IsZero() || d.syncElapsed > 0 {
		return
	}
	d.syncElapsed = time.Since(d.syncStartedAt)
}

func (d codeDiagnostics) meta(source string, lastSyncAt time.Time) map[string]any {
	if d.startedAt.IsZero() {
		d.startedAt = time.Now()
	}
	if d.syncElapsed <= 0 && !d.syncStartedAt.IsZero() {
		d.syncElapsed = time.Since(d.syncStartedAt)
	}
	meta := map[string]any{
		"source":          source,
		"elapsed_ms":      elapsedMilliseconds(time.Since(d.startedAt)),
		"sync_elapsed_ms": elapsedMilliseconds(d.syncElapsed),
		"last_sync_at":    nil,
	}
	if !lastSyncAt.IsZero() {
		meta["last_sync_at"] = lastSyncAt
	}
	return meta
}

func elapsedMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

func writeCodeSuccess(w http.ResponseWriter, status int, data any, meta map[string]any) {
	writeJSON(w, status, map[string]any{"success": true, "data": data, "meta": meta})
}

func (s *Server) writeObservedCodeSuccess(w http.ResponseWriter, result mailboxservice.CodeResult, diagnostics codeDiagnostics, source string, lastSyncAt time.Time) {
	meta := diagnostics.meta(source, lastSyncAt)
	s.log.Info("验证码取码时间线",
		"邮箱", result.Email,
		"目录", result.Folder,
		"邮件到达", result.ReceivedAt,
		"程序读到", result.DetectedAt,
		"读取延迟_ms", result.DeliveryDelayMS,
		"接口来源", source,
		"接口耗时_ms", meta["elapsed_ms"],
		"同步耗时_ms", meta["sync_elapsed_ms"],
	)
	writeCodeSuccess(w, http.StatusOK, result, meta)
}

func writeCodeError(w http.ResponseWriter, status int, code, message string, meta map[string]any) {
	payload := map[string]any{"success": false, "code": code, "message": message, "meta": meta}
	if code == "no_code" || code == "mail_sync_failed" {
		payload["retryable"] = true
		payload["retry_after_ms"] = 2000
	}
	writeJSON(w, status, payload)
}
