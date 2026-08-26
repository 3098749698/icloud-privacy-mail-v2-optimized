package protocol

import (
	"context"
	"time"
)

type IMAPSyncResult struct {
	MessagesByMailbox map[string][]ICloudSyncedMessage
	LastUID           string
	LastJunkUID       string
}

const (
	IMAPSyncFolderAll   = ""
	IMAPSyncFolderInbox = "inbox"
	IMAPSyncFolderJunk  = "junk"
)

func SyncICloudIMAPMessagesDetailed(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int) (IMAPSyncResult, error) {
	result, err := SyncICloudIMAPMessagesWithCursor(ctx, state, mailboxes, after, keyword, maxMessages)
	return IMAPSyncResult{MessagesByMailbox: result.MessagesByMailbox, LastUID: result.LastUID, LastJunkUID: result.LastJunkUID}, err
}

func SyncICloudIMAPMessagesFolderDetailed(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int, folder string) (IMAPSyncResult, error) {
	result, err := syncICloudIMAPMessagesWithCursorFolder(ctx, state, mailboxes, after, keyword, maxMessages, folder)
	return IMAPSyncResult{MessagesByMailbox: result.MessagesByMailbox, LastUID: result.LastUID, LastJunkUID: result.LastJunkUID}, err
}
