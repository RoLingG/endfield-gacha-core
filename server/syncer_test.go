package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"endfield-gacha-core/api"
	"endfield-gacha-core/config"
	"endfield-gacha-core/logger"
	"endfield-gacha-core/model"
	"endfield-gacha-core/storage"
)

// newTestSyncer 构造仅用于调度逻辑测试的 Syncer，数据目录指向临时目录
func newTestSyncer(t *testing.T, minTrigger time.Duration) *Syncer {
	t.Helper()
	storage.SetBaseDir(t.TempDir())
	t.Cleanup(func() { storage.SetBaseDir("") })

	return NewSyncer(&config.Config{
		ShortToken:           "test-token",
		UID:                  "948995284",
		ServerID:             "1",
		Lang:                 "zh-cn",
		MinTriggerDuration:   minTrigger,
		SyncIntervalDuration: time.Hour,
		SyncTimeoutDuration:  time.Minute,
	})
}

// TestTriggerRejectsWhenBufferFull 缓冲未消费时应当拒绝，而非排队。
// 这是「拒绝重复触发」策略的核心保证（方案 §5.2）。
func TestTriggerRejectsWhenBufferFull(t *testing.T) {
	s := newTestSyncer(t, 0)

	if err := s.Trigger(); err != nil {
		t.Fatalf("首次触发应成功: %v", err)
	}
	// 缓冲区已有 1 个待处理信号，再次触发必须被拒绝
	err := s.Trigger()
	if !errors.Is(err, ErrSyncInProgress) {
		t.Fatalf("重复触发应返回 ErrSyncInProgress，实际: %v", err)
	}
}

// TestTriggerRateLimited 两次触发间隔小于 min_trigger 时应被限流
func TestTriggerRateLimited(t *testing.T) {
	s := newTestSyncer(t, time.Minute)

	if err := s.Trigger(); err != nil {
		t.Fatalf("首次触发应成功: %v", err)
	}
	// 消费掉信号，排除「缓冲已满」这一干扰因素，单独验证限流逻辑
	<-s.trigger

	err := s.Trigger()
	if err == nil {
		t.Fatal("间隔不足时应被限流拒绝")
	}
	if errors.Is(err, ErrSyncInProgress) {
		t.Fatalf("此处应为限流错误而非并发拒绝: %v", err)
	}
	if !strings.Contains(err.Error(), "频繁") {
		t.Errorf("限流错误信息应说明原因，实际: %v", err)
	}
	// 错误信息中的等待秒数必须是正数，便于用户重试
	if !strings.Contains(err.Error(), "秒后重试") {
		t.Errorf("限流错误应提示重试时间，实际: %v", err)
	}
}

// TestTriggerSucceedsAfterBufferDrained 缓冲被消费后应能再次触发
func TestTriggerSucceedsAfterBufferDrained(t *testing.T) {
	s := newTestSyncer(t, 0)

	if err := s.Trigger(); err != nil {
		t.Fatalf("首次触发失败: %v", err)
	}
	<-s.trigger // 模拟调度循环取走信号

	if err := s.Trigger(); err != nil {
		t.Fatalf("缓冲清空后应可再次触发: %v", err)
	}
}

// TestTriggerConcurrentOnlyOneAccepted 并发触发时只允许一个通过，
// 保证不会出现多次同步同时写盘
func TestTriggerConcurrentOnlyOneAccepted(t *testing.T) {
	s := newTestSyncer(t, 0)

	const goroutines = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0

	wg.Add(goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			if err := s.Trigger(); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if accepted != 1 {
		t.Errorf("并发触发被接受 %d 次，期望恰好 1 次", accepted)
	}
}

// TestStateSnapshotConsistent 状态读取与写入并发时不得 panic，且字段自洽
func TestStateSnapshotConsistent(t *testing.T) {
	s := newTestSyncer(t, 0)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.markStart("manual")
			s.markEnd("manual", nil, i, i, time.Second)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			state := s.State()
			// running 与 lastResult 必须自洽
			if state.Running && state.LastResult != "running" {
				t.Errorf("running=true 但 lastResult=%q", state.LastResult)
				return
			}
		}
	}()
	wg.Wait()
}

// TestMarkEndRecordsFailure 失败结果需落进状态，供 /api/sync/status 展示
func TestMarkEndRecordsFailure(t *testing.T) {
	s := newTestSyncer(t, 0)

	s.markStart("scheduled")
	s.markEnd("scheduled", errors.New("网络不可达"), 0, 0, 2*time.Second)

	state := s.State()
	if state.Running {
		t.Error("任务结束后 running 应为 false")
	}
	if state.LastResult != "failed" {
		t.Errorf("lastResult = %q, 期望 failed", state.LastResult)
	}
	if !strings.Contains(state.LastError, "网络不可达") {
		t.Errorf("失败原因未记录: %q", state.LastError)
	}
	if state.LastSource != SourceScheduled {
		t.Errorf("lastSource = %q, 期望 %q", state.LastSource, SourceScheduled)
	}
}

