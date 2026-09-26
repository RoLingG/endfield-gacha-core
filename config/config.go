// Package config 负责服务端运行配置的加载、环境变量覆盖与校验。
//
// 查找顺序（先命中者生效）：
//  1. 命令行参数 -config /path/to/config.yaml
//  2. 环境变量 CONFIG_PATH
//  3. 程序同级的 config.yaml
//
// 环境变量（SHORT_TOKEN / UID 等）优先级高于配置文件，
// 便于在 docker-compose 中通过 .env 注入凭证而不落盘。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 默认值
const (
	DefaultSyncInterval = 6 * time.Hour
	DefaultLang         = "zh-cn"
	DefaultLogLevel     = "info"
	DefaultDataDir      = "./data"
	DefaultSyncTimeout  = 5 * time.Minute
	DefaultMinTrigger   = 60 * time.Second
	// TokenWarnAfter 距上次成功同步超过该时长即提示更新 token（短 token 有效期 1~3 个月）
	TokenWarnAfter = 60 * 24 * time.Hour
)

// Config 服务端完整配置
type Config struct {
	// 凭证（必填）
	ShortToken string `yaml:"short_token"` // 官网登录后从 localStorage 获取，有效期 1~3 个月
	UID        string `yaml:"uid"`         // 目标账号 uid
	ServerID   string `yaml:"server_id"`   // 服务器区服，官方服通常是 "1"
	ServerType string `yaml:"server_type"` // 数据落盘按官方/渠道服分目录，留空按官方处理

	// 同步设置
	SyncInterval string `yaml:"sync_interval"` // 支持 30m / 6h / 24h
	DataDir      string `yaml:"data_dir"`      // 数据落盘目录
	SyncTimeout  string `yaml:"sync_timeout"`  // 单次同步超时，留空取 5m

	// 可选
	Lang     string `yaml:"lang"`      // 接口语言，默认 zh-cn
	LogLevel string `yaml:"log_level"` // 目前仅影响启动横幅展示

	// HTTP 接口（阶段二）
	HTTPAddr   string `yaml:"http_addr"`   // 留空则不启动 HTTP 服务
	AccessKey  string `yaml:"access_key"`  // 接口鉴权密钥，启用 HTTP 时必填
	MinTrigger string `yaml:"min_trigger"` // 手动触发最小间隔，默认 60s

	// 运行时解析结果（非配置文件字段）
	SyncIntervalDuration time.Duration `yaml:"-"`
	SyncTimeoutDuration  time.Duration `yaml:"-"`
	MinTriggerDuration   time.Duration `yaml:"-"`
	ConfigPath           string        `yaml:"-"`
}

// Path 返回实际使用的配置文件路径
func Path() string {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-config" || arg == "--config" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		// 支持 -config=/path 形式
		if v, ok := strings.CutPrefix(arg, "-config="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(arg, "--config="); ok {
			return v
		}
	}
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		return p
	}
	return "config.yaml"
}

