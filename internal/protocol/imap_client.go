package protocol

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultICloudIMAPHost = "imap.mail.me.com"
	defaultICloudIMAPPort = 993
	qqJunkFetchMin        = 32
	qqJunkEventFetchMax   = 12
	qqJunkIdleProbe       = 1 * time.Second
	// QQ closes otherwise healthy IDLE sessions at roughly 20 minutes. Renew
	// early on the same authenticated connection to avoid a reconnect gap.
	imapIdleReadTimeout = 15 * time.Minute
)

var iCloudIMAPDNSServers = []string{"1.1.1.1:53", "8.8.8.8:53", "127.0.0.53:53"}
var errIMAPIdleRenew = errors.New("renew IMAP IDLE")
var iCloudIMAPFallbackIPv4 = []net.IP{
	net.IPv4(17, 57, 152, 32),
	net.IPv4(17, 57, 152, 35),
	net.IPv4(17, 57, 155, 39),
	net.IPv4(17, 42, 251, 69),
	net.IPv4(17, 42, 251, 72),
	net.IPv4(17, 56, 9, 32),
	net.IPv4(17, 156, 192, 7),
}

type iCloudIMAPSyncResult struct {
	MessagesByMailbox map[string][]ICloudSyncedMessage
	LastUID           string
	LastJunkUID       string
}

func newICloudIMAPDialer(serverName string) tls.Dialer {
	serverName = firstNonEmpty(strings.TrimSpace(serverName), defaultICloudIMAPHost)
	return tls.Dialer{
		NetDialer: &net.Dialer{
			Timeout:  15 * time.Second,
			Resolver: newICloudIMAPResolver(iCloudIMAPDNSServers),
		},
		Config: &tls.Config{
			ServerName: serverName,
			MinVersion: tls.VersionTLS12,
		},
	}
}

func dialICloudIMAPTLS(ctx context.Context, serverName string, port int) (net.Conn, error) {
	serverName = firstNonEmpty(strings.TrimSpace(serverName), defaultICloudIMAPHost)
	if port <= 0 {
		port = defaultICloudIMAPPort
	}
	var lastErr error
	if strings.EqualFold(strings.TrimSuffix(serverName, "."), defaultICloudIMAPHost) {
		if conn, err := dialICloudIMAPTLSIPs(ctx, serverName, port, iCloudIMAPFallbackIPv4); err == nil {
			return conn, nil
		} else {
			lastErr = err
		}
	}
	ips, lookupErr := lookupICloudIMAPIPv4(ctx, serverName)
	if len(appendUniqueIPv4(nil, ips...)) > 0 {
		if conn, err := dialICloudIMAPTLSIPs(ctx, serverName, port, ips); err == nil {
			return conn, nil
		} else if err != nil {
			lastErr = err
		}
	}
	address := net.JoinHostPort(serverName, strconv.Itoa(port))
	dialer := newICloudIMAPDialer(serverName)
	conn, err := dialer.DialContext(ctx, "tcp4", address)
	if err == nil {
		return conn, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("IMAP IPv4 直连失败：%v；域名拨号失败：%w", lastErr, err)
	}
	if lookupErr != nil {
		return nil, fmt.Errorf("IMAP DNS-over-TCP 解析失败：%v；域名拨号失败：%w", lookupErr, err)
	}
	return nil, err
}

