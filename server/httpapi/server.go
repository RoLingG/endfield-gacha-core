// Package httpapi 提供服务端对外 HTTP 接口。
//
// 接口清单：
//
//	POST /api/sync         手动触发一次同步（需鉴权）
//	GET  /api/sync/status  查询同步任务状态（需鉴权）
//	GET  /healthz          健康检查（无需鉴权，供 Docker healthcheck 使用）
//
// 安全约束见 docs/backend-plan.md 第七节：接口一旦暴露端口即存在风险，
// 除健康检查外全部要求 Bearer 鉴权；鉴权使用常量时间比较，避免时序侧信道。
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"endfield-gacha-core/logger"
	"endfield-gacha-core/server"

	"go.uber.org/zap"
)

// Server HTTP 接口服务
type Server struct {
	syncer    *server.Syncer
	accessKey string
	httpSrv   *http.Server
}

// New 创建 HTTP 服务。addr 为空时由调用方决定不启动。
func New(addr, accessKey string, syncer *server.Syncer) *Server {
	s := &Server{syncer: syncer, accessKey: accessKey}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/sync", s.auth(s.handleSync))
	mux.HandleFunc("/api/sync/status", s.auth(s.handleStatus))

	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: logRequests(mux),
		// 服务端仅处理极少量请求，收紧超时避免连接被长期占用
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// Start 在后台启动监听，就绪后返回。启动失败会记录日志但不终止服务。
func (s *Server) Start() {
	go func() {
		logger.Log.Info("HTTP 接口已启动", zap.String("地址", s.httpSrv.Addr))
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Log.Error("HTTP 服务异常退出，同步调度仍在运行", zap.Error(err))
		}
	}()
}

// Shutdown 优雅关闭 HTTP 服务
func (s *Server) Shutdown() error {
	return s.httpSrv.Close()
}

// auth 鉴权中间件：校验 Authorization: Bearer <access_key>
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			// 兼容部分客户端不带 Bearer 前缀的写法
			token = strings.TrimSpace(header)
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.accessKey)) != 1 {
			logger.Log.Warn("接口鉴权失败",
				zap.String("remote", r.RemoteAddr),
				zap.String("path", r.URL.Path))
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "鉴权失败：请在 Authorization 头中提供正确的 Bearer 密钥",
			})
			return
		}
		next(w, r)
	}
}

// handleSync POST /api/sync —— 手动触发一次同步
//
// 状态码约定（见方案 §5.4）：
//
//	202 已开始同步
//	401 鉴权失败
//	405 使用了非 POST 方法
//	409 已有同步任务运行中，或触发过于频繁
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "该接口仅支持 POST",
		})
		return
	}

	if err := s.syncer.Trigger(); err != nil {
		status := http.StatusConflict
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"message": "已开始同步，可轮询 GET /api/sync/status 查看进度",
	})
}

// handleStatus GET /api/sync/status —— 查询同步任务状态
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "该接口仅支持 GET",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.syncer.State())
}

// handleHealthz GET /healthz —— 健康检查，无需鉴权
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	state := s.syncer.State()
	status := http.StatusOK
	body := map[string]any{
		"status":  "ok",
		"running": state.Running,
	}
	// token 失效属于需人工介入的故障，健康检查返回 503 便于监控发现
	if s.syncer.TokenInvalid() {
		status = http.StatusServiceUnavailable
		body["status"] = "token_invalid"
		body["hint"] = "短 token 可能已失效，请更新配置中的 short_token"
	}
	writeJSON(w, status, body)
}

// logRequests 记录访问日志，便于排查「谁在什么时候触发了同步」
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Log.Debug("HTTP 请求",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("remote", r.RemoteAddr),
			zap.Duration("耗时", time.Since(start)))
	})
}

// writeJSON 统一 JSON 响应输出
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(body); err != nil {
		logger.Log.Debug("响应写入失败", zap.Error(err))
	}
}
