# endfield-gacha-core

《明日方舟：终末地》抽卡记录同步服务。将抽卡数据的抓取、合并、落盘能力封装为常驻服务，
可部署于 NAS、VPS 或任意 Linux 主机，按固定间隔自动同步，无需保持桌面客户端在线。

## 特性

- **定时同步** —— 默认每 6 小时一次，进程启动时立即执行一次，避免等待首个周期
- **增量抓取** —— 依据本地已落盘记录早停翻页，减少重复请求
- **免配置账号** —— 仅需提供短 token，服务自动识别绑定的游戏角色与区服
- **凭证保护** —— 日志中的 token、uid 一律脱敏输出
- **失效检测** —— 短 token 过期时中止同步并输出可操作的提示，不产生无效重试
- **状态可观测** —— 可选 HTTP 接口，支持手动触发、状态轮询与健康检查
- **无 GUI 依赖** —— 不依赖 webview，适用于无头容器环境

## 快速开始

```bash
# 1. 准备配置：复制示例文件，填写 short_token
cp config.example.yaml config.yaml
$EDITOR config.yaml

# 2. 创建数据目录
mkdir -p data

# 3. 启动
docker run -d --name endfield-sync --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  -v "$(pwd)/data:/app/data" \
  -e TZ=Asia/Shanghai \
  rolingg/endfield-gacha-core:latest

# 4. 查看日志，出现「同步完成」即表示成功
docker logs -f endfield-sync
```

> `--user "$(id -u):$(id -g)"` 使容器以当前用户身份运行。Docker 挂载宿主目录时，
> 可写性取决于宿主目录属主而非镜像内权限设置，指定该参数可避免额外的属主调整。

完整部署流程（NAS 图形界面、systemd、故障排查）见 [linux-deploy-guide.md](linux-deploy-guide.md)。

## 获取短 token

短 token 是访问账号抽卡记录的凭证，有效期约 1~3 个月，过期后需重新获取。

1. 浏览器登录《明日方舟：终末地》官网，按 `F12` 打开开发者工具
2. 切换至 **Application**（应用）标签页
3. 左侧选择 **Local Storage** → `ef-webview.hypergryph.com`
4. 找到名称包含 `token` 的条目，复制其值

> 需使用**短 token**，而非 `u8_token`。后者有效期较短，由服务端按需换取。

`uid` 无需填写。服务启动时会自动查询绑定角色：

```
已自动识别账号角色    {"昵称": "博士", "uid": "94*****84", "区服": "official"}
```

## 配置

完整配置项及说明见 [config.example.yaml](config.example.yaml)。常用项如下：

| 配置项 | 必填 | 默认值 | 说明 |
|--------|------|--------|------|
| `short_token` | 是 | — | 账号短 token |
| `uid` | 否 | 自动识别 | 通常无需填写 |
| `server_id` | 否 | `1` | 服务器区服 |
| `sync_interval` | 否 | `6h` | 同步间隔，建议 6h~12h |
| `data_dir` | 否 | `./data` | 数据目录，容器内应保持 `/app/data` |
| `http_addr` | 否 | 空 | 留空则不启用 HTTP 接口 |
| `access_key` | 否 | 空 | 启用 HTTP 接口时必填 |

**环境变量覆盖**：环境变量优先级高于配置文件，适用于不便落盘凭证的场景。

`SHORT_TOKEN` / `UID` / `SERVER_ID` / `DATA_DIR` / `SYNC_INTERVAL` /
`HTTP_ADDR` / `ACCESS_KEY` / `MIN_TRIGGER`

**配置查找顺序**：`-config` 参数 → `CONFIG_PATH` 环境变量 → 程序同级目录 `config.yaml`。

## HTTP 接口

需在配置中同时设置 `http_addr` 与 `access_key` 方可启用。

```bash
KEY="<access_key>"
curl -X POST -H "Authorization: Bearer $KEY" http://127.0.0.1:8080/api/sync
curl      -H "Authorization: Bearer $KEY" http://127.0.0.1:8080/api/sync/status
curl                                         http://127.0.0.1:8080/healthz
```

| 端点 | 方法 | 说明 |
|------|------|------|
| `/api/sync` | POST | 手动触发同步 |
| `/api/sync/status` | GET | 查询同步状态 |
| `/healthz` | GET | 健康检查，无需鉴权 |

| 状态码 | 含义 |
|--------|------|
| `202` | 已开始同步 |
| `401` | 鉴权失败 |
| `405` | 方法不允许 |
| `409` | 已有任务运行中，或触发频率超出 `min_trigger` 限制 |
| `503` | 健康检查专用：短 token 已失效，需人工处理 |

> `access_key` 是接口的唯一凭证，建议使用 `openssl rand -hex 24` 生成。
> 建议仅监听内网地址；确需外网访问时，通过 SSH 隧道或反向代理加装 TLS 与认证，
> 不要将端口直接暴露至公网。

## 数据目录

```
data/
├── logs/endfield_gacha.log          运行日志（自动切割，保留 10 份）
└── userdata/
    ├── <uid>_<创建时间>/             抽卡记录
    │   ├── official_char_history.json
    │   └── official_weapon_history.json
    ├── poolConfig/                  卡池配置
    │   ├── pool_config.json
    │   ├── discovered_pool_ids.json
    │   └── pool_types.json
    └── sync_token_state.json        token 健康状态
```

备份时打包 `data/` 目录即可，其中不含凭证。

## 凭证维护

| 情况 | 服务行为 |
|------|---------|
| 距上次成功同步超过 60 天 | 日志输出更新提醒 |
| token 已失效 | 中止同步并输出醒目错误，不再发起请求；`/healthz` 返回 `503` |

更新流程：重新获取短 token → 修改 `config.yaml` → 重启容器。

服务会持久化 token 失效状态，容器重启后不会重复发起无效请求。

## 部署

- **Docker / Linux 服务器**：[linux-deploy-guide.md](linux-deploy-guide.md)
- **docker-compose**：[docker-compose.yml](docker-compose.yml)

## 开发

```bash
go build -o endfield-sync ./cmd/server   # 编译（输出名不要用 server，会与 server/ 源码目录冲突）
go test ./...                            # 运行测试
docker build -t endfield-gacha-core .    # 构建镜像
```

命令行参数：

```bash
./endfield-sync -config /path/to/config.yaml   # 指定配置文件
./endfield-sync -example                       # 输出示例配置
./endfield-sync -version                       # 输出版本号
```

### 项目结构

```
endfield-gacha-core/
├── api/          抓取链路：HTTP 客户端、分页抓取、卡池元信息
├── model/        数据结构定义
├── storage/      落盘、合并、去重
├── logger/       日志封装
├── retry/        重试策略
├── config/       配置加载与校验
├── server/       同步调度、状态管理
│   └── httpapi/  HTTP 接口
└── cmd/server/   服务入口
```

依赖仅 `zap`、`lumberjack`、`yaml.v3`，Go 版本要求 1.24。

## 许可证

[MIT](LICENSE)
