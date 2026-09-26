// Package server 提供服务端的同步调度：定时抓取 + 手动触发 + 状态可观测。
//
// 设计要点见 docs/backend-plan.md 第五节：
//   - 定时器与手动触发汇入同一执行路径，避免并发写盘
//   - 用 cap=1 的 channel + select default 实现「运行中拒绝重复触发」
//   - context 只负责取消（优雅关闭 / 单次超时），不负责触发
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"endfield-gacha-core/api"
	"endfield-gacha-core/config"
	"endfield-gacha-core/logger"
	"endfield-gacha-core/model"
	"endfield-gacha-core/storage"

	"go.uber.org/zap"
)

// ErrUnauthorizedToken 短 token 失效（换取 u8_token 失败）。
// 该错误不应重试，需提示用户重新获取 token。
var ErrUnauthorizedToken = errors.New("短 token 已失效")

// ErrTokenRejected 查询账号时短 token 被官方拒绝，是 token 失效的一种表现
var ErrTokenRejected = errors.New("短 token 被拒绝")

// ErrSyncInProgress 已有同步任务执行中
var ErrSyncInProgress = errors.New("已有同步任务运行中")

// 触发来源，仅用于日志与状态展示
const (
	SourceScheduled = "scheduled"
	SourceManual    = "manual"
	SourceStartup   = "startup"
)

// SyncState 同步任务状态，对应 GET /api/sync/status 的响应体
type SyncState struct {
	Running       bool   `json:"running"`
	LastStart     string `json:"lastStart"`
	LastEnd       string `json:"lastEnd"`
	LastSource    string `json:"lastSource"`
	LastResult    string `json:"lastResult"`
	LastError     string `json:"lastError,omitempty"`
	LastDuration  string `json:"lastDuration,omitempty"`
	LastCharCount int    `json:"lastCharCount"`
	LastWpnCount  int    `json:"lastWeaponCount"`
	NextScheduled string `json:"nextScheduled"`
	// TokenAgeDays 距上次成功同步的天数，用于判断 token 是否临近过期
	TokenAgeDays int `json:"tokenAgeDays"`
}

// Syncer 编排一次完整同步：短 token → u8_token → 抓取 → 合并落盘
type Syncer struct {
	cfg *config.Config

	mu    sync.Mutex
	state SyncState

	// trigger 缓冲容量 1，是并发控制的关键：
	// 写入成功说明当前无待处理任务，写入失败（缓冲已满）说明上一轮尚未消费，直接拒绝
	trigger chan struct{}
	ticker  *time.Ticker

	lastTrigger time.Time
	// tokenState 记录 token 相关的时间与失败原因，跨重启持久化到数据目录
	tokenState tokenState
}

// NewSyncer 创建同步器并加载持久化的 token 状态
func NewSyncer(cfg *config.Config) *Syncer {
	s := &Syncer{
		cfg:     cfg,
		trigger: make(chan struct{}, 1),
	}
	s.tokenState = loadTokenState()
	return s
}

// State 返回当前状态快照（并发安全）
func (s *Syncer) State() SyncState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	state.NextScheduled = s.nextScheduledLocked()
	state.TokenAgeDays = s.tokenAgeDaysLocked()
	return state
}

// RecordTokenFailure 供同包外的测试与诊断入口标记 token 失效。
// 生产路径一律通过 doSync 中的换取结果触发，调用方不应手动使用。
func (s *Syncer) RecordTokenFailure(err error) {
	s.recordTokenFailure(err)
}

// TokenInvalid 报告短 token 当前是否处于失效状态（持久化标记，跨重启保留）
func (s *Syncer) TokenInvalid() bool {
	return s.tokenInvalid()
}

// Trigger 手动触发一次同步。已有任务运行或被限流时返回错误，由调用方映射为 HTTP 状态码。
func (s *Syncer) Trigger() error {
	s.mu.Lock()
	if time.Since(s.lastTrigger) < s.cfg.MinTriggerDuration {
		wait := s.cfg.MinTriggerDuration - time.Since(s.lastTrigger)
		s.mu.Unlock()
		return fmt.Errorf("触发过于频繁，请 %d 秒后重试", int(wait.Seconds())+1)
	}
	s.mu.Unlock()

	select {
	case s.trigger <- struct{}{}:
		s.mu.Lock()
		s.lastTrigger = time.Now()
		s.mu.Unlock()
		return nil
	default:
		return ErrSyncInProgress
	}
}

