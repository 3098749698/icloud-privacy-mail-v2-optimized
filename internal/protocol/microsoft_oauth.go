package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	microsoftOAuthDeviceCodeURL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/devicecode"
	microsoftOAuthTokenURL      = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
	microsoftIMAPScope          = "offline_access openid profile https://outlook.office.com/IMAP.AccessAsUser.All"
)

type MicrosoftDeviceCode struct {
	DeviceCode              string    `json:"-"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	Message                 string    `json:"message,omitempty"`
	ExpiresAt               time.Time `json:"expires_at"`
	IntervalSeconds         int       `json:"interval_seconds"`
}

type MicrosoftTokenResult struct {
	Status       string
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Message      string
	RetryAfter   int
}

type microsoftDeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	Message                 string `json:"message"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

type microsoftTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func StartMicrosoftDeviceCode(ctx context.Context, clientID string) (MicrosoftDeviceCode, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return MicrosoftDeviceCode{}, errCode("microsoft_client_id_missing", "请先填写 Microsoft 应用客户端 ID", false)
	}
	form := url.Values{"client_id": {clientID}, "scope": {microsoftIMAPScope}}
	var response microsoftDeviceCodeResponse
	if err := postMicrosoftOAuthForm(ctx, microsoftOAuthDeviceCodeURL, form, &response); err != nil {
		return MicrosoftDeviceCode{}, err
	}
	if response.Error != "" {
		return MicrosoftDeviceCode{}, microsoftOAuthResponseError(response.Error, response.ErrorDescription)
	}
	if response.DeviceCode == "" || response.UserCode == "" || response.VerificationURI == "" {
		return MicrosoftDeviceCode{}, errCode("microsoft_device_code_invalid", "Microsoft 返回的设备码数据不完整", true)
	}
	if response.Interval < 1 {
		response.Interval = 5
	}
	return MicrosoftDeviceCode{
		DeviceCode:              response.DeviceCode,
		UserCode:                response.UserCode,
		VerificationURI:         response.VerificationURI,
		VerificationURIComplete: response.VerificationURIComplete,
		Message:                 response.Message,
		ExpiresAt:               time.Now().Add(time.Duration(response.ExpiresIn) * time.Second),
		IntervalSeconds:         response.Interval,
	}, nil
}

func PollMicrosoftDeviceCode(ctx context.Context, clientID, deviceCode string) (MicrosoftTokenResult, error) {
	clientID = strings.TrimSpace(clientID)
	deviceCode = strings.TrimSpace(deviceCode)
	if clientID == "" || deviceCode == "" {
		return MicrosoftTokenResult{}, errCode("microsoft_oauth_pending_missing", "Microsoft 授权任务不存在或已过期", false)
	}
	form := url.Values{
		"client_id":   {clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	var response microsoftTokenResponse
	if err := postMicrosoftOAuthForm(ctx, microsoftOAuthTokenURL, form, &response); err != nil {
		return MicrosoftTokenResult{}, err
	}
	switch response.Error {
	case "":
		if response.AccessToken == "" || response.RefreshToken == "" {
			return MicrosoftTokenResult{}, errCode("microsoft_token_invalid", "Microsoft 返回的 Token 数据不完整", true)
		}
		return MicrosoftTokenResult{Status: "authorized", AccessToken: response.AccessToken, RefreshToken: response.RefreshToken, ExpiresIn: response.ExpiresIn}, nil
	case "authorization_pending":
		return MicrosoftTokenResult{Status: response.Error, Message: "等待完成 Microsoft 授权"}, nil
	case "slow_down":
		return MicrosoftTokenResult{Status: response.Error, Message: "Microsoft 要求降低查询频率", RetryAfter: 5}, nil
	case "authorization_declined", "expired_token", "bad_verification_code":
		return MicrosoftTokenResult{Status: response.Error, Message: firstNonEmpty(response.ErrorDescription, "Microsoft 授权未完成")}, nil
	default:
		return MicrosoftTokenResult{}, microsoftOAuthResponseError(response.Error, response.ErrorDescription)
	}
}

func PrepareIMAPLoginState(ctx context.Context, state LoginState) (LoginState, error) {
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return LoginState{}, err
	}
	if !strings.EqualFold(state.IMAPAuthMethod, "xoauth2") {
		return state, nil
	}
	if state.OAuthAccessToken != "" && state.OAuthTokenExpiresAt.After(time.Now().Add(2*time.Minute)) {
		return state, nil
	}
	return refreshMicrosoftOAuthState(ctx, state)
}

func refreshMicrosoftOAuthState(ctx context.Context, state LoginState) (LoginState, error) {
	if strings.TrimSpace(state.OAuthClientID) == "" || strings.TrimSpace(state.OAuthRefreshToken) == "" {
		return LoginState{}, errCode("microsoft_refresh_token_missing", "Outlook 授权已失效，请重新连接 Microsoft", false)
	}
	form := url.Values{
		"client_id":     {state.OAuthClientID},
		"refresh_token": {state.OAuthRefreshToken},
		"grant_type":    {"refresh_token"},
		"scope":         {microsoftIMAPScope},
	}
	var response microsoftTokenResponse
	if err := postMicrosoftOAuthForm(ctx, microsoftOAuthTokenURL, form, &response); err != nil {
		return LoginState{}, err
	}
	if response.Error != "" {
		return LoginState{}, microsoftOAuthResponseError(response.Error, response.ErrorDescription)
	}
	if response.AccessToken == "" {
		return LoginState{}, errCode("microsoft_token_invalid", "Microsoft 刷新的 Token 数据不完整", true)
	}
	if response.ExpiresIn < 1 {
		response.ExpiresIn = 3600
	}
	state.OAuthAccessToken = response.AccessToken
	if response.RefreshToken != "" {
		state.OAuthRefreshToken = response.RefreshToken
	}
	state.SavedAt = time.Now()
	state.OAuthTokenExpiresAt = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	return state, nil
}

func postMicrosoftOAuthForm(ctx context.Context, endpoint string, form url.Values, destination any) error {
	requestCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errCode("microsoft_oauth_request_failed", "创建 Microsoft 授权请求失败："+err.Error(), true)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errCode("microsoft_oauth_request_failed", "连接 Microsoft 授权服务失败："+err.Error(), true)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return errCode("microsoft_oauth_response_failed", "读取 Microsoft 授权响应失败："+err.Error(), true)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return errCode("microsoft_oauth_response_invalid", fmt.Sprintf("Microsoft 授权响应格式异常（HTTP %d）", resp.StatusCode), true)
	}
	if resp.StatusCode >= 500 {
		return errCode("microsoft_oauth_unavailable", fmt.Sprintf("Microsoft 授权服务暂时异常（HTTP %d）", resp.StatusCode), true)
	}
	return nil
}

func microsoftOAuthResponseError(code, description string) error {
	description = strings.TrimSpace(description)
	if description == "" {
		description = code
	}
	retryable := code == "temporarily_unavailable" || code == "server_error"
	return errCode("microsoft_oauth_"+strings.TrimSpace(code), "Microsoft 授权失败："+description, retryable)
}
