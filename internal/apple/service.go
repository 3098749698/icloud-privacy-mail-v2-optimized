package apple

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

type Service struct {
	cfg       config.Config
	store     *store.Store
	auth      *protocol.AuthFacade
	client    *protocol.ICloudClient
	validator *protocol.ICloudSessionValidator
	oauthMu   sync.Mutex
	oauth     map[string]outlookOAuthPending
	loginMu   sync.Mutex
	logins    map[string]loginCredentialPending
}

type LoginRequest struct {
	Flow            protocol.AuthFlow
	AppleID         string
	Password        string
	TwoFactorMethod string
}

type LoginResult struct {
	PendingID string         `json:"pending_id,omitempty"`
	Needs2FA  bool           `json:"needs_2fa"`
	Message   string         `json:"message"`
	AppleID   string         `json:"apple_id"`
	ExpiresAt time.Time      `json:"expires_at,omitempty"`
	Account   AccountSummary `json:"account,omitempty"`
}

type ReloginRequest struct {
	Password        string
	TwoFactorMethod string
}

type loginCredentialPending struct {
	AccountID       string
	AppleID         string
	Password        string
	TwoFactorMethod string
	ExpiresAt       time.Time
}

type LoginStateSummary struct {
	Kind              string    `json:"kind"`
	Saved             bool      `json:"saved"`
	LastCheckedAt     time.Time `json:"last_checked_at,omitempty"`
	LastCheckOK       bool      `json:"last_check_ok"`
	LastStatusMessage string    `json:"last_status_message,omitempty"`
	ManageExpiresAt   time.Time `json:"manage_expires_at,omitempty"`
	IMAPEmail         string    `json:"imap_email,omitempty"`
	IMAPHost          string    `json:"imap_host,omitempty"`
	IMAPProvider      string    `json:"imap_provider,omitempty"`
	IMAPAuthMethod    string    `json:"imap_auth_method,omitempty"`
	OAuthClientID     string    `json:"oauth_client_id,omitempty"`
	ReloginRequired   bool      `json:"relogin_required,omitempty"`
}

type IMAPLoginRequest struct {
	Provider    string
	Email       string
	AppPassword string
}

type OutlookOAuthStartRequest struct {
	Email    string
	ClientID string
}

