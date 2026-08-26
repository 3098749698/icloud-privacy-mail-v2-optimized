package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/store"
)

func TestProtectedRequestRefreshesSessionCookie(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	cfg := config.Default()
	cfg.SessionTTLHours = 24 * 30
	server := New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil)))

	setup := httptest.NewRecorder()
	server.ServeHTTP(setup, httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"username":"admin","password":"test-password"}`)))
	if setup.Code != http.StatusCreated || len(setup.Result().Cookies()) != 1 {
		t.Fatalf("管理员初始化失败：status=%d cookies=%v body=%s", setup.Code, setup.Result().Cookies(), setup.Body.String())
	}
	cookie := setup.Result().Cookies()[0]

	dashboard := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	request.AddCookie(cookie)
	server.ServeHTTP(dashboard, request)
	if dashboard.Code != http.StatusOK {
		t.Fatalf("受保护接口返回 %d：%s", dashboard.Code, dashboard.Body.String())
	}
	refreshed := dashboard.Result().Cookies()
	if len(refreshed) != 1 || refreshed[0].Name != cookie.Name {
		t.Fatalf("受保护接口没有刷新会话 Cookie：%v", refreshed)
	}
	if time.Until(refreshed[0].Expires) < 29*24*time.Hour {
		t.Fatalf("刷新后的会话有效期过短：%s", refreshed[0].Expires)
	}
}