func dialICloudIMAPTLSIPs(ctx context.Context, serverName string, port int, ips []net.IP) (net.Conn, error) {
	ips = appendUniqueIPv4(nil, ips...)
	if len(ips) == 0 {
		return nil, errors.New("没有可用的 IPv4 地址")
	}
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	results := make(chan result, len(ips))
	var wg sync.WaitGroup
	for _, ip := range ips {
		ip := ip
		wg.Add(1)
		go func() {
			defer wg.Done()
			address := net.JoinHostPort(ip.String(), strconv.Itoa(port))
			dialer := tls.Dialer{
				NetDialer: &net.Dialer{Timeout: 5 * time.Second},
				Config: &tls.Config{
					ServerName: serverName,
					MinVersion: tls.VersionTLS12,
				},
			}
			conn, err := dialer.DialContext(dialCtx, "tcp4", address)
			select {
			case results <- result{conn: conn, err: err}:
			case <-dialCtx.Done():
				if conn != nil {
					_ = conn.Close()
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	var errs []string
	for item := range results {
		if item.err == nil && item.conn != nil {
			cancel()
			return item.conn, nil
		}
		if item.conn != nil {
			_ = item.conn.Close()
		}
		if item.err != nil && !errors.Is(item.err, context.Canceled) {
			errs = append(errs, item.err.Error())
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if len(errs) == 0 {
		return nil, errors.New("所有 IMAP IPv4 连接均失败")
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

func appendUniqueIPv4(base []net.IP, extra ...net.IP) []net.IP {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]net.IP, 0, len(base)+len(extra))
	for _, ip := range base {
		if ip == nil || ip.To4() == nil {
			continue
		}
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ip)
	}
	for _, ip := range extra {
		if ip == nil || ip.To4() == nil {
			continue
		}
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ip)
	}
	return out
}

func newICloudIMAPResolver(servers []string) *net.Resolver {
	cleaned := make([]string, 0, len(servers))
	for _, server := range servers {
		server = strings.TrimSpace(server)
		if server != "" {
			cleaned = append(cleaned, server)
		}
	}
	if len(cleaned) == 0 {
		cleaned = []string{"1.1.1.1:53"}
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var lastErr error
			for _, dnsNetwork := range dnsFallbackNetworks(network) {
				for _, server := range cleaned {
					dialer := net.Dialer{Timeout: 2 * time.Second}
					conn, err := dialer.DialContext(ctx, dnsNetwork, server)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, lastErr
		},
	}
}

func lookupICloudIMAPIPv4(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, errors.New("主机地址为空")
	}
	var messages []string
	seen := make(map[string]struct{})
	var out []net.IP
	for _, server := range iCloudIMAPDNSServers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		ips, err := lookupDNSAOverTCP(ctx, server, host)
		if err != nil {
			messages = append(messages, server+": "+err.Error())
			continue
		}
		for _, ip := range ips {
			if ip == nil || ip.To4() == nil {
				continue
			}
			key := ip.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ip)
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	if len(messages) == 0 {
		return nil, errors.New("没有配置 DNS 服务器")
	}
	return nil, errors.New(strings.Join(messages, "; "))
}

func lookupDNSAOverTCP(ctx context.Context, server, host string) ([]net.IP, error) {
	queryID := uint16(time.Now().UnixNano())
	query, err := dnsBuildAQuery(host, queryID)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp4", server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	prefix := make([]byte, 2)
	binary.BigEndian.PutUint16(prefix, uint16(len(query)))
	if _, err := conn.Write(prefix); err != nil {
		return nil, err
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(conn, prefix); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(prefix))
	if size <= 0 || size > 65535 {
		return nil, fmt.Errorf("DNS 响应长度无效：%d", size)
	}
	response := make([]byte, size)
	if _, err := io.ReadFull(conn, response); err != nil {
		return nil, err
	}
	return dnsParseAResponse(response, queryID)
}

func dnsBuildAQuery(host string, id uint16) ([]byte, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, errors.New("DNS 主机为空")
	}
	buf := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(buf[0:2], id)
	binary.BigEndian.PutUint16(buf[2:4], 0x0100)
	binary.BigEndian.PutUint16(buf[4:6], 1)
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return nil, fmt.Errorf("DNS 标签为空：%q", host)
		}
		if len(label) > 63 {
			return nil, fmt.Errorf("DNS 标签过长：%q", host)
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 1)
	buf = binary.BigEndian.AppendUint16(buf, 1)
	return buf, nil
}

func dnsParseAResponse(message []byte, id uint16) ([]net.IP, error) {
	if len(message) < 12 {
		return nil, errors.New("DNS 响应过短")
	}
	if binary.BigEndian.Uint16(message[0:2]) != id {
		return nil, errors.New("DNS 响应 ID 不匹配")
	}
	flags := binary.BigEndian.Uint16(message[2:4])
	if flags&0x000f != 0 {
		return nil, fmt.Errorf("DNS 响应码：%d", flags&0x000f)
	}
	qdCount := int(binary.BigEndian.Uint16(message[4:6]))
	anCount := int(binary.BigEndian.Uint16(message[6:8]))
	offset := 12
	var err error
	for i := 0; i < qdCount; i++ {
		offset, err = dnsSkipName(message, offset)
		if err != nil {
			return nil, err
		}
		if offset+4 > len(message) {
			return nil, errors.New("DNS 问题段过短")
		}
		offset += 4
	}
	var out []net.IP
	for i := 0; i < anCount; i++ {
		offset, err = dnsSkipName(message, offset)
		if err != nil {
			return nil, err
		}
		if offset+10 > len(message) {
			return nil, errors.New("DNS 答案段过短")
		}
		recordType := binary.BigEndian.Uint16(message[offset : offset+2])
		recordClass := binary.BigEndian.Uint16(message[offset+2 : offset+4])
		rdLength := int(binary.BigEndian.Uint16(message[offset+8 : offset+10]))
		offset += 10
		if offset+rdLength > len(message) {
			return nil, errors.New("DNS RDATA 过短")
		}
		if recordType == 1 && recordClass == 1 && rdLength == 4 {
			out = append(out, net.IPv4(message[offset], message[offset+1], message[offset+2], message[offset+3]))
		}
		offset += rdLength
	}
	if len(out) == 0 {
		return nil, errors.New("DNS 响应没有 A 记录")
	}
	return out, nil
}

func dnsSkipName(message []byte, offset int) (int, error) {
	for jumps := 0; ; jumps++ {
		if jumps > 128 {
			return 0, errors.New("DNS 名称压缩出现循环")
		}
		if offset >= len(message) {
			return 0, errors.New("DNS 名称过短")
		}
		length := int(message[offset])
		if length&0xc0 == 0xc0 {
			if offset+1 >= len(message) {
				return 0, errors.New("DNS 压缩指针过短")
			}
			return offset + 2, nil
		}
		if length&0xc0 != 0 {
			return 0, errors.New("DNS 标签无效")
		}
		offset++
		if length == 0 {
			return offset, nil
		}
		if offset+length > len(message) {
			return 0, errors.New("DNS 标签过短")
		}
		offset += length
	}
}

func dnsFallbackNetworks(network string) []string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "udp", "udp4", "udp6":
		return []string{"tcp", "udp"}
	case "tcp", "tcp4", "tcp6":
		return []string{"tcp", "udp"}
	case "":
		return []string{"tcp", "udp"}
	default:
		return []string{network, "udp", "tcp"}
	}
}

func CheckICloudIMAPLogin(ctx context.Context, email, appPassword string) error {
	return CheckIMAPLogin(ctx, LoginState{
		IMAPEmail:       email,
		IMAPUsername:    email,
		IMAPHost:        defaultICloudIMAPHost,
		IMAPPort:        defaultICloudIMAPPort,
		IMAPAppPassword: appPassword,
	})
}

