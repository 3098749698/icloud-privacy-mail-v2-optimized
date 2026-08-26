package protocol

import "testing"

func TestAppleAccountPrivateEmailRemotes(t *testing.T) {
	inactive := false
	response := appleAccountPrivateEmailList{
		ForwardToEmailAddress: "owner@example.com",
		PrivateEmailList: []appleAccountPrivateEmailItem{{
			ID: "active-id", EmailAddress: "ACTIVE@icloud.com", Label: "active", Note: "note",
		}},
		InactivePrivateEmailList: []appleAccountPrivateEmailItem{
			{ID: "inactive-id", EmailAddress: "inactive@icloud.com", Active: &inactive},
			{ID: "duplicate-id", EmailAddress: "ACTIVE@icloud.com"},
		},
	}

	items := appleAccountPrivateEmailRemotes(response)
	if len(items) != 2 {
		t.Fatalf("邮箱数=%d，期望=2", len(items))
	}
	if items[0].Email != "active@icloud.com" || !items[0].IsActive || items[0].ForwardToEmail != "owner@example.com" {
		t.Fatalf("活跃邮箱映射错误：%+v", items[0])
	}
	if items[1].Email != "inactive@icloud.com" || items[1].IsActive {
		t.Fatalf("停用邮箱映射错误：%+v", items[1])
	}
}
