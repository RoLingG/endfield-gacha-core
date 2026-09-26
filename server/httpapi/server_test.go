package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"endfield-gacha-core/config"
	"endfield-gacha-core/logger"
	"endfield-gacha-core/server"
	"endfield-gacha-core/storage"
)

const testAccessKey = "test-access-key-abcdef"

// newTestServer 构造带临时数据目录的 HTTP 服务与底层 Syncer
func newTestServer(t *testing.T, minTrigger time.Duration) (*Server, *server.Syncer) {
	t.Helper()
	if logger.Log == nil {
		logger.InitLoggerWithOptions(logger.Options{Level: "error", Console: false})
	}
	storage.SetBaseDir(t.TempDir())
	t.Cleanup(func() { storage.SetBaseDir("") })

	syncer := server.NewSyncer(&config.Config{
		UID:                  "948995284",
		ShortToken:           "test-token",
		SyncTimeoutDuration:  time.Minute,
		SyncIntervalDuration: time.Hour,
		MinTriggerDuration:   minTrigger,
	})
	return New("127.0.0.1:0", testAccessKey, syncer), syncer
}

// do 发起一次请求并返回响应
func do(t *testing.T, s *Server, method, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	return rec
}

// TestAuthRejectsMissingHeader 无鉴权头必须 401 —— 接口暴露即等于数据暴露（方案 §7）
func TestAuthRejectsMissingHeader(t *testing.T) {
	s, _ := newTestServer(t, 0)

	for _, path := range []string{"/api/sync/status", "/api/sync"} {
		rec := do(t, s, http.MethodGet, path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 无鉴权应返回 401，实际 %d", path, rec.Code)
		}
	}
}

// TestAuthRejectsWrongKey 错误密钥必须 401
func TestAuthRejectsWrongKey(t *testing.T) {
	s, _ := newTestServer(t, 0)

	for _, key := range []string{"Bearer wrong-key", "wrong-key", "Bearer ", "Bearer " + testAccessKey + "x"} {
		rec := do(t, s, http.MethodGet, "/api/sync/status", key)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("密钥 %q 应返回 401，实际 %d", key, rec.Code)
		}
	}
}

// TestAuthAcceptsValidKey 正确密钥可通过，兼容带/不带 Bearer 前缀
func TestAuthAcceptsValidKey(t *testing.T) {
	s, _ := newTestServer(t, 0)

	for _, key := range []string{"Bearer " + testAccessKey, testAccessKey} {
		rec := do(t, s, http.MethodGet, "/api/sync/status", key)
		if rec.Code != http.StatusOK {
			t.Errorf("密钥 %q 应返回 200，实际 %d", key, rec.Code)
		}
	}
}

// TestSyncTriggerReturnsAccepted 手动触发成功应返回 202（方案 §5.4）
func TestSyncTriggerReturnsAccepted(t *testing.T) {
	s, _ := newTestServer(t, 0)

	rec := do(t, s, http.MethodPost, "/api/sync", "Bearer "+testAccessKey)
	if rec.Code != http.StatusAccepted {
		t.Errorf("触发同步应返回 202，实际 %d，响应体：%s", rec.Code, rec.Body.String())
	}
}

// TestSyncTriggerConflictWhenRunning 已有任务运行时返回 409
func TestSyncTriggerConflictWhenRunning(t *testing.T) {
	s, syncer := newTestServer(t, 0)

	// 首次触发占用缓冲
	if err := syncer.Trigger(); err != nil {
		t.Fatalf("首次触发失败: %v", err)
	}

	rec := do(t, s, http.MethodPost, "/api/sync", "Bearer "+testAccessKey)
	if rec.Code != http.StatusConflict {
		t.Errorf("运行中触发应返回 409，实际 %d", rec.Code)
	}
}

// TestSyncRejectsNonPost 非 POST 方法返回 405 并带 Allow 头
func TestSyncRejectsNonPost(t *testing.T) {
	s, _ := newTestServer(t, 0)

	rec := do(t, s, http.MethodGet, "/api/sync", "Bearer "+testAccessKey)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/sync 应返回 405，实际 %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow 头 = %q, 期望 POST", allow)
	}
}

// TestStatusResponseShape 状态接口需返回方案约定的字段
func TestStatusResponseShape(t *testing.T) {
	s, _ := newTestServer(t, 0)

	rec := do(t, s, http.MethodGet, "/api/sync/status", "Bearer "+testAccessKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态查询应返回 200，实际 %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	for _, key := range []string{"running", "lastStart", "lastEnd", "lastSource", "lastResult", "nextScheduled"} {
		if _, ok := body[key]; !ok {
			t.Errorf("状态响应缺少字段 %q，实际内容：%s", key, rec.Body.String())
		}
	}
}

// TestStatusRejectsNonGet 状态接口仅支持 GET
func TestStatusRejectsNonGet(t *testing.T) {
	s, _ := newTestServer(t, 0)

	rec := do(t, s, http.MethodPost, "/api/sync/status", "Bearer "+testAccessKey)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/sync/status 应返回 405，实际 %d", rec.Code)
	}
}

// TestHealthzNoAuthRequired 健康检查供 Docker healthcheck 使用，不能要求鉴权
func TestHealthzNoAuthRequired(t *testing.T) {
	s, _ := newTestServer(t, 0)

	rec := do(t, s, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz 无需鉴权且应返回 200，实际 %d", rec.Code)
	}
}

// TestHealthzReportsTokenInvalid token 失效时健康检查返回 503，
// 便于监控系统发现需人工更新凭证
func TestHealthzReportsTokenInvalid(t *testing.T) {
	s, syncer := newTestServer(t, 0)

	syncer.RecordTokenFailure(errors.New("短 token 已失效: HTTP 状态码 400"))

	rec := do(t, s, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("token 失效时 /healthz 应返回 503，实际 %d，响应体：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "token") {
		t.Errorf("响应应提示 token 问题，实际：%s", rec.Body.String())
	}
}