func CheckIMAPLogin(ctx context.Context, state LoginState) error {
	state, err := PrepareIMAPLoginState(ctx, state)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return errCode("imap_connect_failed", "连接 IMAP 服务器失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(25 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return errCode("imap_greeting_failed", "读取 IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return errCode("imap_greeting_failed", "IMAP 服务器未就绪："+imapResponseSummary([]string{greeting}), true)
	}

	if err := imapAuthenticate(conn, reader, "A001", state); err != nil {
		return err
	}

	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return errCode("imap_select_failed", "打开 IMAP 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return errCode("imap_select_failed", "打开 IMAP 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
	return nil
}

func WatchICloudIMAPExists(ctx context.Context, state LoginState, onReady, onExists func()) error {
	return watchICloudIMAPMailboxExists(ctx, state, "INBOX", nil, "", 0, onReady, onExists, nil)
}

// WatchICloudIMAPJunkExists keeps a second QQ IMAP connection selected on the
// Junk folder. INBOX IDLE does not receive EXISTS events for messages that QQ
// classifies directly as junk.
func WatchICloudIMAPJunkExists(ctx context.Context, state LoginState, onReady, onExists func()) error {
	return watchICloudIMAPMailboxExists(ctx, state, "junk", nil, "", 0, onReady, onExists, nil)
}

// WatchICloudIMAPJunkMessages uses the already-authenticated Junk watcher
// connection to fetch only UIDs newly visible in QQ Junk. The callback runs
// after parsing and recipient matching, without entering the account scan
// coordinator or opening another IMAP connection.
func WatchICloudIMAPJunkMessages(ctx context.Context, state LoginState, mailboxes []Mailbox, keyword string, maxMessages int, onReady func(), onMessages func(map[string][]ICloudSyncedMessage) error) error {
	return watchICloudIMAPMailboxExists(ctx, state, "junk", mailboxes, keyword, maxMessages, onReady, nil, onMessages)
}

func watchICloudIMAPMailboxExists(ctx context.Context, state LoginState, watchFolder string, mailboxes []Mailbox, keyword string, maxMessages int, onReady, onExists func(), onMessages func(map[string][]ICloudSyncedMessage) error) error {
	state, err := PrepareIMAPLoginState(ctx, state)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return errCode("imap_connect_failed", "连接 IMAP 服务器失败："+err.Error(), true)
	}
	defer conn.Close()
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return errCode("imap_greeting_failed", "读取 IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return errCode("imap_greeting_failed", "IMAP 服务器未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	if err := imapAuthenticate(conn, reader, "A001", state); err != nil {
		return err
	}
	tagIndex := 2
	nextTag := func() string {
		tag := fmt.Sprintf("A%03d", tagIndex)
		tagIndex++
		return tag
	}
	mailbox := "INBOX"
	if strings.EqualFold(strings.TrimSpace(watchFolder), "junk") {
		listTag := nextTag()
		listLines, listErr := imapCommand(conn, reader, listTag, `LIST "" "*"`)
		if listErr != nil {
			return errCode("imap_junk_list_failed", "列出 QQ IMAP 邮件目录失败："+listErr.Error(), true)
		}
		if !imapTaggedOK(listLines, listTag) {
			return errCode("imap_junk_list_failed", "列出 QQ IMAP 邮件目录失败："+imapResponseSummary(listLines), true)
		}
		mailbox = imapJunkMailbox(listLines)
		if mailbox == "" {
			return errCode("imap_junk_folder_missing", "QQ IMAP 未返回可识别的垃圾箱目录", true)
		}
	}
	selectTag := nextTag()
	selectLines, err := imapCommand(conn, reader, selectTag, "SELECT "+imapQuote(mailbox))
	if err != nil {
		return errCode("imap_select_failed", "打开 IMAP 邮件目录失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, selectTag) {
		return errCode("imap_select_failed", "打开 IMAP 邮件目录失败："+imapResponseSummary(selectLines), true)
	}
	lastExists := imapExistsCount(selectLines)
	lastUID := imapSelectLastUID(selectLines)
	watchingJunk := strings.EqualFold(strings.TrimSpace(watchFolder), "junk")
	lastJunkFingerprint := ""
	var lastJunkUIDs []int
	if watchingJunk {
		searchTag := nextTag()
		lines, searchErr := imapCommand(conn, reader, searchTag, "UID SEARCH ALL")
		if searchErr != nil {
			return searchErr
		}
		lastJunkUIDs = imapRecentUIDs(lines, 32)
		lastJunkFingerprint = imapUIDValuesFingerprint(lastJunkUIDs)
		if currentUID := imapMaxUID(lines); currentUID > lastUID {
			lastUID = currentUID
		}
	}
	probeJunk := func() error {
		searchTag := nextTag()
		lines, searchErr := imapCommand(conn, reader, searchTag, "UID SEARCH ALL")
		if searchErr != nil {
			return searchErr
		}
		currentUID := imapMaxUID(lines)
		currentUIDs := imapRecentUIDs(lines, 32)
		fingerprint := imapUIDValuesFingerprint(currentUIDs)
		changed := currentUID > lastUID || (lastJunkFingerprint != "" && fingerprint != lastJunkFingerprint)
		addedUIDs := imapUIDDifference(currentUIDs, lastJunkUIDs)
		lastJunkUIDs = currentUIDs
		lastJunkFingerprint = fingerprint
		if currentUID > lastUID {
			lastUID = currentUID
		}
		if !changed {
			return nil
		}
		if onMessages != nil && len(addedUIDs) > 0 {
			fetchLimit := maxMessages
			if fetchLimit <= 0 || fetchLimit > qqJunkEventFetchMax {
				fetchLimit = qqJunkEventFetchMax
			}
			addedUIDs = lastIntValues(addedUIDs, fetchLimit)
			fetched, fetchErr := fetchSelectedIMAPUIDs(conn, reader, nextTag, addedUIDs, "junk")
			if fetchErr != nil {
				return fetchErr
			}
			matchedMailboxes := append([]Mailbox(nil), mailboxes...)
			for i := range matchedMailboxes {
				matchedMailboxes[i].LastSyncAt = time.Time{}
			}
			after := time.Now().Add(-24 * time.Hour)
			messages := iCloudIMAPMessagesByMailbox(fetched, matchedMailboxes, after, keyword, state.IMAPEmail, state.IMAPUsername)
			if len(messages) > 0 {
				return onMessages(messages)
			}
			return nil
		}
		if onExists != nil {
			onExists()
		}
		return nil
	}
	if onReady != nil {
		onReady()
	}

	idleTimeout := imapIdleReadTimeout
	if watchingJunk {
		idleTimeout = qqJunkIdleProbe
	}
	for ctx.Err() == nil {
		tag := nextTag()
		if err := imapWaitForExistsWithTimeout(ctx, conn, reader, tag, idleTimeout); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, errIMAPIdleRenew) {
				if watchingJunk {
					if probeErr := probeJunk(); probeErr != nil {
						return probeErr
					}
				}
				continue
			}
			return err
		}
		_ = conn.SetReadDeadline(time.Time{})
		if watchingJunk {
			if probeErr := probeJunk(); probeErr != nil {
				return probeErr
			}
		} else if onExists != nil {
			onExists()
		}
		lastExists++
	}
	return ctx.Err()
}

func imapWaitForExists(ctx context.Context, conn net.Conn, reader *bufio.Reader, tag string) error {
	return imapWaitForExistsWithTimeout(ctx, conn, reader, tag, imapIdleReadTimeout)
}

func imapWaitForExistsWithTimeout(ctx context.Context, conn net.Conn, reader *bufio.Reader, tag string, readTimeout time.Duration) error {
	if readTimeout <= 0 {
		readTimeout = imapIdleReadTimeout
	}
	if _, err := fmt.Fprintf(conn, "%s IDLE\r\n", tag); err != nil {
		return err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "+") {
		return fmt.Errorf("IMAP IDLE 未进入等待状态：%s", strings.TrimSpace(line))
	}
	for ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		line, err = reader.ReadString('\n')
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() && ctx.Err() == nil {
				_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
				if _, writeErr := fmt.Fprint(conn, "DONE\r\n"); writeErr != nil {
					return writeErr
				}
				if _, doneErr := imapReadUntilTag(reader, tag); doneErr != nil {
					return doneErr
				}
				_ = conn.SetReadDeadline(time.Time{})
				return errIMAPIdleRenew
			}
			_, _ = fmt.Fprint(conn, "DONE\r\n")
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if imapLineHasExistsEvent(line) {
			_, _ = fmt.Fprint(conn, "DONE\r\n")
			_, _ = imapReadUntilTag(reader, tag)
			return nil
		}
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			return nil
		}
	}
	_, _ = fmt.Fprint(conn, "DONE\r\n")
	_, _ = imapReadUntilTag(reader, tag)
	return ctx.Err()
}