type OutlookOAuthStartResult struct {
	PendingID               string    `json:"pending_id"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	Message                 string    `json:"message,omitempty"`
	ExpiresAt               time.Time `json:"expires_at"`
	IntervalSeconds         int       `json:"interval_seconds"`
}

type OutlookOAuthPollResult struct {
	Status     string          `json:"status"`
	Message    string          `json:"message,omitempty"`
	RetryAfter int             `json:"retry_after_seconds,omitempty"`
	Account    *AccountSummary `json:"account,omitempty"`
}

type outlookOAuthPending struct {
	AccountID  string
	Email      string
	ClientID   string
	DeviceCode string
	ExpiresAt  time.Time
	Interval   int
}

type AccountSummary struct {
	domain.AppleAccount
	LoginStates     []LoginStateSummary `json:"login_states"`
	AutoDeleteReady bool                `json:"auto_delete_ready"`
	ReloginReady    bool                `json:"relogin_ready"`
}

func NewService(cfg config.Config, state *store.Store) *Service {
	return &Service{
		cfg:       cfg,
		store:     state,
		auth:      protocol.NewAuthFacade(),
		client:    protocol.NewICloudClient(),
		validator: protocol.NewICloudSessionValidator(),
		oauth:     make(map[string]outlookOAuthPending),
		logins:    make(map[string]loginCredentialPending),
	}
}

func (s *Service) StartLogin(ctx context.Context, request LoginRequest) (LoginResult, error) {
	result, err := s.auth.Start(ctx, request.Flow, request.AppleID, request.Password, s.cfg.ICloudDefaultHost, s.cfg.ICloudClientID, request.TwoFactorMethod)
	if err != nil {
		return LoginResult{}, err
	}
	out := LoginResult{
		PendingID: result.PendingID,
		Needs2FA:  result.Needs2FA,
		Message:   result.Message,
		AppleID:   result.AppleID,
		ExpiresAt: result.ExpiresAt,
	}
	if !result.Needs2FA {
		saved, err := s.store.SaveICloudSession(result.Session)
		if err != nil {
			return LoginResult{}, err
		}
		out.Account = s.summaryForSession(saved)
		if err := s.store.SaveAppleCredential(saved.AccountID, saved.AppleID, request.Password, request.TwoFactorMethod); err != nil {
			return LoginResult{}, err
		}
		out.Account.ReloginReady = true
	} else {
		s.rememberLoginCredential(result.PendingID, loginCredentialPending{
			AppleID: result.AppleID, Password: request.Password, TwoFactorMethod: request.TwoFactorMethod, ExpiresAt: result.ExpiresAt,
		})
	}
	return out, nil
}

func (s *Service) Submit2FA(ctx context.Context, pendingID, code string, phoneNumber json.RawMessage) (LoginResult, error) {
	credential, hasCredential := s.loginCredential(pendingID)
	session, err := s.auth.Submit2FA(ctx, pendingID, code, phoneNumber)
	if err != nil {
		return LoginResult{}, err
	}
	if credential.AccountID != "" {
		session.AccountID = credential.AccountID
	}
	saved, err := s.store.SaveICloudSession(session)
	if err != nil {
		return LoginResult{}, err
	}
	if hasCredential {
		if err := s.store.SaveAppleCredential(saved.AccountID, saved.AppleID, credential.Password, credential.TwoFactorMethod); err != nil {
			return LoginResult{}, err
		}
		s.forgetLoginCredential(pendingID)
	}
	account := s.summaryForSession(saved)
	return LoginResult{
		Needs2FA: false,
		Message:  "Apple 登录和 2FA 已完成",
		AppleID:  saved.AppleID,
		Account:  account,
	}, nil
}

func (s *Service) StartRelogin(ctx context.Context, accountID string, request ReloginRequest) (LoginResult, error) {
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return LoginResult{}, errors.New("Apple 账号不存在")
	}
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		return LoginResult{}, errors.New("Apple 账号登录态不存在")
	}
	password := request.Password
	method := strings.TrimSpace(request.TwoFactorMethod)
	if strings.TrimSpace(password) == "" {
		credential, found, err := s.store.AppleCredential(accountID)
		if err != nil {
			return LoginResult{}, err
		}
		if !found {
			return LoginResult{}, protocol.NewError("apple_password_not_saved", "该历史账号尚未保存登录密码，请先输入一次密码", false)
		}
		password = credential.Password
		if method == "" {
			method = credential.TwoFactorMethod
		}
	}
	if method == "" {
		method = "trusted_device"
	}
	flow, trustedState, err := reloginFlowAndState(session)
	if err != nil {
		return LoginResult{}, err
	}
	result, err := s.auth.StartWithTrustedState(ctx, flow, account.AppleID, password, s.cfg.ICloudDefaultHost, s.cfg.ICloudClientID, method, trustedState)
	if err != nil {
		return LoginResult{}, err
	}
	out := LoginResult{
		PendingID: result.PendingID,
		Needs2FA:  result.Needs2FA,
		Message:   result.Message,
		AppleID:   account.AppleID,
		ExpiresAt: result.ExpiresAt,
	}
	pending := loginCredentialPending{
		AccountID: accountID, AppleID: account.AppleID, Password: password, TwoFactorMethod: method, ExpiresAt: result.ExpiresAt,
	}
	if result.Needs2FA {
		s.rememberLoginCredential(result.PendingID, pending)
		return out, nil
	}
	result.Session.AccountID = accountID
	result.Session.OwnerID = account.OwnerID
	result.Session.AppleID = account.AppleID
	saved, err := s.store.SaveICloudSessionWithEvent(result.Session, "info", "Apple 账号重新登录成功")
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.store.SaveAppleCredential(accountID, account.AppleID, password, method); err != nil {
		return LoginResult{}, err
	}
	out.Message = "Apple 账号已重新登录"
	out.Account = s.summaryForSession(saved)
	return out, nil
}

func reloginFlowAndState(session domain.ICloudSession) (protocol.AuthFlow, domain.LoginState, error) {
	if state, ok := protocol.LoginStateForKind(session, domain.LoginStateAppleAccount); ok {
		return protocol.AuthFlowAppleAccount, state, nil
	}
	if state, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudWeb); ok {
		return protocol.AuthFlowICloudWeb, state, nil
	}
	if len(session.Cookies) > 0 {
		return protocol.AuthFlowICloudWeb, domain.LoginState{
			Kind: domain.LoginStateICloudWeb, Host: session.Host, Cookies: append([]domain.SessionCookie(nil), session.Cookies...),
		}, nil
	}
	return "", domain.LoginState{}, errors.New("当前账号没有可重新登录的 Apple 协议通道")
}

func (s *Service) rememberLoginCredential(pendingID string, credential loginCredentialPending) {
	pendingID = strings.TrimSpace(pendingID)
	if pendingID == "" {
		return
	}
	s.loginMu.Lock()
	s.cleanupLoginCredentialsLocked(time.Now())
	s.logins[pendingID] = credential
	s.loginMu.Unlock()
	delay := time.Until(credential.ExpiresAt)
	if delay <= 0 {
		delay = 10 * time.Minute
	}
	time.AfterFunc(delay, func() {
		s.loginMu.Lock()
		if current, ok := s.logins[pendingID]; ok && current.ExpiresAt.Equal(credential.ExpiresAt) {
			delete(s.logins, pendingID)
		}
		s.loginMu.Unlock()
	})
}

func (s *Service) loginCredential(pendingID string) (loginCredentialPending, bool) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.cleanupLoginCredentialsLocked(time.Now())
	credential, ok := s.logins[strings.TrimSpace(pendingID)]
	return credential, ok
}

func (s *Service) cleanupLoginCredentialsLocked(now time.Time) {
	for pendingID, credential := range s.logins {
		if !credential.ExpiresAt.IsZero() && !now.Before(credential.ExpiresAt) {
			delete(s.logins, pendingID)
		}
	}
}

func (s *Service) forgetLoginCredential(pendingID string) {
	s.loginMu.Lock()
	delete(s.logins, strings.TrimSpace(pendingID))
	s.loginMu.Unlock()
}

func (s *Service) Accounts() []AccountSummary {
	accounts := s.store.AppleAccounts()
	sessions := s.store.ICloudSessions()
	byAccount := make(map[string]domain.ICloudSession, len(sessions))
	for _, session := range sessions {
		byAccount[session.AccountID] = session
	}
	out := make([]AccountSummary, 0, len(accounts))
	for _, account := range accounts {
		summary := AccountSummary{AppleAccount: account, LoginStates: []LoginStateSummary{}}
		summary.ReloginReady = s.store.HasAppleCredential(account.ID)
		if session, ok := byAccount[account.ID]; ok {
			summary.LoginStates = publicLoginStates(session)
			summary.AutoDeleteReady = s.supportsMailboxAutoDelete(session)
		}
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (s *Service) Account(accountID string) (AccountSummary, error) {
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return AccountSummary{}, errors.New("Apple 账号不存在")
	}
	summary := AccountSummary{AppleAccount: account, LoginStates: []LoginStateSummary{}}
	summary.ReloginReady = s.store.HasAppleCredential(account.ID)
	if session, ok := s.store.ICloudSessionByAccountID(accountID); ok {
		summary.LoginStates = publicLoginStates(session)
		summary.AutoDeleteReady = s.supportsMailboxAutoDelete(session)
	}
	return summary, nil
}

func (s *Service) Check(ctx context.Context, accountID string) (AccountSummary, error) {
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		return AccountSummary{}, errors.New("Apple 账号登录态不存在")
	}
	checkedAny := false
	var lastErr error
	if _, ok := protocol.LoginStateForKind(session, domain.LoginStateAppleAccount); ok {
		checkedAny = true
		updated, err := s.client.CheckAppleAccountManageSession(ctx, session)
		if err != nil {
			lastErr = err
			markLoginState(&session, domain.LoginStateAppleAccount, false, err.Error())
		} else {
			session = updated
			markLoginState(&session, domain.LoginStateAppleAccount, true, "Apple Account 登录态正常")
		}
	}
	if _, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudWeb); ok || len(session.Cookies) > 0 {
		checkedAny = true
		updated, err := s.validator.ValidateSession(ctx, session, s.cfg.ICloudDefaultHost)
		session = updated
		if err != nil {
			lastErr = err
			markLoginState(&session, domain.LoginStateICloudWeb, false, err.Error())
		} else {
			markLoginState(&session, domain.LoginStateICloudWeb, true, "iCloud Web 登录态正常")
		}
	}
	if state, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP); ok {
		checkedAny = true
		prepared, prepareErr := protocol.PrepareIMAPLoginState(ctx, state)
		if prepareErr == nil {
			state = prepared
			session = protocol.WithLoginState(session, state)
		}
		err := prepareErr
		if err == nil {
			err = protocol.CheckIMAPLogin(ctx, state)
		}
		if err != nil {
			lastErr = err
			markLoginState(&session, domain.LoginStateICloudIMAP, false, err.Error())
		} else {
			markLoginState(&session, domain.LoginStateICloudIMAP, true, "IMAP 取码登录正常")
		}
	}
	if !checkedAny {
		return AccountSummary{}, errors.New("该账号没有可检测的登录态")
	}
	saved, err := s.store.SaveICloudSession(session)
	if err != nil {
		return AccountSummary{}, err
	}
	summary := s.summaryForSession(saved)
	if lastErr != nil {
		return summary, lastErr
	}
	return summary, nil
}

func (s *Service) SaveIMAP(ctx context.Context, accountID string, request IMAPLoginRequest) (AccountSummary, error) {
	state, err := newIMAPLoginState(request)
	if err != nil {
		return AccountSummary{}, err
	}
	if err := protocol.CheckIMAPLogin(ctx, state); err != nil {
		return AccountSummary{}, err
	}
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		account, exists := s.store.FindAppleAccount(accountID)
		if !exists {
			return AccountSummary{}, errors.New("Apple 账号不存在")
		}
		session = domain.ICloudSession{AccountID: account.ID, AppleID: account.AppleID, SavedAt: time.Now()}
	}
	session = protocol.WithLoginState(session, state)
	saved, err := s.store.SaveICloudSession(session)
	if err != nil {
		return AccountSummary{}, err
	}
	return s.summaryForSession(saved), nil
}

func (s *Service) StartOutlookOAuth(ctx context.Context, accountID string, request OutlookOAuthStartRequest) (OutlookOAuthStartResult, error) {
	if _, exists := s.store.FindAppleAccount(accountID); !exists {
		return OutlookOAuthStartResult{}, errors.New("Apple 账号不存在")
	}
	email := strings.ToLower(strings.TrimSpace(request.Email))
	if email == "" {
		return OutlookOAuthStartResult{}, errors.New("请输入 Outlook 邮箱账号")
	}
	clientID := strings.TrimSpace(request.ClientID)
	if clientID == "" {
		clientID = strings.TrimSpace(s.cfg.MicrosoftOAuthClientID)
	}
	device, err := protocol.StartMicrosoftDeviceCode(ctx, clientID)
	if err != nil {
		return OutlookOAuthStartResult{}, err
	}
	pendingID, err := newOAuthPendingID()
	if err != nil {
		return OutlookOAuthStartResult{}, err
	}
	s.oauthMu.Lock()
	for id, pending := range s.oauth {
		if pending.ExpiresAt.Before(time.Now()) || (pending.AccountID == accountID && strings.EqualFold(pending.Email, email)) {
			delete(s.oauth, id)
		}
	}
	s.oauth[pendingID] = outlookOAuthPending{
		AccountID: accountID, Email: email, ClientID: clientID, DeviceCode: device.DeviceCode,
		ExpiresAt: device.ExpiresAt, Interval: device.IntervalSeconds,
	}
	s.oauthMu.Unlock()
	return OutlookOAuthStartResult{
		PendingID: pendingID, UserCode: device.UserCode, VerificationURI: device.VerificationURI,
		VerificationURIComplete: device.VerificationURIComplete, Message: device.Message,
		ExpiresAt: device.ExpiresAt, IntervalSeconds: device.IntervalSeconds,
	}, nil
}

func (s *Service) PollOutlookOAuth(ctx context.Context, accountID, pendingID string) (OutlookOAuthPollResult, error) {
	pendingID = strings.TrimSpace(pendingID)
	s.oauthMu.Lock()
	pending, ok := s.oauth[pendingID]
	s.oauthMu.Unlock()
	if !ok || pending.ExpiresAt.Before(time.Now()) {
		if ok {
			s.oauthMu.Lock()
			delete(s.oauth, pendingID)
			s.oauthMu.Unlock()
		}
		return OutlookOAuthPollResult{}, errors.New("Microsoft 授权任务不存在或已过期，请重新开始")
	}
	if pending.AccountID != strings.TrimSpace(accountID) {
		return OutlookOAuthPollResult{}, errors.New("Microsoft 授权任务与当前账号不匹配")
	}
	token, err := protocol.PollMicrosoftDeviceCode(ctx, pending.ClientID, pending.DeviceCode)
	if err != nil {
		return OutlookOAuthPollResult{}, err
	}
	if token.Status != "authorized" {
		if token.Status == "authorization_declined" || token.Status == "expired_token" || token.Status == "bad_verification_code" {
			s.oauthMu.Lock()
			delete(s.oauth, pendingID)
			s.oauthMu.Unlock()
		}
		retryAfter := pending.Interval
		if token.RetryAfter > 0 {
			retryAfter += token.RetryAfter
		}
		return OutlookOAuthPollResult{Status: token.Status, Message: token.Message, RetryAfter: retryAfter}, nil
	}
	expiresIn := token.ExpiresIn
	if expiresIn < 1 {
		expiresIn = 3600
	}
	now := time.Now()
	state := domain.LoginState{
		Kind: domain.LoginStateICloudIMAP, Host: "outlook.office365.com", Origin: "imaps://outlook.office365.com", SavedAt: now,
		IMAPEmail: pending.Email, IMAPUsername: pending.Email, IMAPHost: "outlook.office365.com", IMAPPort: 993,
		IMAPAuthMethod: "xoauth2", OAuthClientID: pending.ClientID, OAuthAccessToken: token.AccessToken,
		OAuthRefreshToken: token.RefreshToken, OAuthTokenExpiresAt: now.Add(time.Duration(expiresIn) * time.Second),
		LastCheckedAt: now, LastCheckOK: true, LastStatusMessage: "Outlook OAuth2 IMAP 取码登录正常",
	}
	if err := protocol.CheckIMAPLogin(ctx, state); err != nil {
		return OutlookOAuthPollResult{}, err
	}
	session, ok := s.store.ICloudSessionByAccountID(pending.AccountID)
	if !ok {
		account, exists := s.store.FindAppleAccount(pending.AccountID)
		if !exists {
			return OutlookOAuthPollResult{}, errors.New("Apple 账号不存在")
		}
		session = domain.ICloudSession{AccountID: account.ID, AppleID: account.AppleID, SavedAt: now}
	}
	session = protocol.WithLoginState(session, state)
	saved, err := s.store.SaveICloudSession(session)
	if err != nil {
		return OutlookOAuthPollResult{}, err
	}
	s.oauthMu.Lock()
	delete(s.oauth, pendingID)
	s.oauthMu.Unlock()
	account := s.summaryForSession(saved)
	return OutlookOAuthPollResult{Status: "authorized", Message: "Outlook 已通过 OAuth2 连接", Account: &account}, nil
}

func newOAuthPendingID() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func newIMAPLoginState(request IMAPLoginRequest) (domain.LoginState, error) {
	provider := strings.ToLower(strings.TrimSpace(request.Provider))
	if provider == "" {
		provider = "icloud"
	}
	email := strings.ToLower(strings.TrimSpace(request.Email))
	password := strings.TrimSpace(request.AppPassword)
	if email == "" {
		return domain.LoginState{}, errors.New("请输入取码邮箱账号")
	}
	if password == "" {
		return domain.LoginState{}, errors.New("请输入邮箱授权密码")
	}

	host := "imap.mail.me.com"
	label := "iCloud"
	switch provider {
	case "icloud":
	case "qq":
		host = "imap.qq.com"
		label = "QQ 邮箱"
	default:
		return domain.LoginState{}, errors.New("暂不支持该 IMAP 服务商")
	}
	now := time.Now()
	return domain.LoginState{
		Kind:              domain.LoginStateICloudIMAP,
		Host:              host,
		Origin:            "imaps://" + host,
		SavedAt:           now,
		IMAPEmail:         email,
		IMAPUsername:      email,
		IMAPHost:          host,
		IMAPPort:          993,
		IMAPAppPassword:   password,
		IMAPAuthMethod:    "password",
		LastCheckedAt:     now,
		LastCheckOK:       true,
		LastStatusMessage: label + " IMAP 取码登录正常",
	}, nil
}

func imapProviderForHost(host string) string {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "imap.qq.com":
		return "qq"
	case "outlook.office365.com":
		return "outlook"
	default:
		return "icloud"
	}
}

func (s *Service) KeepAliveState(ctx context.Context, state domain.LoginState) (domain.LoginState, error) {
	return s.client.KeepAliveAppleAccountManageState(ctx, state)
}

func (s *Service) summaryForSession(session domain.ICloudSession) AccountSummary {
	account, _ := s.store.FindAppleAccount(session.AccountID)
	return AccountSummary{
		AppleAccount:    account,
		LoginStates:     publicLoginStates(session),
		AutoDeleteReady: s.supportsMailboxAutoDelete(session),
		ReloginReady:    s.store.HasAppleCredential(session.AccountID),
	}
}

func (s *Service) supportsMailboxAutoDelete(session domain.ICloudSession) bool {
	return protocol.SupportsAppleAccountMailboxManagement(session, s.cfg.AppleAccountAPIKey) ||
		protocol.SupportsICloudWebMailboxManagement(session)
}

func publicLoginStates(session domain.ICloudSession) []LoginStateSummary {
	out := make([]LoginStateSummary, 0, len(session.LoginStates)+1)
	seenWeb := false
	for _, state := range session.LoginStates {
		if state.Kind == domain.LoginStateICloudWeb {
			seenWeb = true
		}
		out = append(out, LoginStateSummary{
			Kind:              state.Kind,
			Saved:             loginStateSaved(state),
			LastCheckedAt:     state.LastCheckedAt,
			LastCheckOK:       state.LastCheckOK,
			LastStatusMessage: state.LastStatusMessage,
			ManageExpiresAt:   state.ManageExpiresAt,
			IMAPEmail:         state.IMAPEmail,
			IMAPHost:          state.IMAPHost,
			IMAPProvider:      imapProviderForHost(state.IMAPHost),
			IMAPAuthMethod:    state.IMAPAuthMethod,
			OAuthClientID:     state.OAuthClientID,
			ReloginRequired:   state.ReloginRequired,
		})
	}
	if !seenWeb && len(session.Cookies) > 0 {
		out = append(out, LoginStateSummary{Kind: domain.LoginStateICloudWeb, Saved: true, LastCheckedAt: session.LastCheckedAt, LastCheckOK: session.LastCheckOK, LastStatusMessage: session.LastStatusMessage})
	}
	return out
}

func loginStateSaved(state domain.LoginState) bool {
	switch state.Kind {
	case domain.LoginStateAppleAccount:
		return strings.TrimSpace(state.Scnt) != "" && len(state.Cookies) > 0
	case domain.LoginStateICloudIMAP:
		if strings.EqualFold(state.IMAPHost, "outlook.office365.com") || strings.EqualFold(state.IMAPAuthMethod, "xoauth2") {
			return strings.TrimSpace(state.IMAPEmail) != "" && strings.TrimSpace(state.OAuthClientID) != "" && strings.TrimSpace(state.OAuthRefreshToken) != ""
		}
		return strings.TrimSpace(state.IMAPEmail) != "" && strings.TrimSpace(state.IMAPAppPassword) != ""
	default:
		return len(state.Cookies) > 0
	}
}

func markLoginState(session *domain.ICloudSession, kind string, ok bool, message string) {
	state, exists := protocol.LoginStateForKind(*session, kind)
	if !exists {
		state.Kind = kind
		if kind == domain.LoginStateICloudWeb {
			state.Cookies = append([]domain.SessionCookie(nil), session.Cookies...)
		}
	}
	state.LastCheckedAt = time.Now()
	state.LastCheckOK = ok
	state.LastStatusMessage = message
	if ok {
		state.ReloginRequired = false
	}
	*session = protocol.WithLoginState(*session, state)
}
