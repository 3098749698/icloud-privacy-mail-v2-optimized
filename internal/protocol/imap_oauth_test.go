package protocol

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"
)

func TestIMAPAuthenticateXOAUTH2(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	command := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(server)
		line, _ := reader.ReadString('\n')
		command <- strings.TrimSpace(line)
		_, _ = server.Write([]byte("A001 OK AUTHENTICATE completed\r\n"))
	}()

	state := LoginState{IMAPUsername: "owner@outlook.com", IMAPAuthMethod: "xoauth2", OAuthAccessToken: "ACCESS_TOKEN"}
	if err := imapAuthenticate(client, bufio.NewReader(client), "A001", state); err != nil {
		t.Fatalf("imapAuthenticate() error = %v", err)
	}
	parts := strings.Fields(<-command)
	if len(parts) != 4 || parts[0] != "A001" || parts[1] != "AUTHENTICATE" || parts[2] != "XOAUTH2" {
		t.Fatalf("unexpected AUTHENTICATE command: %q", strings.Join(parts, " "))
	}
	payload, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("decode XOAUTH2 payload: %v", err)
	}
	want := "user=owner@outlook.com\x01auth=Bearer ACCESS_TOKEN\x01\x01"
	if string(payload) != want {
		t.Fatalf("XOAUTH2 payload = %q, want %q", string(payload), want)
	}
}