func imapExistsCount(lines []string) int {
	latest := -1
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || fields[0] != "*" || !strings.EqualFold(fields[2], "EXISTS") {
			continue
		}
		count, err := strconv.Atoi(fields[1])
		if err == nil {
			latest = count
		}
	}
	return latest
}

func imapMaxUID(lines []string) int {
	maxUID := 0
	for _, uid := range imapSearchUIDs(lines) {
		if uid > maxUID {
			maxUID = uid
		}
	}
	return maxUID
}

func imapUIDFingerprint(lines []string, limit int) string {
	return imapUIDValuesFingerprint(imapRecentUIDs(lines, limit))
}

func imapRecentUIDs(lines []string, limit int) []int {
	uids := imapSearchUIDs(lines)
	if len(uids) == 0 {
		return nil
	}
	sortInts(uids)
	return lastIntValues(uids, limit)
}

func imapUIDValuesFingerprint(uids []int) string {
	parts := make([]string, 0, len(uids))
	for _, uid := range uids {
		parts = append(parts, strconv.Itoa(uid))
	}
	return strings.Join(parts, ",")
}

func imapUIDDifference(current, previous []int) []int {
	seen := make(map[int]struct{}, len(previous))
	for _, uid := range previous {
		seen[uid] = struct{}{}
	}
	added := make([]int, 0, len(current))
	for _, uid := range current {
		if _, ok := seen[uid]; !ok {
			added = append(added, uid)
		}
	}
	return added
}

func imapReadUntilTag(reader *bufio.Reader, tag string) ([]string, error) {
	lines := make([]string, 0, 4)
	for i := 0; i < 200; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return lines, err
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, strings.TrimSpace(line))
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			return lines, nil
		}
	}
	return lines, errors.New("IMAP IDLE 响应过长")
}

func imapCommand(conn net.Conn, reader *bufio.Reader, tag, command string) ([]string, error) {
	lines, _, err := imapCommandWithLiterals(conn, reader, tag, command)
	return lines, err
}

func imapCommandWithLiterals(conn net.Conn, reader *bufio.Reader, tag, command string) ([]string, [][]byte, error) {
	if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, command); err != nil {
		return nil, nil, err
	}
	lines := make([]string, 0, 4)
	literals := make([][]byte, 0)
	for i := 0; i < 200; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return lines, literals, err
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, strings.TrimSpace(line))
		if n, ok := imapLiteralSize(line); ok {
			body := make([]byte, n)
			if _, err := io.ReadFull(reader, body); err != nil {
				return lines, literals, err
			}
			literals = append(literals, body)
		}
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			return lines, literals, nil
		}
	}
	return lines, literals, errors.New("IMAP 响应过长")
}

func imapAuthenticate(conn net.Conn, reader *bufio.Reader, tag string, state LoginState) error {
	command := "LOGIN " + imapQuote(state.IMAPUsername) + " " + imapQuote(state.IMAPAppPassword)
	errorHint := "请确认邮箱账号和授权密码"
	if strings.EqualFold(state.IMAPAuthMethod, "xoauth2") {
		payload := "user=" + state.IMAPUsername + "\x01auth=Bearer " + state.OAuthAccessToken + "\x01\x01"
		command = "AUTHENTICATE XOAUTH2 " + base64.StdEncoding.EncodeToString([]byte(payload))
		errorHint = "请重新连接 Microsoft 授权"
		if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, command); err != nil {
			return errCode("imap_login_failed", "IMAP 登录请求失败："+err.Error(), true)
		}
		lines := make([]string, 0, 4)
		for len(lines) < 20 {
			line, err := reader.ReadString('\n')
			if err != nil {
				return errCode("imap_login_failed", "IMAP 登录请求失败："+err.Error(), true)
			}
			line = strings.TrimSpace(line)
			lines = append(lines, line)
			if strings.HasPrefix(strings.ToUpper(line), strings.ToUpper(tag)+" ") {
				if imapTaggedOK(lines, tag) {
					return nil
				}
				return errCode("imap_login_failed", "IMAP 登录失败，"+errorHint+"："+imapResponseSummary(lines), false)
			}
			if strings.HasPrefix(line, "+") {
				if _, err := io.WriteString(conn, "\r\n"); err != nil {
					return errCode("imap_login_failed", "IMAP 登录请求失败："+err.Error(), true)
				}
			}
		}
		return errCode("imap_login_failed", "IMAP 登录响应过长", true)
	}
	lines, err := imapCommand(conn, reader, tag, command)
	if err != nil {
		return errCode("imap_login_failed", "IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(lines, tag) {
		return errCode("imap_login_failed", "IMAP 登录失败，"+errorHint+"："+imapResponseSummary(lines), false)
	}
	return nil
}

func imapTaggedOK(lines []string, tag string) bool {
	for _, line := range lines {
		upper := strings.ToUpper(strings.TrimSpace(line))
		if strings.HasPrefix(upper, strings.ToUpper(tag)+" ") {
			return strings.Contains(upper, " OK")
		}
	}
	return false
}

func imapQuote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	return `"` + value + `"`
}

func imapResponseSummary(lines []string) string {
	joined := strings.Join(lines, "；")
	return trimForError([]byte(joined))
}

func SyncICloudIMAPMessages(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int) (map[string][]ICloudSyncedMessage, error) {
	result, err := SyncICloudIMAPMessagesWithCursor(ctx, state, mailboxes, after, keyword, maxMessages)
	if err != nil {
		return nil, err
	}
	return result.MessagesByMailbox, nil
}

