package protocol

import (
	"context"
	"strings"
	"time"
)

func LoginStateForKind(session ICloudSession, kind string) (LoginState, bool) {
	for _, state := range session.LoginStates {
		if state.Kind == kind {
			return state, true
		}
	}
	return LoginState{}, false
}

func WithLoginState(session ICloudSession, next LoginState) ICloudSession {
	next.Kind = strings.TrimSpace(next.Kind)
	for i := range session.LoginStates {
		if session.LoginStates[i].Kind == next.Kind {
			session.LoginStates[i] = next
			return session
		}
	}
	session.LoginStates = append(session.LoginStates, next)
	return session
}

// normalizeICloudWebSession 让旧接口请求优先使用 iCloud Web 专属登录态。
// Apple Account 新接口保活后可能会更新顶层 Host，但不应影响 iCloud Web 的中国区域名和 Cookie。
func normalizeICloudWebSession(session ICloudSession) ICloudSession {
	state, ok := LoginStateForKind(session, LoginStateICloudWeb)
	if !ok {
		return session
	}
	if host := strings.TrimSpace(state.Host); host != "" {
		session.Host = host
	}
	if len(state.Cookies) > 0 {
		session.Cookies = append([]SessionCookie(nil), state.Cookies...)
	}
	return session
}

// SupportsICloudWebMailboxManagement reports whether the saved session has the
// legacy iCloud Web fields required by list/delete mailbox APIs. Apple Account
// management cookies alone can create aliases but do not satisfy these APIs.
func SupportsICloudWebMailboxManagement(session ICloudSession) bool {
	session = normalizeICloudWebSession(session)
	return strings.TrimSpace(session.PremiumMailBaseURL) != "" &&
		strings.TrimSpace(session.DSID) != "" && len(session.Cookies) > 0
}

func (c *ICloudSessionValidator) ValidateSession(ctx context.Context, session ICloudSession, defaultHost string) (ICloudSession, error) {
	session = normalizeICloudWebSession(session)
	result, err := c.Validate(ctx, session.Cookies, firstNonEmpty(session.Host, defaultHost))
	checkedAt := time.Now()
	if err != nil {
		session.LastCheckedAt = checkedAt
		session.LastCheckOK = false
		session.LastStatusMessage = err.Error()
		return session, err
	}
	session.AppleID = firstNonEmpty(result.AppleID, session.AppleID)
	session.DSID = firstNonEmpty(result.DSID, session.DSID)
	session.ClientID = firstNonEmpty(result.ClientID, session.ClientID)
	session.ClientBuildNumber = firstNonEmpty(result.ClientBuildNumber, session.ClientBuildNumber)
	session.MasteringNumber = firstNonEmpty(result.MasteringNumber, session.MasteringNumber)
	session.PremiumMailBaseURL = firstNonEmpty(result.PremiumMailBaseURL, session.PremiumMailBaseURL)
	session.MailGatewayBaseURL = firstNonEmpty(result.MailGatewayBaseURL, session.MailGatewayBaseURL)
	session.MailBaseURL = firstNonEmpty(result.MailBaseURL, session.MailBaseURL)
	session.IsICloudPlus = result.IsICloudPlus
	session.CanCreateHME = result.CanCreateHME
	session.LastCheckedAt = checkedAt
	session.LastCheckOK = true
	session.LastStatusMessage = "iCloud Web 登录态正常"
	return session, nil
}