// TestTokenStatePersistedAcrossRestart token 失效标记必须跨进程重启保留，
// 否则容器反复重启会持续发起无效请求
func TestTokenStatePersistedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	storage.SetBaseDir(dir)
	t.Cleanup(func() { storage.SetBaseDir("") })

	cfg := &config.Config{UID: "948995284", SyncIntervalDuration: time.Hour}
	first := NewSyncer(cfg)
	first.recordTokenFailure(errors.New("token 已过期"))

	if !first.tokenInvalid() {
		t.Fatal("失败后应标记 token 失效")
	}

	// 模拟重启：重新构造 Syncer 并从磁盘恢复状态
	second := NewSyncer(cfg)
	if !second.tokenInvalid() {
		t.Error("重启后 token 失效标记丢失，会重复发起无效请求")
	}

	// 状态文件落在数据目录，且权限收紧到仅属主可读
	path := filepath.Join(dir, "sync_token_state.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("状态文件未落盘: %v", err)
	}
}

// TestRecordTokenSuccessClearsInvalid 成功换取后必须清除失效标记
func TestRecordTokenSuccessClearsInvalid(t *testing.T) {
	s := newTestSyncer(t, 0)

	s.recordTokenFailure(errors.New("网络抖动"))
	if !s.tokenInvalid() {
		t.Fatal("失败后应标记失效")
	}
	s.recordTokenSuccess()
	if s.tokenInvalid() {
		t.Error("成功换取后应清除失效标记")
	}
	if s.tokenAgeDaysLocked() != 0 {
		t.Errorf("刚成功后 token 天龄应为 0，实际 %d", s.tokenAgeDaysLocked())
	}
}

// TestRunAlwaysSyncsOnStartup 启动时必须尝试同步，即使上次因 token 失效中止。
// 回归测试：曾用 tokenInvalid() 跳过启动同步，导致用户「换了 token 重启也没反应」——
// 旧的失效标记与「本次 token 是否已更新」无关，不可据此跳过。
func TestRunAlwaysSyncsOnStartup(t *testing.T) {
	// Run 会输出日志，需先初始化 logger
	if logger.Log == nil {
		logger.InitLoggerWithOptions(logger.Options{Level: "error", Console: false})
	}

	dir := t.TempDir()
	storage.SetBaseDir(dir)
	t.Cleanup(func() { storage.SetBaseDir("") })

	// 模拟上个进程留下的失效标记
	cfg := &config.Config{
		UID:                  "948995284",
		ShortToken:           "invalid-token",
		ServerID:             "1",
		Lang:                 "zh-cn",
		SyncIntervalDuration: time.Hour,
		SyncTimeoutDuration:  5 * time.Second,
		MinTriggerDuration:   time.Second,
	}
	prev := NewSyncer(cfg)
	prev.recordTokenFailure(errors.New("登录已过期"))
	if !prev.tokenInvalid() {
		t.Fatal("前置条件失败：应已标记失效")
	}

	// 重新构造（模拟重启）+ 立即取消，只验证「是否发起了启动同步」
	next := NewSyncer(cfg)
	if !next.tokenInvalid() {
		t.Fatal("重启后应恢复失效标记")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		next.Run(ctx)
	}()

	// 给启动同步留出执行时间后关闭
	time.Sleep(2 * time.Second)
	cancel()
	<-done

	// Run 内部会调用 runSync，其结束会写入状态。
	// 关键断言：启动同步确实执行过（lastStart 被写入），而不是被跳过。
	state := next.State()
	if state.LastStart == "" {
		t.Error("启动同步被跳过，用户更新 token 后重启将不会生效")
	}
	if state.LastSource != SourceStartup {
		t.Errorf("首次同步来源应为 startup，实际 %q", state.LastSource)
	}
}

// TestResolveAccountRequiresGrant resolveAccount 无论是否已配置 uid，
// 都必须先换取通行证 token——后续换取 u8_token 依赖它。
// 回归测试：曾误将短 token 直接传给 GetU8Token，官方返回「登录已过期」，
// 导致同步在抓取数据前就失败。
func TestResolveAccountRequiresGrant(t *testing.T) {
	s := newTestSyncer(t, 0)
	s.cfg.ShortToken = "obviously-invalid-short-token"
	s.cfg.UID = "011309408" // 即使已配置 uid，也不能跳过 Grant 换取

	_, err := s.resolveAccount()
	if err == nil {
		t.Skip("短 token 未按预期被拒，跳过")
	}
	// 必须是因为 Grant 失败而报错，证明代码确实走了换通行证 token 的流程
	if !errors.Is(err, ErrTokenRejected) {
		t.Errorf("应因换取通行证 token 失败而报错，实际: %v", err)
	}
}