func LatestICloudIMAPUID(ctx context.Context, state LoginState) (string, error) {
	state, err := PrepareIMAPLoginState(ctx, state)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return "", errCode("imap_connect_failed", "连接 IMAP 服务器失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return "", errCode("imap_greeting_failed", "读取 IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return "", errCode("imap_greeting_failed", "IMAP 服务器未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	if err := imapAuthenticate(conn, reader, "A001", state); err != nil {
		return "", err
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return "", errCode("imap_select_failed", "打开 IMAP 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return "", errCode("imap_select_failed", "打开 IMAP 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
	uid := imapSelectLastUID(selectLines)
	if uid <= 0 {
		return "", nil
	}
	return strconv.Itoa(uid), nil
}

func SyncICloudIMAPMessagesWithCursor(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int) (iCloudIMAPSyncResult, error) {
	return syncICloudIMAPMessagesWithCursorFolder(ctx, state, mailboxes, after, keyword, maxMessages, IMAPSyncFolderAll)
}

func syncICloudIMAPMessagesWithCursorFolder(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int, folder string) (iCloudIMAPSyncResult, error) {
	state, err := PrepareIMAPLoginState(ctx, state)
	if err != nil {
		return iCloudIMAPSyncResult{}, err
	}
	if maxMessages <= 0 {
		maxMessages = 50
	}
	if maxMessages > 200 {
		maxMessages = 200
	}
	if keyword = strings.TrimSpace(keyword); keyword == "" {
		keyword = "OpenAI"
	}
	if len(mailboxes) == 0 {
		return iCloudIMAPSyncResult{MessagesByMailbox: map[string][]ICloudSyncedMessage{}}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_connect_failed", "连接 IMAP 服务器失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_greeting_failed", "读取 IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return iCloudIMAPSyncResult{}, errCode("imap_greeting_failed", "IMAP 服务器未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	if err := imapAuthenticate(conn, reader, "A001", state); err != nil {
		return iCloudIMAPSyncResult{}, err
	}
	tagIndex := 1
	nextTag := func() string {
		tagIndex++
		return fmt.Sprintf("A%03d", tagIndex)
	}
	syncInbox, syncJunk := imapSyncFolderSelection(folder)

	junkMailbox := ""
	var junkSyncErr error
	if syncJunk && isQQIMAPHost(state.IMAPHost) {
		listTag := nextTag()
		listLines, listErr := imapCommand(conn, reader, listTag, `LIST "" "*"`)
		switch {
		case listErr != nil:
			junkSyncErr = errCode("imap_junk_list_failed", "列出 QQ IMAP 邮件目录失败："+listErr.Error(), true)
		case !imapTaggedOK(listLines, listTag):
			junkSyncErr = errCode("imap_junk_list_failed", "列出 QQ IMAP 邮件目录失败："+imapResponseSummary(listLines), true)
		default:
			junkMailbox = imapJunkMailbox(listLines)
			if junkMailbox == "" {
				junkSyncErr = errCode("imap_junk_folder_missing", "QQ IMAP 未返回可识别的垃圾箱目录", true)
			}
		}
	}

	var fetched []iCloudIMAPFetchedMessage
	lastUID := ""
	if syncInbox {
		fetched, lastUID, err = syncSelectedIMAPMailbox(conn, reader, nextTag, "INBOX", "", imapSearchCommand(state, mailboxes, after), maxMessages, imapHasSyncCursor(state, mailboxes))
		if err != nil {
			return iCloudIMAPSyncResult{}, err
		}
	}

	var junkFetched []iCloudIMAPFetchedMessage
	lastJunkUID := ""
	if junkMailbox != "" {
		junkAfter := after
		oneDayAgo := time.Now().Add(-24 * time.Hour)
		if junkAfter.IsZero() || junkAfter.Before(oneDayAgo) {
			junkAfter = oneDayAgo
		}
		junkMaxMessages := maxMessages
		if junkMaxMessages < qqJunkFetchMin {
			junkMaxMessages = qqJunkFetchMin
		}
		// An IDLE/UID probe points at the newest Junk burst. Keep event-driven
		// scans small so ten registration callers do not repeatedly transfer a
		// large window; the scheduled all-folder fallback still uses the full
		// configured window for missed events.
		if folder == IMAPSyncFolderJunk && junkMaxMessages > qqJunkEventFetchMax {
			junkMaxMessages = qqJunkEventFetchMax
		}
		var err error
		// QQ may assign or reshuffle Junk UIDs when a message is classified or
		// moved after delivery. A persistent UID cursor can therefore skip a
		// newly visible message forever. Re-read the bounded recent window, take
		// the newest messages, and rely on folder-scoped RemoteID de-duplication.
		junkFetched, lastJunkUID, err = syncSelectedIMAPMailboxFiltered(conn, reader, nextTag, junkMailbox, "junk", imapJunkSearchCommand(state, junkAfter), junkMaxMessages, false, func(header []byte) bool {
			return imapJunkHeaderCandidate(header, mailboxes, keyword, state.IMAPEmail, state.IMAPUsername)
		})
		if err != nil {
			junkSyncErr = errCode("imap_junk_sync_failed", "同步 QQ IMAP 垃圾箱失败："+err.Error(), true)
			lastJunkUID = ""
		}
	}
	_, _ = imapCommand(conn, reader, nextTag(), "LOGOUT")

	messagesByMailbox := iCloudIMAPMessagesByMailbox(fetched, mailboxes, after, keyword, state.IMAPEmail, state.IMAPUsername)
	if len(junkFetched) > 0 {
		// Junk has no persistent UID cursor. Re-read the bounded window and let RemoteID de-duplicate it.
		junkMailboxes := append([]Mailbox(nil), mailboxes...)
		for i := range junkMailboxes {
			junkMailboxes[i].LastSyncAt = time.Time{}
		}
		messageAfter := after
		oneDayAgo := time.Now().Add(-24 * time.Hour)
		if messageAfter.IsZero() || messageAfter.Before(oneDayAgo) {
			messageAfter = oneDayAgo
		}
		junkMessages := iCloudIMAPMessagesByMailbox(junkFetched, junkMailboxes, messageAfter, keyword, state.IMAPEmail, state.IMAPUsername)
		for mailboxID, messages := range junkMessages {
			messagesByMailbox[mailboxID] = append(messagesByMailbox[mailboxID], messages...)
			sortMessagesByReceivedAt(messagesByMailbox[mailboxID])
		}
	}
	return iCloudIMAPSyncResult{
		MessagesByMailbox: messagesByMailbox,
		LastUID:           lastUID,
		LastJunkUID:       lastJunkUID,
	}, junkSyncErr
}

func imapSyncFolderSelection(folder string) (syncInbox, syncJunk bool) {
	switch strings.ToLower(strings.TrimSpace(folder)) {
	case IMAPSyncFolderInbox:
		return true, false
	case IMAPSyncFolderJunk:
		return false, true
	default:
		return true, true
	}
}

func syncSelectedIMAPMailbox(conn net.Conn, reader *bufio.Reader, nextTag func() string, mailbox, remoteFolder, searchCommand string, maxMessages int, incremental bool) ([]iCloudIMAPFetchedMessage, string, error) {
	return syncSelectedIMAPMailboxFiltered(conn, reader, nextTag, mailbox, remoteFolder, searchCommand, maxMessages, incremental, nil)
}

func syncSelectedIMAPMailboxFiltered(conn net.Conn, reader *bufio.Reader, nextTag func() string, mailbox, remoteFolder, searchCommand string, maxMessages int, incremental bool, headerCandidate func([]byte) bool) ([]iCloudIMAPFetchedMessage, string, error) {
	selectTag := nextTag()
	selectLines, err := imapCommand(conn, reader, selectTag, "SELECT "+imapQuote(mailbox))
	if err != nil {
		return nil, "", errCode("imap_select_failed", "打开 IMAP 邮件目录失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, selectTag) {
		return nil, "", errCode("imap_select_failed", "打开 IMAP 邮件目录失败："+imapResponseSummary(selectLines), true)
	}
	searchTag := nextTag()
	searchLines, err := imapCommand(conn, reader, searchTag, searchCommand)
	if err != nil {
		return nil, "", errCode("imap_search_failed", "搜索 IMAP 邮件失败："+err.Error(), true)
	}
	if !imapTaggedOK(searchLines, searchTag) {
		return nil, "", errCode("imap_search_failed", "搜索 IMAP 邮件失败："+imapResponseSummary(searchLines), true)
	}
	uids := imapSearchUIDs(searchLines)
	if len(uids) == 0 {
		return nil, "", nil
	}
	sortInts(uids)
	uids = limitIMAPUIDs(uids, maxMessages, incremental)
	lastUID := strconv.Itoa(uids[len(uids)-1])
	if headerCandidate != nil {
		candidateUIDs := make([]int, 0, len(uids))
		for _, chunk := range chunkInts(uids, 50) {
			fetchTag := nextTag()
			command := "UID FETCH " + imapUIDSet(chunk) + " (UID BODY.PEEK[HEADER.FIELDS (TO CC DELIVERED-TO X-ORIGINAL-TO ENVELOPE-TO SUBJECT FROM DATE MESSAGE-ID)])"
			lines, literals, fetchErr := imapCommandWithLiterals(conn, reader, fetchTag, command)
			if fetchErr != nil {
				return nil, "", errCode("imap_header_fetch_failed", "读取 IMAP 邮件头失败："+fetchErr.Error(), true)
			}
			if !imapTaggedOK(lines, fetchTag) {
				return nil, "", errCode("imap_header_fetch_failed", "读取 IMAP 邮件头失败："+imapResponseSummary(lines), true)
			}
			fetchUIDs := imapFetchUIDs(lines)
			for i, raw := range literals {
				if i < len(fetchUIDs) && headerCandidate(raw) {
					candidateUIDs = append(candidateUIDs, fetchUIDs[i])
				}
			}
		}
		uids = candidateUIDs
		if len(uids) == 0 {
			return nil, lastUID, nil
		}
	}
	fetched, fetchErr := fetchSelectedIMAPUIDs(conn, reader, nextTag, uids, remoteFolder)
	if fetchErr != nil {
		return nil, "", fetchErr
	}
	return fetched, lastUID, nil
}

func fetchSelectedIMAPUIDs(conn net.Conn, reader *bufio.Reader, nextTag func() string, uids []int, remoteFolder string) ([]iCloudIMAPFetchedMessage, error) {
	fetched := make([]iCloudIMAPFetchedMessage, 0, len(uids))
	for _, chunk := range chunkInts(uids, 20) {
		fetchTag := nextTag()
		lines, literals, fetchErr := imapCommandWithLiterals(conn, reader, fetchTag, "UID FETCH "+imapUIDSet(chunk)+" (UID INTERNALDATE BODY.PEEK[]<0.200000>)")
		if fetchErr != nil {
			return nil, errCode("imap_fetch_failed", "读取 IMAP 邮件失败："+fetchErr.Error(), true)
		}
		if !imapTaggedOK(lines, fetchTag) {
			return nil, errCode("imap_fetch_failed", "读取 IMAP 邮件失败："+imapResponseSummary(lines), true)
		}
		fetchUIDs := imapFetchUIDs(lines)
		internalDates := imapFetchInternalDates(lines)
		for i, raw := range literals {
			uid := ""
			var providerReceivedAt time.Time
			if i < len(fetchUIDs) {
				uidNumber := fetchUIDs[i]
				uid = strconv.Itoa(uidNumber)
				providerReceivedAt = internalDates[uidNumber]
			}
			fetched = append(fetched, iCloudIMAPFetchedMessage{UID: uid, Folder: remoteFolder, Raw: raw, ProviderReceivedAt: providerReceivedAt})
		}
	}
	return fetched, nil
}

func imapJunkHeaderCandidate(raw []byte, mailboxes []Mailbox, keyword string, ignoredEmails ...string) bool {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		// A malformed or unexpectedly encoded header must not make a real code
		// message disappear. Fetch its body and let the full parser decide.
		return true
	}
	headerText := imapHeaderText(msg.Header)
	aliases := make(map[string]string, len(mailboxes))
	ignored := make(map[string]struct{}, len(ignoredEmails))
	for _, email := range ignoredEmails {
		if email = normalizeICloudIMAPEmail(email); email != "" {
			ignored[email] = struct{}{}
		}
	}
	for _, mailbox := range mailboxes {
		email := normalizeICloudIMAPEmail(mailbox.Email)
		if email == "" {
			continue
		}
		if _, skip := ignored[email]; skip {
			continue
		}
		aliases[mailbox.ID] = email
	}
	if len(matchingMailboxIDs(headerText, aliases)) > 0 {
		return true
	}
	// Some forwarding paths omit the alias recipient but preserve the sender
	// and subject. Fetch all verification-looking candidates to avoid misses.
	return looksLikeVerificationText(decodeMIMEHeader(msg.Header.Get("Subject"))+"\n"+decodeMIMEHeader(msg.Header.Get("From")), keyword)
}

func isQQIMAPHost(host string) bool {
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(host), "."), "imap.qq.com")
}

func imapJunkMailbox(lines []string) string {
	fallback := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(trimmed), "* LIST ") {
			continue
		}
		mailbox, ok := imapListMailboxName(trimmed)
		if !ok || mailbox == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.Contains(upper, "\\NOSELECT") {
			continue
		}
		if strings.Contains(upper, "\\JUNK") {
			return mailbox
		}
		name := strings.ToLower(strings.TrimSpace(mailbox))
		if name == "junk" || name == "spam" || strings.HasSuffix(name, "/junk") || strings.HasSuffix(name, "/spam") {
			fallback = mailbox
		}
	}
	return fallback
}

