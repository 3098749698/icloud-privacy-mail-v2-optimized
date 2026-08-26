package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestExportedMailboxCandidatesStrictlyFiltersStatusExportAndLeases(t *testing.T) {
	base := time.Date(2026, time.August, 22, 10, 0, 0, 0, time.UTC)
	state := domain.State{
		Mailboxes: []domain.Mailbox{
			{ID: "eligible_old", AccountID: "acc_a", Email: "old@example.test", Status: domain.StatusUsed, ExportedAt: base},
			{ID: "available_exported", AccountID: "acc_a", Email: "available@example.test", Status: domain.StatusAvailable, ExportedAt: base},
			{ID: "legacy_used", AccountID: "acc_a", Email: "legacy@example.test", Status: domain.StatusUsed},
			{ID: "active_lease", AccountID: "acc_a", Email: "active@example.test", Status: domain.StatusUsed, ExportedAt: base.Add(time.Minute), ActiveLeaseID: "lease_active"},
			{ID: "claimed_lease", AccountID: "acc_a", Email: "claimed@example.test", Status: domain.StatusUsed, ExportedAt: base.Add(2 * time.Minute)},
			{ID: "released_lease", AccountID: "acc_a", Email: "released@example.test", Status: domain.StatusUsed, ExportedAt: base.Add(3 * time.Minute)},
			{ID: "other_account", AccountID: "acc_b", Email: "other@example.test", Status: domain.StatusUsed, ExportedAt: base.Add(4 * time.Minute)},
		},
		MailboxLeases: []domain.MailboxLease{
			{ID: "lease_claimed", MailboxID: "claimed_lease", State: domain.MailboxLeaseClaimed},
			{ID: "lease_released", MailboxID: "released_lease", State: domain.MailboxLeaseReleased},
		},
	}

	candidates := exportedMailboxCandidates(state, "acc_a", 0, false)
	if len(candidates) != 2 || candidates[0].ID != "eligible_old" || candidates[1].ID != "released_lease" {
		t.Fatalf("候选过滤不正确：%+v", candidates)
	}
	limited := exportedMailboxCandidates(state, "", 2, false)
	if len(limited) != 2 || limited[0].ID != "eligible_old" || limited[1].ID != "released_lease" {
		t.Fatalf("候选排序或数量上限不正确：%+v", limited)
	}
}

func TestExportedMailboxCandidatesIncludesLegacyUsedOnlyWhenExplicit(t *testing.T) {
	state := domain.State{Mailboxes: []domain.Mailbox{{
		ID: "legacy_used", AccountID: "acc_a", Email: "legacy@example.test", Status: domain.StatusUsed,
	}}}
	if got := exportedMailboxCandidates(state, "acc_a", 0, false); len(got) != 0 {
		t.Fatalf("默认模式选中了旧版已使用记录：%+v", got)
	}
	got := exportedMailboxCandidates(state, "acc_a", 0, true)
	if len(got) != 1 || got[0].ID != "legacy_used" {
		t.Fatalf("显式兼容模式没有选中旧版已使用记录：%+v", got)
	}
}

func TestProcessCandidatesDefaultsToDryRun(t *testing.T) {
	candidate := domain.Mailbox{
		ID:         "mailbox_1",
		AccountID:  "account_1",
		Email:      "mailbox@example.test",
		Status:     domain.StatusUsed,
		ExportedAt: time.Now(),
	}
	deleteCalls := 0
	var output bytes.Buffer
	summary, err := processCandidates(context.Background(), []domain.Mailbox{candidate}, maintenanceOptions{}, func(context.Context, string) error {
		deleteCalls++
		return nil
	}, &output)
	if err != nil {
		t.Fatalf("dry-run 返回错误：%v", err)
	}
	if deleteCalls != 0 || summary.Attempted != 0 || summary.Deleted != 0 || summary.Candidates != 1 {
		t.Fatalf("dry-run 意外执行删除：calls=%d summary=%+v", deleteCalls, summary)
	}
	if !strings.Contains(output.String(), "模式：预览") || !strings.Contains(output.String(), "未执行远端删除") {
		t.Fatalf("dry-run 输出不明确：%s", output.String())
	}
}

func TestSafeErrorTextHidesSensitiveResponses(t *testing.T) {
	message := safeErrorText(assertionError("request Cookie: session-secret"))
	if strings.Contains(message, "session-secret") {
		t.Fatalf("错误输出泄漏敏感内容：%s", message)
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }
