// Command server 是终末地抽卡数据同步服务端入口。
//
// 无界面常驻程序：读配置 → 定时调度 → 抓取落盘，可选开放 HTTP 接口用于手动触发与状态查询。
// 不依赖 webview，凭证由用户填入配置文件或环境变量（见 docs/backend-plan.md 第八节）。
//
// 支持的命令行参数：
//
//	-config /path/to/config.yaml   指定配置文件路径
//	-example                       打印示例配置后退出
//	-version                       打印版本号后退出
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"endfield-gacha-core/config"
	"endfield-gacha-core/logger"
	"endfield-gacha-core/server"
	"endfield-gacha-core/server/httpapi"
	"endfield-gacha-core/storage"

	"go.uber.org/zap"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入
var version = "dev"

func main() {
	// -config 的取值由 config.Path() 自行解析（需先于 flag.Parse 生效），
	// 此处仅注册一个占位 flag 让 flag 包接受该参数而不报「未定义」
	flag.String("config", "", "配置文件路径（默认程序同级 config.yaml，可用 CONFIG_PATH 指定）")
	showExample := flag.Bool("example", false, "打印示例配置后退出")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Printf("endfield-gacha-core %s\n", version)
		return
	}

	// -example 必须早于配置加载：它的使用场景正是「还没有可用的配置文件」，
	// 此时校验必然失败，若放在后面就永远打不出示例
	if *showExample {
		fmt.Print(config.ExampleYAML())
		return
	}

	// 先加载配置：日志目录依赖 data_dir，故日志初始化必须晚于配置解析
	cfg, err := config.Load()
	if err != nil {
		printStartupFailure(err)
		return
	}

	dataDir, err := cfg.EnsureDataDir()
	if err != nil {
		printStartupFailure(err)
		return
	}

	logger.InitLoggerWithOptions(logger.Options{
		Dir:     dataDir + string(os.PathSeparator) + "logs",
		Level:   cfg.LogLevel,
		Console: true, // 容器场景由 `docker logs` 收集
	})
	defer logger.Sync()

	// 数据根目录改为配置项：桌面版取 exe 同级，容器中需指向挂载卷
	// 目录结构与其他落盘逻辑保持一致：<data_dir>/userdata/<uid>_<时间>/
	storage.SetBaseDir(dataDir + string(os.PathSeparator) + storage.DataDirectoryName)

	logger.PrintBanner("终末地抽卡数据同步服务启动", version, append(cfg.Describe(),
		"数据根目录: "+storage.DataDirectoryName,
	))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	syncer := server.NewSyncer(cfg)

	var httpSrv *httpapi.Server
	if cfg.HTTPAddr != "" {
		httpSrv = httpapi.New(cfg.HTTPAddr, cfg.AccessKey, syncer)
		httpSrv.Start()
	} else {
		logger.Log.Info("未配置 http_addr，跳过 HTTP 服务（手动触发与状态查询不可用）")
	}

	// Run 阻塞至收到关闭信号或子任务收尾完成
	syncer.Run(ctx)

	// 退出前先停掉 HTTP 监听，再留出时间让在场请求收尾
	if httpSrv != nil {
		if err := httpSrv.Shutdown(); err != nil {
			logger.Log.Debug("HTTP 服务关闭时出错", zap.Error(err))
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	<-shutdownCtx.Done()

	logger.Log.Info("服务已停止")
}

// printStartupFailure 启动阶段失败时在 stderr 给出可操作提示。
// 此时日志系统尚未初始化，必须直接写标准错误，否则用户看不到原因。
func printStartupFailure(err error) {
	fmt.Fprintf(os.Stderr, "\n启动失败：%v\n\n", err)
	fmt.Fprintf(os.Stderr, "已查找的配置文件：%s\n", config.Path())
	fmt.Fprintf(os.Stderr, "可用 -config /path/to/config.yaml 指定路径，或用 CONFIG_PATH 环境变量指定。\n")
	fmt.Fprintf(os.Stderr, "需要示例配置时执行：%s -example\n\n", os.Args[0])
}