// Run 启动调度循环，阻塞直到 ctx 被取消（服务关闭）
func (s *Syncer) Run(ctx context.Context) {
	s.ticker = time.NewTicker(s.cfg.SyncIntervalDuration)
	defer s.ticker.Stop()

	logger.Log.Info("同步调度已启动",
		zap.String("间隔", s.cfg.SyncIntervalDuration.String()),
		zap.String("下次执行", time.Now().Add(s.cfg.SyncIntervalDuration).Format(time.DateTime)))

	// 启动即同步一次：进程重启后无需等待一个完整周期。
	//
	// 此处不检查上次是否因 token 失效而中止：用户更新 token 后重启正是最常见的情形，
	// 若沿用旧的失效标记就会跳过同步，导致「换了 token 也没反应」。
	// token 仍然失效时，同步会在换取阶段失败并输出提示，代价仅一次请求。
	if s.tokenInvalid() {
		logger.Log.Info("检测到上次同步因 token 失效中止，本次启动将重新尝试（若已更新 token 即可恢复正常）")
	}
	s.runSync(ctx, SourceStartup)

	for {
		select {
		case <-s.ticker.C:
			s.runSync(ctx, SourceScheduled)
		case <-s.trigger:
			s.runSync(ctx, SourceManual)
		case <-ctx.Done():
			logger.Log.Info("收到关闭信号，同步调度退出")
			return
		}
	}
}

// runSync 执行一次同步。每次派生带超时的 context，防止任务卡死。
func (s *Syncer) runSync(parent context.Context, source string) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.SyncTimeoutDuration)
	defer cancel()

	start := time.Now()
	s.markStart(source)
	logger.Log.Info("开始同步", zap.String("来源", sourceName(source)))

	charCount, wpnCount, err := s.doSync(ctx)

	duration := time.Since(start)
	s.markEnd(source, err, charCount, wpnCount, duration)

	switch {
	case err == nil:
		logger.Log.Info("同步完成",
			zap.String("来源", sourceName(source)),
			zap.Int("角色记录", charCount),
			zap.Int("武器记录", wpnCount),
			zap.Duration("耗时", duration))
	case errors.Is(err, ErrUnauthorizedToken):
		// token 失效：醒目报错，不重试（重试只会产生无效请求）
		logger.Log.Error("=====================================================")
		logger.Log.Error("短 token 已失效，同步中止，需要重新获取 token！",
			zap.String("原因", err.Error()))
		logger.Log.Error("处理方式：浏览器登录终末地官网 → F12 → Local Storage 复制新的 token")
		logger.Log.Error("→ 更新 config.yaml 的 short_token 或环境变量 SHORT_TOKEN → 重启容器")
		logger.Log.Error("=====================================================")
	default:
		logger.Log.Error("同步失败",
			zap.String("来源", sourceName(source)),
			zap.Duration("耗时", duration),
			zap.Error(err))
	}
}

// doSync 实际的抓取与落盘流程
func (s *Syncer) doSync(ctx context.Context) (int, int, error) {
	// 1. 先确定目标账号：u8_token 的换取需要 uid，
	//    而 uid 可能要靠短 token 自动解析出来，故这步必须在前
	target, err := s.resolveAccount()
	if err != nil {
		// token 无效会导致解析失败，此时同样按失效处理（可避免反复无效请求）
		if errors.Is(err, ErrTokenRejected) {
			s.recordTokenFailure(err)
			return 0, 0, fmt.Errorf("%w: %v", ErrUnauthorizedToken, err)
		}
		return 0, 0, err
	}

	// 2. 用通行证 token 换取 u8_token（有效期短，每次同步都重新换取）。
	//    注意此处必须传 HgToken 而非短 token —— 接口要求的是通行证 token，
	//    直接传短 token 会被官方判为「登录已过期」。
	u8Token, err := api.GetU8Token(target.HgToken, target.UID)
	if err != nil {
		// 换取失败即判定 token 过期；网络抖动由用户重试，不做盲目重试
		s.recordTokenFailure(err)
		return 0, 0, fmt.Errorf("%w: %v", ErrUnauthorizedToken, err)
	}
	s.recordTokenSuccess()
	s.warnIfTokenAging()
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}

	// 3. 读取本地已落盘的 seqId，用于增量早停（避免每次都全量翻页）
	knownChar := storage.LoadKnownSeqIDs[model.EndFieldCharInfo](target.UID, target.ServerType, model.PoolTypeChar)
	knownWpn := storage.LoadKnownSeqIDs[model.EndFieldWeaponInfo](target.UID, target.ServerType, model.PoolTypeWeapon)

	// 4. 抓取角色池与武器池
	charData, err := api.FetchCharDataAll(ctx, u8Token, s.cfg.ServerID, s.cfg.Lang, knownChar)
	if err != nil {
		return 0, 0, fmt.Errorf("抓取角色池失败：%w", err)
	}
	weaponData, err := api.FetchWeaponDataAll(ctx, u8Token, s.cfg.ServerID, s.cfg.Lang, knownWpn)
	if err != nil {
		return 0, 0, fmt.Errorf("抓取武器池失败：%w", err)
	}

	// 5. 合并落盘（内部去重 + 排序 + .tmp/rename 原子写）
	charSaved, err := storage.MergeAndSaveData(charData, target.UID, target.ServerType, model.PoolTypeChar)
	if err != nil {
		return 0, 0, fmt.Errorf("保存角色数据失败：%w", err)
	}
	wpnSaved, err := storage.MergeAndSaveData(weaponData, target.UID, target.ServerType, model.PoolTypeWeapon)
	if err != nil {
		return 0, 0, fmt.Errorf("保存武器数据失败：%w", err)
	}

	// 6. 卡池详情补充（供桌面版展示 UP 信息，失败不影响主链路）
	s.refreshPoolContent(ctx, charData, weaponData)

	return len(charSaved), len(wpnSaved), nil
}

// account 一次同步的目标账号
type account struct {
	UID        string
	ServerType string
	// HgToken 通行证 token，由短 token 换取而来。
	// 换取 u8_token 必须用它，不能直接用短 token（那会被官方判为「登录已过期」）。
	// 配置了 uid 时无需查询绑定，此处仍会换取，因为后续步骤依赖它。
	HgToken string
}

// resolveAccount 确定本次同步要抓哪个角色，并换取本轮的通行证 token。
//
// 一次同步只换取一次通行证 token，用户名下的角色通过它查询。
// 一个短 token 对应一个角色，因此取绑定列表的第一项即可。
func (s *Syncer) resolveAccount() (account, error) {
	// 短 token → 通行证 token（后续所有接口都基于它，故无论是否查绑定都必须先换取）
	hgToken, err := api.GetGrantToken(s.cfg.ShortToken)
	if err != nil {
		// 换取失败通常意味着短 token 已被拒绝（如过期），
		// 包上 ErrTokenRejected 让上层按失效处理，避免反复无效请求
		return account{}, fmt.Errorf("%w：换取通行证 token 失败：%v", ErrTokenRejected, err)
	}

	// 已配置 uid 时无需查询绑定，直接采用
	if uid := strings.TrimSpace(s.cfg.UID); uid != "" {
		return account{
			UID:        uid,
			ServerType: s.normalizeServerType(s.cfg.ServerType),
			HgToken:    hgToken,
		}, nil
	}

	players, err := api.GetPlayerBindings(hgToken)
	if err != nil {
		return account{}, fmt.Errorf("查询账号角色失败：%w", err)
	}
	// 防配置填错账号：绑定列表为空说明该 token 名下没有终末地角色
	if len(players) == 0 {
		return account{}, fmt.Errorf("该账号下未找到终末地角色，请确认短 token 对应的账号是否正确")
	}

	p := players[0]
	logger.Log.Info("已自动识别账号角色",
		zap.String("昵称", p.NickName),
		zap.String("uid", config.RedactedUID(p.Uid)),
		zap.String("区服", p.ServerType))
	return account{
		UID:        p.Uid,
		ServerType: s.normalizeServerType(p.ServerType),
		HgToken:    hgToken,
	}, nil
}