func TestNormalizeIMAPOAuthState(t *testing.T) {
	state, err := normalizeICloudIMAPState(LoginState{
		IMAPEmail:           "Owner@Outlook.com",
		IMAPHost:            "outlook.office365.com",
		IMAPAuthMethod:      "XOAUTH2",
		OAuthClientID:       "CLIENT_ID",
		OAuthRefreshToken:   "REFRESH_TOKEN",
		OAuthTokenExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("normalizeICloudIMAPState() error = %v", err)
	}
	if state.IMAPEmail != "owner@outlook.com" || state.IMAPUsername != "owner@outlook.com" {
		t.Fatalf("normalized identity = %q/%q", state.IMAPEmail, state.IMAPUsername)
	}
	if state.IMAPAuthMethod != "xoauth2" || state.IMAPPort != 993 {
		t.Fatalf("normalized auth/port = %q/%d", state.IMAPAuthMethod, state.IMAPPort)
	}
}

func TestNormalizeIMAPRejectsLegacyOutlookPassword(t *testing.T) {
	_, err := normalizeICloudIMAPState(LoginState{
		IMAPEmail:       "owner@outlook.com",
		IMAPHost:        "outlook.office365.com",
		IMAPAppPassword: "legacy-password",
	})
	if err == nil {
		t.Fatal("normalizeICloudIMAPState() accepted legacy Outlook password authentication")
	}
}

func TestIMAPAuthenticateXOAUTH2CompletesErrorChallenge(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	completed := make(chan bool, 1)
	go func() {
		reader := bufio.NewReader(server)
		_, _ = reader.ReadString('\n')
		_, _ = server.Write([]byte("+ eyJzdGF0dXMiOiI0MDEifQ==\r\n"))
		line, _ := reader.ReadString('\n')
		completed <- strings.TrimSpace(line) == ""
		_, _ = server.Write([]byte("A001 NO AUTHENTICATE failed.\r\n"))
	}()

	state := LoginState{IMAPUsername: "owner@outlook.com", IMAPAuthMethod: "xoauth2", OAuthAccessToken: "EXPIRED"}
	if err := imapAuthenticate(client, bufio.NewReader(client), "A001", state); err == nil {
		t.Fatal("imapAuthenticate() succeeded after an OAuth error challenge")
	}
	if !<-completed {
		t.Fatal("imapAuthenticate() did not complete the server OAuth error challenge")
	}
}

func TestLimitIMAPUIDsKeepsOldestIncrementalBatch(t *testing.T) {
	uids := []int{101, 102, 103, 104, 105}
	incremental := limitIMAPUIDs(uids, 2, true)
	if len(incremental) != 2 || incremental[0] != 101 || incremental[1] != 102 {
		t.Fatalf("增量批次跳过了较早 UID：%v", incremental)
	}
	initial := limitIMAPUIDs(uids, 2, false)
	if len(initial) != 2 || initial[0] != 104 || initial[1] != 105 {
		t.Fatalf("首次同步没有保留最新 UID：%v", initial)
	}
}

func TestQQJunkWindowKeepsNewestBurstWithoutTrustingCursor(t *testing.T) {
	uids := make([]int, 40)
	for i := range uids {
		uids[i] = 101 + i
	}
	latest := limitIMAPUIDs(uids, qqJunkFetchMin, false)
	if len(latest) != qqJunkFetchMin || latest[0] != 109 || latest[len(latest)-1] != 140 {
		t.Fatalf("QQ Junk 没有保留最新邮件：%v", latest)
	}
	state := LoginState{IMAPLastSyncUID: "999", IMAPJunkLastSyncUID: "132"}
	if got := imapJunkSearchCommand(state, time.Time{}); !strings.HasPrefix(got, "UID SEARCH SINCE ") {
		t.Fatalf("QQ Junk 仍依赖不可靠游标：%q", got)
	}
}

func TestIMAPJunkHeaderCandidateMatchesAliasOrOpenAI(t *testing.T) {
	mailboxes := []Mailbox{{ID: "target", Email: "alias.one@icloud.com"}}
	aliasHeader := []byte("To: alias.one@icloud.com\r\nSubject: unrelated\r\nFrom: sender@example.com\r\n\r\n")
	if !imapJunkHeaderCandidate(aliasHeader, mailboxes, "OpenAI") {
		t.Fatal("target alias header was filtered out")
	}
	openAIHeader := []byte("To: hidden@example.com\r\nSubject: Your ChatGPT verification code\r\nFrom: noreply@openai.com\r\n\r\n")
	if !imapJunkHeaderCandidate(openAIHeader, mailboxes, "OpenAI") {
		t.Fatal("OpenAI candidate without visible alias was filtered out")
	}
	unrelated := []byte("To: hidden@example.com\r\nSubject: Store receipt\r\nFrom: shop@example.com\r\n\r\n")
	if imapJunkHeaderCandidate(unrelated, mailboxes, "OpenAI") {
		t.Fatal("unrelated junk message should not require a full body fetch")
	}
	malformed := []byte("not a valid RFC 822 header")
	if !imapJunkHeaderCandidate(malformed, mailboxes, "OpenAI") {
		t.Fatal("malformed header must fail open")
	}
}

func TestIMAPSyncFolderSelectionPrioritizesIdleFolder(t *testing.T) {
	tests := []struct {
		folder    string
		wantInbox bool
		wantJunk  bool
	}{
		{folder: IMAPSyncFolderAll, wantInbox: true, wantJunk: true},
		{folder: IMAPSyncFolderInbox, wantInbox: true, wantJunk: false},
		{folder: IMAPSyncFolderJunk, wantInbox: false, wantJunk: true},
		{folder: " JUNK ", wantInbox: false, wantJunk: true},
	}
	for _, test := range tests {
		inbox, junk := imapSyncFolderSelection(test.folder)
		if inbox != test.wantInbox || junk != test.wantJunk {
			t.Fatalf("folder=%q: inbox=%v junk=%v, want inbox=%v junk=%v", test.folder, inbox, junk, test.wantInbox, test.wantJunk)
		}
	}
}

func TestQQJunkInitialSearchUsesBoundedRecentWindow(t *testing.T) {
	after := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	state := LoginState{IMAPLastSyncUID: "500"}
	if got := imapJunkSearchCommand(state, after); got != "UID SEARCH SINCE 22-Aug-2026" {
		t.Fatalf("QQ Junk 初次扫描范围错误：%q", got)
	}
}

func TestIMAPExistsCountUsesLatestMailboxObservation(t *testing.T) {
	lines := []string{"* FLAGS (\\Seen)", "* 41 EXISTS", "* 42 EXISTS", "A002 OK SELECT completed"}
	if got := imapExistsCount(lines); got != 42 {
		t.Fatalf("EXISTS=%d, want 42", got)
	}
	if got := imapExistsCount([]string{"A003 OK NOOP completed"}); got != -1 {
		t.Fatalf("missing EXISTS=%d, want -1", got)
	}
}

func TestIMAPMaxUIDDetectsNewMailWhenExistsCountIsUnchanged(t *testing.T) {
	before := []string{"* SEARCH 640 645 647", "A004 OK SEARCH completed"}
	after := []string{"* SEARCH 640 645 648", "A005 OK SEARCH completed"}
	if got := imapMaxUID(before); got != 647 {
		t.Fatalf("before max UID=%d, want 647", got)
	}
	if got := imapMaxUID(after); got != 648 {
		t.Fatalf("after max UID=%d, want 648", got)
	}
	if got := imapMaxUID([]string{"* SEARCH", "A006 OK SEARCH completed"}); got != 0 {
		t.Fatalf("empty max UID=%d, want 0", got)
	}
}

func TestIMAPJunkMailboxPrefersSpecialUse(t *testing.T) {
	lines := []string{
		`* LIST (\\HasNoChildren) "/" "Spam"`,
		`* LIST (\\HasNoChildren \\Junk) "/" "QQ Mail/Junk"`,
		`A002 OK LIST completed`,
	}
	if got := imapJunkMailbox(lines); got != "QQ Mail/Junk" {
		t.Fatalf("imapJunkMailbox() = %q, want %q", got, "QQ Mail/Junk")
	}
}

func TestIMAPJunkMailboxFallsBackToJunkName(t *testing.T) {
	lines := []string{
		`* LIST (\\HasNoChildren) "/" "INBOX"`,
		`* LIST (\\HasNoChildren) "/" "Junk"`,
	}
	if got := imapJunkMailbox(lines); got != "Junk" {
		t.Fatalf("imapJunkMailbox() = %q, want Junk", got)
	}
}

func TestIMAPJunkMailboxSkipsNoSelect(t *testing.T) {
	lines := []string{`* LIST (\\Noselect \\Junk) "/" "Junk"`}
	if got := imapJunkMailbox(lines); got != "" {
		t.Fatalf("imapJunkMailbox() = %q, want empty", got)
	}
}

func TestQQIMAPHostScope(t *testing.T) {
	if !isQQIMAPHost(" IMAP.QQ.COM. ") {
		t.Fatal("QQ IMAP host was not recognized")
	}
	if isQQIMAPHost("imap.mail.me.com") || isQQIMAPHost("outlook.office365.com") {
		t.Fatal("non-QQ IMAP host was recognized as QQ")
	}
}

func TestIMAPIdleRenewsBeforeQQServerTimeout(t *testing.T) {
	if imapIdleReadTimeout != 15*time.Minute {
		t.Fatalf("IMAP IDLE renewal = %s, want 15m", imapIdleReadTimeout)
	}
}

func TestQQJunkEventFetchWindowIsBounded(t *testing.T) {
	if qqJunkEventFetchMax >= qqJunkFetchMin {
		t.Fatalf("event Junk window=%d should be smaller than fallback window=%d", qqJunkEventFetchMax, qqJunkFetchMin)
	}
	if qqJunkIdleProbe != time.Second {
		t.Fatalf("QQ Junk probe=%s, want 1s", qqJunkIdleProbe)
	}
}

func TestJunkMessageUsesFolderScopedRemoteID(t *testing.T) {
	raw := []byte("Date: Sun, 17 Aug 2026 12:00:00 +0800\r\n" +
		"To: alias@icloud.com\r\n" +
		"From: OpenAI <noreply@openai.com>\r\n" +
		"Subject: Your verification code is 123456\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Your verification code is 123456.\r\n")
	message, _, ok := parseICloudIMAPMessage(iCloudIMAPFetchedMessage{UID: "42", Folder: "junk", Raw: raw})
	if !ok {
		t.Fatal("parseICloudIMAPMessage() did not parse the message")
	}
	if message.RemoteID != "imap:junk:42" {
		t.Fatalf("RemoteID = %q, want imap:junk:42", message.RemoteID)
	}
	if message.UID != "42" {
		t.Fatalf("UID = %q, want 42", message.UID)
	}
}

func TestQQJunkSearchIgnoresUnreliableUIDCursor(t *testing.T) {
	after := time.Date(2026, 8, 24, 19, 0, 0, 0, time.Local)
	state := LoginState{IMAPJunkLastSyncUID: "999999"}
	got := imapJunkSearchCommand(state, after)
	want := "UID SEARCH SINCE " + after.Format("2-Jan-2006")
	if got != want {
		t.Fatalf("imapJunkSearchCommand() = %q, want %q", got, want)
	}
}

func TestJunkWindowKeepsNewestMessages(t *testing.T) {
	values := []int{1, 2, 3, 4, 5}
	got := limitIMAPUIDs(values, 2, false)
	if len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("latest Junk UIDs = %#v, want [4 5]", got)
	}
}