func imapListMailboxName(line string) (string, bool) {
	closeAttrs := strings.Index(line, ")")
	if closeAttrs < 0 || closeAttrs+1 >= len(line) {
		return "", false
	}
	tokens := imapStringTokens(line[closeAttrs+1:])
	if len(tokens) < 2 {
		return "", false
	}
	return tokens[len(tokens)-1], true
}

func imapStringTokens(value string) []string {
	var tokens []string
	var current strings.Builder
	inQuote := false
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range value {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if inQuote && r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			inQuote = !inQuote
			if !inQuote {
				flush()
			}
			continue
		}
		if !inQuote && (r == ' ' || r == '\t') {
			flush()
			continue
		}
		current.WriteRune(r)
	}
	flush()
	return tokens
}

func imapSearchCommand(state LoginState, mailboxes []Mailbox, after time.Time) string {
	if uid := imapUIDNumber(state.IMAPLastSyncUID); uid > 0 {
		return "UID SEARCH UID " + strconv.Itoa(uid+1) + ":*"
	}
	nextUID := 0
	for _, mailbox := range mailboxes {
		uid := imapUIDNumber(mailbox.LastSyncUID)
		if uid <= 0 {
			nextUID = 0
			break
		}
		if nextUID == 0 || uid+1 < nextUID {
			nextUID = uid + 1
		}
	}
	if nextUID > 0 {
		return "UID SEARCH UID " + strconv.Itoa(nextUID) + ":*"
	}
	searchAfter := after
	if searchAfter.IsZero() {
		searchAfter = time.Now().Add(-24 * time.Hour)
	}
	return "UID SEARCH SINCE " + searchAfter.Format("2-Jan-2006")
}