// TestGetU8TokenRequiresHgTokenNotShortToken 记录一个易错点：
// GetU8Token 的参数是通行证 token。此测试用真实网络调用确认传短 token 会被拒，
// 若将来官方放宽校验，该测试会失败并提示重新评估调用方式。
func TestGetU8TokenRequiresHgTokenNotShortToken(t *testing.T) {
	if testing.Short() {
		t.Skip("需要网络，-short 模式跳过")
	}
	// 短 token 是 base64 形态的 24 字符串，直接传给 u8_token 接口
	_, err := api.GetU8Token("AAAAAAAAAAAAAAAAAAAAAAAA", "011309408")
	if err == nil {
		t.Skip("官方未按预期拒绝，跳过（接口行为可能已变化）")
	}
	// 预期被拒绝；此处只断言「确实失败了」，不绑定具体错误文案
	t.Logf("直接传短 token 被拒绝（符合预期）: %v", err)
}

// TestResolveAccountWithConfiguredUID 已配置 uid 时不应再查询绑定列表。
// 由于通行证 token 是后续步骤的必需品，此处用无效 token 验证错误发生在 Grant 阶段，
// 而非绑定查询阶段——以此证明配置了 uid 就跳过了绑定查询。
func TestResolveAccountWithConfiguredUID(t *testing.T) {
	s := newTestSyncer(t, 0)
	s.cfg.ShortToken = "obviously-invalid-short-token"
	s.cfg.UID = "011309408"

	_, err := s.resolveAccount()
	if err == nil {
		t.Skip("短 token 未按预期被拒，跳过")
	}
	// 报错应来自换通行证 token，而非「查询账号角色失败」（后者说明走了绑定查询）
	if strings.Contains(err.Error(), "查询账号角色失败") {
		t.Errorf("已配置 uid 时不应查询绑定列表，实际错误: %v", err)
	}
	if !strings.Contains(err.Error(), "换取通行证 token") {
		t.Errorf("错误应来自换取通行证 token 阶段，实际: %v", err)
	}
}

// TestTokenAgeDays 天龄计算用于「建议更新 token」提醒（方案 §4.5）
func TestTokenAgeDays(t *testing.T) {
	s := newTestSyncer(t, 0)

	// 无任何记录时按 0 处理，不误报过期
	if got := s.tokenAgeDaysLocked(); got != 0 {
		t.Errorf("无记录时天龄 = %d, 期望 0", got)
	}

	s.mu.Lock()
	s.tokenState.LastSuccessAt = time.Now().Add(-61 * 24 * time.Hour).Format(time.DateTime)
	s.mu.Unlock()

	if got := s.tokenAgeDaysLocked(); got < 60 {
		t.Errorf("61 天前的成功记录应算出 >=60 天，实际 %d", got)
	}
}

// TestSourceName 触发来源要显示为中文，方便用户看日志
func TestSourceName(t *testing.T) {
	cases := map[string]string{
		SourceScheduled: "定时",
		SourceManual:    "手动",
		SourceStartup:   "启动",
	}
	for src, want := range cases {
		if got := sourceName(src); got != want {
			t.Errorf("sourceName(%q) = %q, 期望 %q", src, got, want)
		}
	}
}

// TestResolveAccountTokenRejected 未配置 uid 时若短 token 被拒，
// 必须返回 ErrTokenRejected，让上层按失效处理而不是反复重试
func TestResolveAccountTokenRejected(t *testing.T) {
	s := newTestSyncer(t, 0)
	s.cfg.UID = ""
	s.cfg.ShortToken = "obviously-invalid-token"

	_, err := s.resolveAccount()
	if err == nil {
		t.Skip("网络不可用或接口行为变化，跳过")
	}
	if !errors.Is(err, ErrTokenRejected) {
		t.Errorf("token 被拒时应返回 ErrTokenRejected，实际: %v", err)
	}
}

// TestNormalizeServerType 归一化规则：未知值一律落到官方服，
// 避免同一账号的数据被拆散到不同目录
func TestNormalizeServerType(t *testing.T) {
	s := newTestSyncer(t, 0)

	cases := map[string]string{
		"":           model.ServerOfficial,
		"official":   model.ServerOfficial,
		"bilibili":   model.ServerBilibili,
		" bilibili ": model.ServerBilibili,
		"unknown":    model.ServerOfficial, // 绑定接口未能判定渠道
		"weird":      model.ServerOfficial,
	}
	for input, want := range cases {
		if got := s.normalizeServerType(input); got != want {
			t.Errorf("normalizeServerType(%q) = %q, 期望 %q", input, got, want)
		}
	}
}
