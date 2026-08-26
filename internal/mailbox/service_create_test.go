package mailbox

import (
	"testing"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestHasICloudWebCreateStateDoesNotConfuseAppleAccountState(t *testing.T) {
	session := domain.ICloudSession{LoginStates: []domain.LoginState{{
		Kind: domain.LoginStateAppleAccount,
		Scnt: "apple-account-scnt",
	}}}
	if hasICloudWebCreateState(session) {
		t.Fatal("Apple Account 新接口登录态不应被当成 iCloud Web 登录态")
	}

	session.LoginStates = append(session.LoginStates, domain.LoginState{Kind: domain.LoginStateICloudWeb})
	if !hasICloudWebCreateState(session) {
		t.Fatal("已保存 iCloud Web 登录态时应允许旧接口回退")
	}
}

func TestHasICloudWebCreateStateSupportsLegacyTopLevelSession(t *testing.T) {
	session := domain.ICloudSession{
		DSID:               "12345",
		PremiumMailBaseURL: "https://p-mailws.icloud.com",
		Cookies:            []domain.SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture"}},
	}
	if !hasICloudWebCreateState(session) {
		t.Fatal("旧版顶层 iCloud Web 登录态应继续支持回退")
	}
}
