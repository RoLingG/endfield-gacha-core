package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig 在临时目录写入配置并切换工作目录，返回配置路径
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	t.Setenv("CONFIG_PATH", path)
	return path
}

const validConfig = `
short_token: "test-token-value-1234567890"
uid: "948995284"
server_id: "1"
sync_interval: "6h"
data_dir: "./data"
`

func TestLoadValidConfig(t *testing.T) {
	writeConfig(t, validConfig)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if cfg.UID != "948995284" {
		t.Errorf("uid = %q, 期望 948995284", cfg.UID)
	}
	if cfg.SyncIntervalDuration != 6*time.Hour {
		t.Errorf("同步间隔 = %v, 期望 6h", cfg.SyncIntervalDuration)
	}
	// 未配置项应填充默认值
	if cfg.SyncTimeoutDuration != DefaultSyncTimeout {
		t.Errorf("默认超时 = %v, 期望 %v", cfg.SyncTimeoutDuration, DefaultSyncTimeout)
	}
	if cfg.Lang != DefaultLang || cfg.LogLevel != DefaultLogLevel {
		t.Errorf("默认 lang/log_level 填充失败: %q %q", cfg.Lang, cfg.LogLevel)
	}
	if cfg.ServerID != "1" {
		t.Errorf("server_id = %q, 期望 1", cfg.ServerID)
	}
}

func TestLoadMissingRequiredFields(t *testing.T) {
	writeConfig(t, "sync_interval: \"6h\"\n")
	_, err := Load()
	if err == nil {
		t.Fatal("缺少 short_token 时应报错")
	}
	msg := err.Error()
	// 报错必须包含「怎么获取」的说明，面向非技术用户
	for _, want := range []string{"short_token", "Local Storage"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺少 %q，实际内容：\n%s", want, msg)
		}
	}
	// uid 是可选项，不应出现在必填报错里
	if strings.Contains(msg, "缺少 uid") {
		t.Errorf("uid 已改为可选，不应报缺少：\n%s", msg)
	}
}

// TestUIDOptional 只填 short_token 即可通过校验，uid 由服务端自动识别
func TestUIDOptional(t *testing.T) {
	writeConfig(t, `
short_token: "test-token-value-1234567890"
server_id: "1"
`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("只填 short_token 应能通过校验: %v", err)
	}
	if cfg.UID != "" {
		t.Errorf("uid 未配置时应为空串，实际 %q", cfg.UID)
	}
}

// TestDescribeWithoutUID 未配置 uid 时日志摘要应说明「自动识别」，而非显示空值
func TestDescribeWithoutUID(t *testing.T) {
	cfg := &Config{
		ShortToken:           "test-token-value-1234567890",
		ServerID:             "1",
		SyncIntervalDuration: 6 * time.Hour,
		SyncTimeoutDuration:  5 * time.Minute,
		DataDir:              "./data",
	}
	found := false
	for _, line := range cfg.Describe() {
		if strings.HasPrefix(line, "账号:") {
			found = true
			if !strings.Contains(line, "自动识别") {
				t.Errorf("uid 为空时应提示自动识别，实际: %q", line)
			}
			if strings.Contains(line, "uid= ") || strings.HasSuffix(line, "uid=") {
				t.Errorf("不应输出空的 uid 字段: %q", line)
			}
		}
	}
	if !found {
		t.Error("配置摘要缺少账号行")
	}
}

func TestLoadInvalidDuration(t *testing.T) {
	writeConfig(t, `
short_token: "test-token-value-1234567890"
uid: "948995284"
sync_interval: "abc"
`)
	_, err := Load()
	if err == nil {
		t.Fatal("非法时间格式应报错")
	}
	if !strings.Contains(err.Error(), "30m") {
		t.Errorf("错误信息应给出格式示例，实际：%v", err)
	}
}