// normalizeServerType 归一化落盘用的区服标识，空值或未知值按官方服处理
func (s *Syncer) normalizeServerType(serverType string) string {
	switch strings.TrimSpace(serverType) {
	case model.ServerBilibili:
		return model.ServerBilibili
	case model.ServerOfficial:
		return model.ServerOfficial
	default:
		// 含 "unknown"：绑定接口未能判定渠道时，落盘按官方服目录走，
		// 避免同一账号的数据被拆到两个目录
		if serverType != "" && serverType != "unknown" && logger.Log != nil {
			logger.Log.Warn("未知的 server_type，按官方服处理",
				zap.String("server_type", serverType))
		}
		return model.ServerOfficial
	}
}

// refreshPoolContent 拉取本次涉及卡池的详情并落盘。
// 卡池详情属展示增强数据，失败仅告警，不影响抽卡记录同步结果。
func (s *Syncer) refreshPoolContent(ctx context.Context, charData []model.EndFieldCharInfo, weaponData []model.EndFieldWeaponInfo) {
	charPools := make(map[string]struct{})
	for _, item := range charData {
		if item.PoolID != "" {
			charPools[item.PoolID] = struct{}{}
		}
	}
	wpnPools := make(map[string]struct{})
	for _, item := range weaponData {
		if item.PoolID != "" {
			wpnPools[item.PoolID] = struct{}{}
		}
	}
	if len(charPools) == 0 && len(wpnPools) == 0 {
		return
	}

	var charConfigs []model.PoolConfig
	for poolID := range charPools {
		if ctx.Err() != nil {
			return
		}
		cfg, err := s.fetchPoolConfig(ctx, poolID)
		if err != nil {
			logger.Log.Debug("角色池详情获取失败，跳过", zap.String("pool_id", poolID), zap.Error(err))
			continue
		}
		charConfigs = append(charConfigs, *cfg)
	}
	if len(charConfigs) > 0 {
		if _, err := storage.SavePoolConfig(model.PoolConfigList{CharPools: charConfigs}, false); err != nil {
			logger.Log.Warn("角色池配置落盘失败", zap.Error(err))
		}
	}

	var wpnConfigs []model.PoolConfig
	for poolID := range wpnPools {
		if ctx.Err() != nil {
			return
		}
		cfg, err := s.fetchPoolConfig(ctx, poolID)
		if err != nil {
			logger.Log.Debug("武器池详情获取失败，跳过", zap.String("pool_id", poolID), zap.Error(err))
			continue
		}
		wpnConfigs = append(wpnConfigs, *cfg)
	}
	if len(wpnConfigs) > 0 {
		if _, err := storage.SavePoolConfig(model.PoolConfigList{WeaponPools: wpnConfigs}, true); err != nil {
			logger.Log.Warn("武器池配置落盘失败", zap.Error(err))
		}
	}
}

// fetchPoolConfig 获取单个卡池详情并转换为配置结构
func (s *Syncer) fetchPoolConfig(ctx context.Context, poolID string) (*model.PoolConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resp, err := api.FetchPoolContent(poolID, s.cfg.ServerID, s.cfg.Lang)
	if err != nil {
		// 官方明确返回「池不存在」才按死池剔除；网络超时等临时错误不剔除，防止误杀活池
		if errors.Is(err, api.ErrPoolNotFound) {
			isWeapon := strings.Contains(poolID, "wpn_")
			if rmErr := storage.RemoveDiscoveredPoolIDs([]string{poolID}, isWeapon); rmErr != nil {
				logger.Log.Warn("剔除死池 ID 失败", zap.String("pool_id", poolID), zap.Error(rmErr))
			}
		}
		return nil, err
	}

	pool := resp.Data.Pool
	return &model.PoolConfig{
		PoolID:     poolID,
		PoolName:   pool.PoolName,
		PoolType:   pool.PoolType,
		Up6Name:    pool.Up6Name,
		Up6CharID:  matchIDByName(pool.All, pool.Up6Name),
		GachaType:  pool.PoolGachaType,
		LastUpdate: time.Now().Format(time.DateTime),
	}, nil
}

// matchIDByName 在卡池物品清单中按名称查 ID，供前端跳转图鉴使用
func matchIDByName(items []struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Rarity int    `json:"rarity"`
}, name string) string {
	if name == "" {
		return ""
	}
	for _, it := range items {
		if it.Name == name {
			return it.ID
		}
	}
	return ""
}