func imapJunkSearchCommand(state LoginState, after time.Time) string {
	_ = state // Junk UIDs are not a reliable cross-sync cursor on QQ IMAP.
	if after.IsZero() {
		after = time.Now().Add(-24 * time.Hour)
	}
	return "UID SEARCH SINCE " + after.Format("2-Jan-2006")
}

func imapHasSyncCursor(state LoginState, mailboxes []Mailbox) bool {
	if imapUIDNumber(state.IMAPLastSyncUID) > 0 {
		return true
	}
	if len(mailboxes) == 0 {
		return false
	}
	for _, mailbox := range mailboxes {
		if imapUIDNumber(mailbox.LastSyncUID) <= 0 {
			return false
		}
	}
	return true
}

func imapSelectLastUID(lines []string) int {
	for _, line := range lines {
		upper := strings.ToUpper(line)
		idx := strings.Index(upper, "UIDNEXT")
		if idx < 0 {
			continue
		}
		tail := line[idx+len("UIDNEXT"):]
		fields := strings.FieldsFunc(tail, func(r rune) bool {
			return r < '0' || r > '9'
		})
		if len(fields) == 0 {
			continue
		}
		nextUID, err := strconv.Atoi(fields[0])
		if err == nil && nextUID > 1 {
			return nextUID - 1
		}
	}
	return 0
}

func imapUIDNumber(value string) int {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "imap:")
	uid, err := strconv.Atoi(value)
	if err != nil || uid <= 0 {
		return 0
	}
	return uid
}

func imapLineHasExistsEvent(line string) bool {
	parts := strings.Fields(strings.TrimSpace(line))
	return len(parts) >= 3 && parts[0] == "*" && strings.EqualFold(parts[2], "EXISTS")
}

type iCloudIMAPFetchedMessage struct {
	UID                string
	Folder             string
	Raw                []byte
	ProviderReceivedAt time.Time
}

func iCloudIMAPMessagesByMailbox(fetched []iCloudIMAPFetchedMessage, mailboxes []Mailbox, after time.Time, keyword string, ignoredEmails ...string) map[string][]ICloudSyncedMessage {
	now := time.Now()
	aliases := make(map[string]string)
	afterByMailbox := make(map[string]time.Time)
	ignored := make(map[string]struct{}, len(ignoredEmails))
	for _, email := range ignoredEmails {
		if email = normalizeICloudIMAPEmail(email); email != "" {
			ignored[email] = struct{}{}
		}
	}
	for _, mailbox := range mailboxes {
		id := strings.TrimSpace(mailbox.ID)
		email := normalizeICloudIMAPEmail(mailbox.Email)
		if id == "" || email == "" {
			continue
		}
		if _, skip := ignored[email]; skip {
			continue
		}
		aliases[id] = email
		afterByMailbox[id] = mailboxSyncAfter(mailbox, after, now)
	}
	out := make(map[string][]ICloudSyncedMessage, len(aliases))
	for _, item := range fetched {
		message, recipients, ok := parseICloudIMAPMessage(item)
		if !ok {
			continue
		}
		if !looksLikeVerificationText(message.Subject+"\n"+message.Body, keyword) {
			continue
		}
		matchedMailboxIDs := matchingMailboxIDs(recipients, aliases)
		if len(matchedMailboxIDs) == 0 {
			continue
		}
		for _, mailboxID := range matchedMailboxIDs {
			if after := afterByMailbox[mailboxID]; !after.IsZero() && message.ReceivedAt.Before(after) {
				continue
			}
			out[mailboxID] = append(out[mailboxID], message)
		}
	}
	for mailboxID := range out {
		sortMessagesByReceivedAt(out[mailboxID])
	}
	return out
}

