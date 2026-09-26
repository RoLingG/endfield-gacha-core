package logger

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var Log *zap.Logger

// Options 日志初始化选项。零值即为桌面版原有行为。
type Options struct {
	// Dir 日志文件所在目录。为空时退回 exe 同级的 userdata/logs
	Dir string
	// Level 日志级别：debug / info / warn / error，默认 info
	Level string
	// Console false 时仅写文件。服务端由 supervisor 收集 stdout，
	// 保持 true 便于 `docker logs` 直接查看
	Console bool
}

// InitLogger 以默认选项初始化（桌面版行为：exe 同级 userdata/logs + 控制台输出）
func InitLogger() {
	InitLoggerWithOptions(Options{Level: "info", Console: true})
}

// InitLoggerWithOptions 按指定选项初始化日志。
// 失败时降级为仅控制台输出，保证日志始终可用（服务端启动阶段尤其重要）。
func InitLoggerWithOptions(opt Options) {
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout(time.DateTime)
	encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

	level := parseLevel(opt.Level)
	cores := make([]zapcore.Core, 0, 2)

	fileCore, err := newFileCore(opt.Dir, encoderConfig, level)
	if err != nil {
		// 目录不可写（如只读挂载）时不能拖垮服务，仅提示后继续走控制台
		fallbackConsoleOnly(encoderConfig, level)
		Log.Warn("日志文件初始化失败，已降级为仅控制台输出", zap.Error(err))
		return
	}
	cores = append(cores, fileCore)

	if opt.Console {
		consoleConfig := encoderConfig
		consoleConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		cores = append(cores, zapcore.NewCore(
			zapcore.NewConsoleEncoder(consoleConfig),
			zapcore.AddSync(os.Stdout),
			level,
		))
	}

	Log = zap.New(zapcore.NewTee(cores...), zap.AddCaller())
}

// newFileCore 构建带切割的日志文件 Core
func newFileCore(dir string, encoderConfig zapcore.EncoderConfig, level zapcore.Level) (zapcore.Core, error) {
	if dir == "" {
		exePath, err := os.Executable()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(filepath.Dir(exePath), "userdata", "logs")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	rotator := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "endfield_gacha.log"),
		MaxSize:    5, // MB
		MaxAge:     5, // 天
		MaxBackups: 10,
		Compress:   true,
	}
	return zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(rotator),
		level,
	), nil
}

// fallbackConsoleOnly 文件 Core 不可用时仅保留控制台输出
func fallbackConsoleOnly(encoderConfig zapcore.EncoderConfig, level zapcore.Level) {
	consoleConfig := encoderConfig
	consoleConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	Log = zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(consoleConfig),
		zapcore.AddSync(os.Stdout),
		level,
	), zap.AddCaller())
}

// parseLevel 解析日志级别字符串，无法识别时退回 info
func parseLevel(s string) zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return zapcore.DebugLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// PrintBanner 打印启动横幅与逐行启动信息（不含敏感字段，调用方须先脱敏）
func PrintBanner(title string, version string, lines []string) {
	if Log == nil {
		return
	}
	Log.Info(title, zap.String("version", version))
	for _, line := range lines {
		Log.Info(line)
	}
}

func Sync() {
	if Log != nil {
		_ = Log.Sync()
	}
}
