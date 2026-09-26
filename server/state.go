package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"endfield-gacha-core/config"
	"endfield-gacha-core/logger"
	"endfield-gacha-core/storage"

	"go.uber.org/zap"
)

// tokenStateFile 记录 token 健康度，跨容器重启保留。
// 放在数据目录而非配置文件目录：它属于运行状态，不应被用户手写覆盖。
const tokenStateFile = "sync_token_state.json"

// tokenState token 换取结果的历史记录
type tokenState struct {
	// LastSuccessAt 最近一次成功换取 u8_token 的时间
	LastSuccessAt string `json:"lastSuccessAt"`
	// LastFailureAt 最近一次换取失败的时间
	LastFailureAt string `json:"lastFailureAt"`
	// LastError 最近一次失败原因
	LastError string `json:"lastError"`
	// Invalid 上次是否因 token 失效而中止（为 true 时跳过启动同步，避免无谓请求）
	Invalid bool `json:"invalid"`
}

// loadTokenState 读取持久化的 token 状态，文件缺失或损坏时返回零值
func loadTokenState() tokenState {
	var st tokenState
	path, err := tokenStatePath()
	if err != nil {
		return st
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		logger.Log.Warn("token 状态文件损坏，按全新状态处理", zap.Error(err))
		return tokenState{}
	}
	return st
}

// save 持久化 token 状态。写失败只告警：状态仅用于优化启动行为，不影响主链路。
func (t *tokenState) save() {
	path, err := tokenStatePath()
	if err != nil {
		return
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		logger.Log.Debug("写入 token 状态失败", zap.Error(err))
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

// tokenStatePath 返回状态文件路径，确保数据目录已就绪
func tokenStatePath() (string, error) {
	dir, err := storage.GetStorageDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tokenStateFile), nil
}

// recordTokenSuccess 记录一次成功换取，清零失效标记
func (s *Syncer) recordTokenSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenState.LastSuccessAt = time.Now().Format(time.DateTime)
	s.tokenState.Invalid = false
	s.tokenState.LastError = ""
	s.tokenState.save()
}

// recordTokenFailure 记录一次换取失败，标记 token 失效
func (s *Syncer) recordTokenFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenState.LastFailureAt = time.Now().Format(time.DateTime)
	s.tokenState.Invalid = true
	s.tokenState.LastError = err.Error()
	s.tokenState.save()
}

// tokenInvalid 上次同步是否因 token 失效而中止
func (s *Syncer) tokenInvalid() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenState.Invalid
}

// tokenAgeDaysLocked 距上次成功同步的天数，无记录时返回 0
func (s *Syncer) tokenAgeDaysLocked() int {
	if s.tokenState.LastSuccessAt == "" {
		return 0
	}
	t, err := time.ParseInLocation(time.DateTime, s.tokenState.LastSuccessAt, time.Local)
	if err != nil {
		return 0
	}
	return int(time.Since(t).Hours() / 24)
}

// warnIfTokenAging 距上次成功同步超过阈值时提示更新 token（方案 §4.5）。
// 短 token 有效期 1~3 个月，提前提醒可避免用户在不知情的情况下中断同步。
func (s *Syncer) warnIfTokenAging() {
	s.mu.Lock()
	days := s.tokenAgeDaysLocked()
	s.mu.Unlock()

	if days*24 < int(config.TokenWarnAfter.Hours()) {
		return
	}
	logger.Log.Warn("提醒：当前短 token 已使用较长时间，建议提前更新",
		zap.Int("已使用天数", days),
		zap.String("建议", "短 token 有效期约 1~3 个月，请及时在配置中更换，避免同步中断"))
}

// markStart 记录任务开始
func (s *Syncer) markStart(source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Running = true
	s.state.LastStart = time.Now().Format(time.DateTime)
	s.state.LastSource = source
	s.state.LastResult = "running"
	s.state.LastError = ""
}

// markEnd 记录任务结束结果
func (s *Syncer) markEnd(source string, err error, charCount, wpnCount int, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Running = false
	s.state.LastEnd = time.Now().Format(time.DateTime)
	s.state.LastSource = source
	s.state.LastDuration = duration.Round(time.Millisecond).String()
	s.state.LastCharCount = charCount
	s.state.LastWpnCount = wpnCount
	if err == nil {
		s.state.LastResult = "success"
		s.state.LastError = ""
		return
	}
	s.state.LastResult = "failed"
	s.state.LastError = err.Error()
}

// nextScheduledLocked 计算下次定时执行时间
func (s *Syncer) nextScheduledLocked() string {
	if s.ticker == nil {
		return ""
	}
	next := s.state.LastStart
	if next == "" {
		return time.Now().Add(s.cfg.SyncIntervalDuration).Format(time.DateTime)
	}
	last, err := time.ParseInLocation(time.DateTime, next, time.Local)
	if err != nil {
		return time.Now().Add(s.cfg.SyncIntervalDuration).Format(time.DateTime)
	}
	candidate := last.Add(s.cfg.SyncIntervalDuration)
	if candidate.Before(time.Now()) {
		return time.Now().Add(s.cfg.SyncIntervalDuration).Format(time.DateTime)
	}
	return candidate.Format(time.DateTime)
}

// sourceName 触发来源的中文展示名
func sourceName(source string) string {
	switch source {
	case SourceScheduled:
		return "定时"
	case SourceManual:
		return "手动"
	case SourceStartup:
		return "启动"
	default:
		return source
	}
}