// Load 读取配置文件、应用环境变量覆盖、填充默认值并校验
func Load() (*Config, error) {
	path := Path()

	cfg := &Config{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("配置文件 %s 解析失败：%w\n请检查 YAML 缩进与引号是否配对", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 允许纯环境变量启动（Docker 场景），缺必填项时再由校验统一报错
		fmt.Fprintf(os.Stderr, "提示：未找到配置文件 %s，将仅使用环境变量启动\n", path)
	default:
		return nil, fmt.Errorf("读取配置文件 %s 失败：%w", path, err)
	}
	cfg.ConfigPath = path

	applyEnvOverrides(cfg)
	applyDefaults(cfg)
	if err := resolveDurations(cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnvOverrides 环境变量覆盖，便于 Docker 场景注入凭证
func applyEnvOverrides(cfg *Config) {
	overrides := []struct {
		key    string
		target *string
	}{
		{"SHORT_TOKEN", &cfg.ShortToken},
		{"UID", &cfg.UID},
		{"SERVER_ID", &cfg.ServerID},
		{"SERVER_TYPE", &cfg.ServerType},
		{"SYNC_INTERVAL", &cfg.SyncInterval},
		{"SYNC_TIMEOUT", &cfg.SyncTimeout},
		{"DATA_DIR", &cfg.DataDir},
		{"LANG", &cfg.Lang},
		{"LOG_LEVEL", &cfg.LogLevel},
		{"HTTP_ADDR", &cfg.HTTPAddr},
		{"ACCESS_KEY", &cfg.AccessKey},
		{"MIN_TRIGGER", &cfg.MinTrigger},
	}
	for _, o := range overrides {
		if v := strings.TrimSpace(os.Getenv(o.key)); v != "" {
			*o.target = v
		}
	}
}

// applyDefaults 填充未配置项的默认值
func applyDefaults(cfg *Config) {
	if strings.TrimSpace(cfg.SyncInterval) == "" {
		cfg.SyncInterval = DefaultSyncInterval.String()
	}
	if strings.TrimSpace(cfg.SyncTimeout) == "" {
		cfg.SyncTimeout = DefaultSyncTimeout.String()
	}
	if strings.TrimSpace(cfg.MinTrigger) == "" {
		cfg.MinTrigger = DefaultMinTrigger.String()
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		cfg.DataDir = DefaultDataDir
	}
	if strings.TrimSpace(cfg.Lang) == "" {
		cfg.Lang = DefaultLang
	}
	if strings.TrimSpace(cfg.LogLevel) == "" {
		cfg.LogLevel = DefaultLogLevel
	}
	if strings.TrimSpace(cfg.ServerID) == "" {
		cfg.ServerID = "1"
	}
	cfg.ShortToken = strings.TrimSpace(cfg.ShortToken)
	cfg.UID = strings.TrimSpace(cfg.UID)
}

// resolveDurations 解析时长类配置，格式错误时给出面向非技术用户的提示
func resolveDurations(cfg *Config) error {
	type durField struct {
		name  string
		value string
		dest  *time.Duration
	}
	fields := []durField{
		{"sync_interval", cfg.SyncInterval, &cfg.SyncIntervalDuration},
		{"sync_timeout", cfg.SyncTimeout, &cfg.SyncTimeoutDuration},
		{"min_trigger", cfg.MinTrigger, &cfg.MinTriggerDuration},
	}
	for _, f := range fields {
		d, err := time.ParseDuration(f.value)
		if err != nil {
			return fmt.Errorf(
				"配置项 %s 的取值 %q 不是合法的时间格式。\n"+
					"  支持的写法示例：30m（30 分钟）、6h（6 小时）、24h（1 天）、1h30m",
				f.name, f.value)
		}
		if d <= 0 {
			return fmt.Errorf("配置项 %s 必须大于 0，当前为 %q", f.name, f.value)
		}
		*f.dest = d
	}
	return nil
}

// Validate 校验必填项，报错信息附带获取方式，面向非技术用户
func (c *Config) Validate() error {
	var problems []string

	if c.ShortToken == "" {
		problems = append(problems, "缺少 short_token（短 token）\n"+
			"    获取方式：在浏览器打开终末地官网并登录 → 按 F12 打开开发者工具 →\n"+
			"    Application → Local Storage → 找到 ef_webview 相关项中的 token 字段，整段复制\n"+
			"    （注意：填短 token，不是 u8_token；有效期约 1~3 个月）")
	}
	// uid 非必填：留空时由服务端用短 token 自动查询对应角色，无需任何额外配置
	if c.HTTPAddr != "" && c.AccessKey == "" {
		problems = append(problems, "已启用 http_addr 但未配置 access_key\n"+
			"    HTTP 接口暴露在网络上必须鉴权，请填写一个足够随机的密钥；\n"+
			"    若不使用 HTTP 接口，请把 http_addr 留空")
	}
	if c.MinTriggerDuration >= c.SyncTimeoutDuration {
		problems = append(problems, fmt.Sprintf(
			"min_trigger（%s）不应大于等于 sync_timeout（%s）", c.MinTrigger, c.SyncTimeout))
	}
	if c.SyncIntervalDuration < time.Minute {
		problems = append(problems, fmt.Sprintf(
			"sync_interval（%s）过短，最短允许 1m\n"+
				"    官方接口可能限流，建议 6h~12h", c.SyncInterval))
	}

	if len(problems) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("配置校验未通过：\n")
	for i, p := range problems {
		sb.WriteString(fmt.Sprintf("  %d) %s\n", i+1, p))
	}
	sb.WriteString("\n可参考示例配置 config.example.yaml；如未创建配置文件，请复制一份并填入上面的必填项。")
	return errors.New(sb.String())
}

// RedactedToken 返回打码后的 token，供日志输出，避免凭证泄露
func RedactedToken(token string) string {
	if token == "" {
		return "(未配置)"
	}
	if len(token) <= 8 {
		return strings.Repeat("*", len(token))
	}
	return token[:4] + strings.Repeat("*", 6) + token[len(token)-4:]
}

// RedactedUID 返回打码后的 uid
func RedactedUID(uid string) string {
	if len(uid) <= 4 {
		return strings.Repeat("*", len(uid))
	}
	return uid[:2] + strings.Repeat("*", len(uid)-4) + uid[len(uid)-2:]
}

// Describe 生成不含敏感信息、用于启动日志的配置摘要
func (c *Config) Describe() []string {
	httpState := "未启用"
	if c.HTTPAddr != "" {
		httpState = c.HTTPAddr
	}
	// uid 留空属正常配置（启动后自动识别），此处如实说明而非显示空值
	account := "uid=自动识别 server_id=" + c.ServerID
	if c.UID != "" {
		serverType := c.ServerType
		if serverType == "" {
			serverType = "official"
		}
		account = fmt.Sprintf("uid=%s server_id=%s server_type=%s",
			RedactedUID(c.UID), c.ServerID, serverType)
	}
	return []string{
		"配置文件: " + c.ConfigPath,
		"账号: " + account,
		"凭证: short_token=" + RedactedToken(c.ShortToken),
		"同步间隔: " + c.SyncIntervalDuration.String() + "（单次超时 " + c.SyncTimeoutDuration.String() + "）",
		"落盘目录: " + c.DataDir,
		"HTTP 接口: " + httpState,
	}
}

// ExampleYAML 生成示例配置文件内容，用于配置文件缺失时打印
func ExampleYAML() string {
	return `# 终末地抽卡数据同步服务 配置示例
# 复制本文件为 config.yaml，填入 short_token 即可启动（uid 通常不用填）

# ===== 凭证 =====
short_token: "你的短token"     # 必填
# uid: "948995284"            # 选填。留空则启动时自动识别对应角色，通常不用填
server_id: "1"                # 服务器区服，官方服通常为 1

# ===== 同步设置 =====
sync_interval: "6h"           # 同步间隔，支持 30m / 6h / 24h（建议 6h~12h）
sync_timeout: "5m"            # 单次同步超时，网络慢可调到 10m
data_dir: "./data"            # 数据落盘目录

# ===== 可选 =====
lang: "zh-cn"                 # 接口语言
log_level: "info"             # debug / info / warn / error

# ===== HTTP 接口（可选，留空则不启动）=====
# 两个都填才会启动 HTTP 接口；access_key 是接口的访问密钥，请用足够随机的字符串
http_addr: ""                 # 例如 "0.0.0.0:8080"
access_key: ""                # 例如 "换成一串随机字符"
min_trigger: "60s"            # 手动触发的最小间隔，防止被高频调用
`
}

// EnsureDataDir 创建并返回绝对化的数据目录
func (c *Config) EnsureDataDir() (string, error) {
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return "", fmt.Errorf("解析 data_dir 失败：%w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("创建数据目录 %s 失败：%w", abs, err)
	}
	return abs, nil
}