func TestLoadTooShortInterval(t *testing.T) {
	writeConfig(t, `
short_token: "test-token-value-1234567890"
uid: "948995284"
sync_interval: "10s"
`)
	_, err := Load()
	if err == nil {
		t.Fatal("过短的同步间隔应被拒绝")
	}
	if !strings.Contains(err.Error(), "sync_interval") {
		t.Errorf("错误信息应指明字段名，实际：%v", err)
	}
}

// TestEnvOverridesFile 环境变量优先级必须高于配置文件
func TestEnvOverridesFile(t *testing.T) {
	writeConfig(t, validConfig)
	t.Setenv("SHORT_TOKEN", "from-env-token-abcdefghij")
	t.Setenv("UID", "111222333")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.ShortToken != "from-env-token-abcdefghij" {
		t.Errorf("SHORT_TOKEN 未覆盖配置文件: %q", cfg.ShortToken)
	}
	if cfg.UID != "111222333" {
		t.Errorf("UID 未覆盖配置文件: %q", cfg.UID)
	}
}

// TestHTTPRequiresAccessKey 启用 HTTP 却未设密钥时必须拒绝启动
func TestHTTPRequiresAccessKey(t *testing.T) {
	writeConfig(t, `
short_token: "test-token-value-1234567890"
uid: "948995284"
http_addr: "0.0.0.0:8080"
`)
	_, err := Load()
	if err == nil {
		t.Fatal("启用 http_addr 但缺少 access_key 时应报错")
	}
	if !strings.Contains(err.Error(), "access_key") {
		t.Errorf("错误信息应提示 access_key，实际：%v", err)
	}
}

// TestLoadMissingFileFallsBackToEnv 配置缺失时允许纯环境变量启动（Docker 场景）
func TestLoadMissingFileFallsBackToEnv(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "not-exist.yaml"))
	t.Setenv("SHORT_TOKEN", "env-only-token-abcdefghij")
	t.Setenv("UID", "948995284")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("纯环境变量启动应当成功: %v", err)
	}
	if cfg.ShortToken != "env-only-token-abcdefghij" {
		t.Errorf("环境变量未生效: %q", cfg.ShortToken)
	}
}

func TestRedactedToken(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"空值", "", "(未配置)"},
		{"短值全遮", "abc", "***"},
		{"长值保留首尾", "test-token-value-1234567890", "test******7890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RedactedToken(c.token)
			if got != c.want {
				t.Errorf("RedactedToken(%q) = %q, 期望 %q", c.token, got, c.want)
			}
		})
	}
}

// TestRedactedTokenHidesMiddle 脱敏结果绝不能包含原文中段，避免日志泄露凭证
func TestRedactedTokenHidesMiddle(t *testing.T) {
	secret := "SUPERSECRETMIDDLEPART"
	token := "head" + secret + "tail"
	redacted := RedactedToken(token)
	if strings.Contains(redacted, secret) {
		t.Errorf("脱敏后仍包含原始凭证内容: %q", redacted)
	}
}

// TestRedactedUID 保留首尾两位，中间全部打码
func TestRedactedUID(t *testing.T) {
	if got := RedactedUID("948995284"); got != "94*****84" {
		t.Errorf("RedactedUID = %q, 期望 94*****84", got)
	}
	if got := RedactedUID("12"); got != "**" {
		t.Errorf("短 uid 应全遮，实际 %q", got)
	}
}

// TestDescribeNoSecrets 启动日志摘要不得泄露完整凭证
func TestDescribeNoSecrets(t *testing.T) {
	const token = "verysecrettoken1234567890"
	cfg := &Config{
		ShortToken:           token,
		UID:                  "948995284",
		ServerID:             "1",
		SyncIntervalDuration: 6 * time.Hour,
		SyncTimeoutDuration:  5 * time.Minute,
		DataDir:              "./data",
	}
	for _, line := range cfg.Describe() {
		if strings.Contains(line, token) {
			t.Errorf("配置摘要泄露完整 token: %q", line)
		}
	}
}