func parseICloudIMAPMessage(item iCloudIMAPFetchedMessage) (ICloudSyncedMessage, string, bool) {
	msg, err := mail.ReadMessage(bytes.NewReader(item.Raw))
	if err != nil {
		return ICloudSyncedMessage{}, "", false
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(msg.Body, 1<<20))
	body := decodeICloudIMAPBody(msg.Header, bodyBytes)
	subject := decodeMIMEHeader(msg.Header.Get("Subject"))
	from := decodeMIMEHeader(msg.Header.Get("From"))
	receivedAt := time.Now()
	if date, err := msg.Header.Date(); err == nil {
		receivedAt = date
	}
	uid := strings.TrimSpace(item.UID)
	remoteID := "imap"
	if folder := strings.TrimSpace(item.Folder); folder != "" {
		remoteID += ":" + folder
	}
	if uid != "" {
		remoteID += ":" + uid
	}
	recipients := imapHeaderText(msg.Header)
	return ICloudSyncedMessage{
		RemoteID:           remoteID,
		UID:                uid,
		Subject:            subject,
		From:               from,
		Body:               normalizeMailBody(subject + "\n" + body),
		ReceivedAt:         receivedAt,
		ProviderReceivedAt: item.ProviderReceivedAt,
	}, recipients, true
}

func normalizeICloudIMAPState(state LoginState) (LoginState, error) {
	email := normalizeICloudIMAPEmail(state.IMAPEmail)
	if email == "" {
		email = normalizeICloudIMAPEmail(state.IMAPUsername)
	}
	if email == "" {
		return LoginState{}, errCode("imap_session_missing", "未保存取码登录，请先连接取码邮箱", true)
	}
	state.IMAPHost = firstNonEmpty(strings.TrimSpace(state.IMAPHost), defaultICloudIMAPHost)
	authMethod := strings.ToLower(strings.TrimSpace(state.IMAPAuthMethod))
	if authMethod == "" {
		if strings.EqualFold(state.IMAPHost, "outlook.office365.com") {
			authMethod = "xoauth2"
		} else {
			authMethod = "password"
		}
	}
	if strings.EqualFold(state.IMAPHost, "outlook.office365.com") && authMethod != "xoauth2" {
		return LoginState{}, errCode("microsoft_oauth_required", "Outlook 已改用 Microsoft OAuth2，请重新连接 Microsoft", false)
	}
	if authMethod != "password" && authMethod != "xoauth2" {
		return LoginState{}, errCode("imap_auth_method_invalid", "IMAP 认证方式不受支持", false)
	}
	appPassword := strings.TrimSpace(state.IMAPAppPassword)
	if authMethod == "password" && appPassword == "" {
		return LoginState{}, errCode("imap_app_password_missing", "取码登录缺少授权密码，请重新保存取码登录", false)
	}
	if authMethod == "xoauth2" && (strings.TrimSpace(state.OAuthClientID) == "" || (strings.TrimSpace(state.OAuthAccessToken) == "" && strings.TrimSpace(state.OAuthRefreshToken) == "")) {
		return LoginState{}, errCode("microsoft_oauth_missing", "Outlook 授权数据不完整，请重新连接 Microsoft", false)
	}
	state.IMAPEmail = email
	state.IMAPUsername = firstNonEmpty(strings.TrimSpace(state.IMAPUsername), email)
	if state.IMAPPort == 0 {
		state.IMAPPort = defaultICloudIMAPPort
	}
	state.IMAPAppPassword = appPassword
	state.IMAPAuthMethod = authMethod
	return state, nil
}

func imapLiteralSize(line string) (int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	start := strings.LastIndex(line, "{")
	if start < 0 || start+1 >= len(line) {
		return 0, false
	}
	value := strings.TrimSuffix(line[start+1:], "}")
	value = strings.TrimSuffix(value, "+")
	n, err := strconv.Atoi(value)
	return n, err == nil && n >= 0
}

func imapSearchUIDs(lines []string) []int {
	var out []int
	for _, line := range lines {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "* SEARCH") {
			continue
		}
		for _, part := range strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "* SEARCH"))) {
			n, err := strconv.Atoi(part)
			if err == nil && n > 0 {
				out = append(out, n)
			}
		}
	}
	return out
}

func imapFetchUIDs(lines []string) []int {
	var out []int
	for _, line := range lines {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if strings.EqualFold(strings.Trim(fields[i], "()"), "UID") {
				value := strings.Trim(fields[i+1], "()")
				if n, err := strconv.Atoi(value); err == nil && n > 0 {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

func imapFetchInternalDates(lines []string) map[int]time.Time {
	out := make(map[int]time.Time)
	for _, line := range lines {
		uids := imapFetchUIDs([]string{line})
		if len(uids) == 0 {
			continue
		}
		upper := strings.ToUpper(line)
		const marker = `INTERNALDATE "`
		start := strings.Index(upper, marker)
		if start < 0 {
			continue
		}
		value := line[start+len(marker):]
		end := strings.Index(value, `"`)
		if end < 0 {
			continue
		}
		parsed, err := time.Parse("2-Jan-2006 15:04:05 -0700", value[:end])
		if err == nil {
			out[uids[0]] = parsed
		}
	}
	return out
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func lastIntValues(values []int, limit int) []int {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return values[len(values)-limit:]
}

func limitIMAPUIDs(values []int, limit int, incremental bool) []int {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	if incremental {
		return values[:limit]
	}
	return lastIntValues(values, limit)
}

func chunkInts(values []int, size int) [][]int {
	if size <= 0 {
		size = len(values)
	}
	var out [][]int
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		out = append(out, values[start:end])
	}
	return out
}

func imapUIDSet(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func imapHeaderText(header mail.Header) string {
	var out []string
	for key, values := range header {
		for _, value := range values {
			out = append(out, key+": "+decodeMIMEHeader(value))
		}
	}
	return strings.Join(out, "\n")
}

func decodeMIMEHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decoded)
}

func decodeICloudIMAPBody(header mail.Header, body []byte) string {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(header.Get("Content-Type")))
	}
	decoded := decodeICloudIMAPTransfer(header.Get("Content-Transfer-Encoding"), body)
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return string(decoded)
		}
		reader := multipart.NewReader(bytes.NewReader(decoded), boundary)
		var parts []string
		for i := 0; i < 30; i++ {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			partBody, _ := io.ReadAll(io.LimitReader(part, 1<<20))
			text := decodeICloudIMAPBody(mail.Header(part.Header), partBody)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "text/") || mediaType == "" {
		return string(decoded)
	}
	return string(decoded)
}

func decodeICloudIMAPTransfer(encoding string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err == nil {
			return decoded
		}
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(body)))
		if err == nil {
			return decoded
		}
	}
	return body
}

func sortMessagesByReceivedAt(messages []ICloudSyncedMessage) {
	for i := 1; i < len(messages); i++ {
		for j := i; j > 0 && messages[j].ReceivedAt.After(messages[j-1].ReceivedAt); j-- {
			messages[j], messages[j-1] = messages[j-1], messages[j]
		}
	}
}
