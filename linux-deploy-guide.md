# Linux 部署指南

本文档说明如何在 Linux 服务器上以 Docker 方式部署本服务。以下步骤假设目标主机已安装 Docker（可通过 `docker --version` 验证）。

> token 获取方式、配置项说明、HTTP 接口用法见 [README.md](README.md)。

## 目录

- [一、准备配置文件](#一准备配置文件)
- [二、创建数据目录](#二创建数据目录)
- [三、启动服务](#三启动服务)
- [四、验证运行](#四验证运行)
- [五、日常操作](#五日常操作)
- [六、启用 HTTP 接口（可选）](#六启用-http-接口可选)
- [七、故障排查](#七故障排查)
- [附录 A：权限问题](#附录-a权限问题)
- [附录 B：不使用 Docker 的部署方式](#附录-b不使用-docker-的部署方式)

第一至七章（Docker 方式）统一使用 `/opt/endfield-sync` 作为部署目录，用于存放配置文件与数据，可按需替换为其他路径。

> 该目录与源码目录无关，只是容器挂载数据的落点，无需在其中放置代码。不使用 Docker 的部署方式见[附录 B](#附录-b不使用-docker-的部署方式)。

## 一、准备配置文件

```bash
mkdir -p /opt/endfield-sync && cd /opt/endfield-sync
```

创建 `config.yaml`：

```yaml
# ===== 凭证 =====
short_token: "<在此填入短 token>"      # 必填
server_id: "1"                        # 官方服通常为 1

# ===== 同步设置 =====
sync_interval: "6h"                   # 支持 30m / 6h / 24h，建议 6h~12h
data_dir: "/app/data"

# ===== 可选 =====
lang: "zh-cn"
log_level: "info"                     # debug / info / warn / error

# ===== HTTP 接口（可选，留空则不启用）=====
http_addr: ""                         # 例如 "0.0.0.0:8080"
access_key: ""                        # 启用 http_addr 时必填
```

完整配置项见 [config.example.yaml](config.example.yaml)。

配置文件含明文凭证，需限制访问权限：

```bash
chmod 600 config.yaml
```

## 二、创建数据目录

```bash
mkdir -p data
```

目录属主无需调整，权限由启动参数 `--user` 处理（见下节）。

## 三、启动服务

镜像已发布至 Docker Hub，**无需自行构建**，`docker run` 时会自动拉取：

```bash
cd /opt/endfield-sync

sudo docker run -d \
  --name endfield-sync \
  --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -v /opt/endfield-sync/config.yaml:/app/config.yaml:ro \
  -v /opt/endfield-sync/data:/app/data \
  -e TZ=Asia/Shanghai \
  rolingg/endfield-gacha-core:latest
```

如拉取缓慢，可先单独执行 `sudo docker pull rolingg/endfield-gacha-core:latest` 观察进度。
镜像仓库地址为 https://hub.docker.com/r/rolingg/endfield-gacha-core 。

参数说明：

| 参数 | 作用 |
|------|------|
| `--restart unless-stopped` | 开机自启，异常退出后自动重启 |
| `--user "$(id -u):$(id -g)"` | 以当前用户身份运行，避免挂载目录权限问题 |
| `-v .../config.yaml:/app/config.yaml:ro` | 挂载配置文件，`:ro` 表示容器内只读 |
| `-v .../data:/app/data` | 挂载数据目录，容器重建后数据保留 |
| `-e TZ=Asia/Shanghai` | 设定时区，使日志时间戳为本地时间 |

### 关于 `--user`

Docker 挂载宿主目录时，容器内进程能否写入取决于**宿主机目录的属主**，镜像内对该路径的权限设置会被挂载覆盖。若目录属主与容器内进程的 uid 不一致，将因权限不足导致启动失败。

`$(id -u):$(id -g)` 取当前 shell 用户的 uid 与 gid，使容器进程与宿主机目录属主一致，从而无需 `chown`，也不依赖任何固定数值。

命令中 `sudo docker` 的 `$(id -u)` 由 shell 在调用 `sudo` 之前展开，取到的是**当前用户**的 uid 而非 root，符合预期。若当前用户已在 `docker` 组内，可省略 `sudo`。

若 `--user` 不适用（例如部分 NAS 的图形界面不提供该选项），参考 [附录 A：权限问题](#附录-a权限问题)。

## 四、验证运行

```bash
sudo docker logs -f endfield-sync
```

日志中出现 `同步完成` 表示同步成功。按 `Ctrl+C` 退出日志查看，不会停止容器。

```
终末地抽卡数据同步服务启动    {"version": "..."}
账号: uid=自动识别 server_id=1
已自动识别账号角色    {"昵称": "博士", "uid": "94*****84", "区服": "official"}
同步完成    {"来源": "启动", "角色记录": 123, "武器记录": 45, "耗时": "8.2s"}
```

确认数据已落盘：

```bash
ls -R /opt/endfield-sync/data
# data/userdata/<uid>_<时间>/official_char_history.json
# data/userdata/<uid>_<时间>/official_weapon_history.json
```

## 五、日常操作

### 查看运行状态

```bash
# 正在运行的容器
sudo docker ps

# 包含已停止的
sudo docker ps -a

# 只看本服务
sudo docker ps -a --filter name=endfield-sync

# 关键字段一览
sudo docker ps -a --format "table {{.Names}}\t{{.Status}}\t{{.Image}}\t{{.Ports}}"
```

`STATUS` 列的含义：

| 显示 | 含义 |
|------|------|
| `Up 5 minutes` | 正常运行 |
| `Exited (0) ...` | 已退出，查看日志确认原因 |
| `Restarting (1) ...` | 反复重启，通常为配置或权限错误 |
| `Created` | 已创建但未启动，执行 `sudo docker start endfield-sync` |

查看日志：

```bash
sudo docker logs -f endfield-sync              # 实时跟踪
sudo docker logs --tail 100 endfield-sync      # 最近 100 行
sudo docker logs --since 30m endfield-sync     # 最近 30 分钟
```

### 修改配置后生效

`config.yaml` 是挂载进容器的，程序**只在启动时读取一次**，修改后必须重启容器：

```bash
sudo docker restart endfield-sync
```

验证配置已生效（确认日志中的间隔与预期一致）：

```bash
sudo docker logs --tail 20 endfield-sync | grep 同步调度
# 同步调度已启动    {"间隔": "6h0m0s", "下次执行": "2026-09-27 05:35:02"}
```

### 重启与重建的区别

| 操作 | 命令 | 适用场景 |
|------|------|---------|
| **重启** | `sudo docker restart endfield-sync` | 只改了配置文件内容（如 token、同步间隔） |
| **重建** | `sudo docker rm -f endfield-sync` 后重新 `docker run` | 改了挂载路径、环境变量、端口映射，或更新了镜像 |

`restart` 只重启进程，**不会改变容器的启动参数**（挂载、`-e`、`-p`）。启动参数有变动时必须重建容器。

重建不会丢数据——数据在宿主机 `data/` 目录中。

### 更新镜像

```bash
sudo docker pull rolingg/endfield-gacha-core:latest
sudo docker rm -f endfield-sync
sudo docker run -d --name endfield-sync --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -v /opt/endfield-sync/config.yaml:/app/config.yaml:ro \
  -v /opt/endfield-sync/data:/app/data \
  -e TZ=Asia/Shanghai \
  rolingg/endfield-gacha-core:latest
```

### 调整同步间隔

修改 `config.yaml` 中的 `sync_interval` 后重启容器：

```yaml
sync_interval: "360h"     # 15 天
```

| 单位 | 说明 |
|------|------|
| `m` | 分钟 |
| `h` | 小时 |
| `d` | **不支持**，`time.ParseDuration` 无此单位，会导致启动失败 |

因此「15 天」应写 `360h`（15 × 24）。

程序允许的最短间隔为 1 分钟，小于该值会被拒绝启动。

> **不建议设置过长的间隔**。官方 `char/meta` 接口仅覆盖近 90 天记录；间隔过长还会推迟 token 失效的发现时间。默认的 `6h` 采用增量同步，无新记录时几乎不产生请求，无需为此调大。

### 停止 / 备份 / 卸载

```bash
# 停止 / 启动
sudo docker stop endfield-sync
sudo docker start endfield-sync

# 备份数据（不含凭证）
tar czf endfield-backup-$(date +%F).tar.gz -C /opt/endfield-sync data

# 卸载
sudo docker rm -f endfield-sync
sudo docker rmi rolingg/endfield-gacha-core:latest
```

## 六、启用 HTTP 接口（可选）

在 `config.yaml` 中添加：

```yaml
http_addr: "0.0.0.0:8080"
access_key: "<随机密钥>"              # 必填，缺失时服务拒绝启动
```

生成随机密钥：

```bash
openssl rand -hex 24
```

需重建容器以映射端口。此处将端口绑定到 `127.0.0.1` 而非 `0.0.0.0`，使接口仅本机可访问：

```bash
sudo docker rm -f endfield-sync
sudo docker run -d --name endfield-sync --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -v /opt/endfield-sync/config.yaml:/app/config.yaml:ro \
  -v /opt/endfield-sync/data:/app/data \
  -e TZ=Asia/Shanghai \
  -p 127.0.0.1:8080:8080 \
  rolingg/endfield-gacha-core:latest
```

接口调用：

```bash
KEY="<access_key>"
# 手动触发同步
curl -X POST -H "Authorization: Bearer $KEY" http://127.0.0.1:8080/api/sync
# 查询同步状态
curl -H "Authorization: Bearer $KEY" http://127.0.0.1:8080/api/sync/status
# 健康检查
curl http://127.0.0.1:8080/healthz     
```

需要从外部主机访问时，建议使用 SSH 隧道，避免直接暴露端口：

```bash
ssh -N -L 8080:127.0.0.1:8080 <user>@<server>
```

## 七、故障排查

### 容器无法启动

```bash
sudo docker ps -a --filter name=endfield-sync    # 查看退出状态
sudo docker logs endfield-sync                   # 查看具体错误
```

| 日志内容 | 原因与处理 |
|----------|-----------|
| `缺少 short_token` | 配置未填写或未正确挂载，检查 `-v` 路径 |
| `配置文件 ... 解析失败` | YAML 语法错误，检查引号是否配对 |
| `创建数据目录 /app/data 失败` | 目录权限问题，见 [附录 A](#附录-a权限问题) |
| `address already in use` | 端口被占用，更换映射端口 |

### 未出现「同步完成」

按序排查：

```bash
# 1. 主机出网是否正常
curl -sS -o /dev/null -w '%{http_code}\n' https://ef-webview.hypergryph.com/api/content

# 2. 系统时间是否准确（偏差过大会导致请求被拒）
date

# 3. 查看具体错误
sudo docker logs --tail 50 endfield-sync
```

若日志出现 `短 token 已失效`，说明 token 已过期。重新获取后更新配置，执行 `sudo docker restart endfield-sync` 即可恢复。

### 日志时间戳时区不正确

确认启动参数包含 `-e TZ=Asia/Shanghai`。

### 挂载的配置文件未被读取

现象：日志提示 `未找到配置文件 config.yaml，将仅使用环境变量启动`，但 `docker run` 中已通过 `-v` 指定了配置文件。

挂载失败**不会报错**，需主动验证。检查挂载是否生效：

```bash
sudo docker run --rm --entrypoint sh \
  -v /opt/endfield-sync/config.yaml:/app/config.yaml:ro \
  rolingg/endfield-gacha-core:latest -c "ls -la /app/config.yaml"
```

- 能列出文件 → 挂载正常
- 报 `No such file or directory` → 挂载参数有误

常见原因：

| 原因 | 处理 |
|------|------|
| 使用了相对路径 | `-v` 的宿主侧路径改为**绝对路径** |
| 文件不存在 | 先在宿主机上创建 `config.yaml` |
| 路径拼写错误 | 用 `ls` 确认宿主机路径正确 |

### 自行构建镜像时失败

镜像已发布至 Docker Hub，**正常部署无需构建**，直接 `docker pull` 即可。
仅在需要修改源码后自行构建（`docker build`）时，才可能遇到以下问题。

**问题一：基础镜像元数据无法解析**

```
failed to resolve source metadata for docker.io/library/alpine:latest:
encountered unknown type text/html; children may not be fetched
```

原因：镜像加速器返回了 HTML 页面而非镜像数据。逐一探测各加速器：

```bash
for m in <加速器地址1> <加速器地址2>; do
  echo "--- $m ---"
  curl -sI -m 10 "https://$m/v2/" | grep -iE "^HTTP|^content-type"
done
```

判断标准：

| 返回 | 含义 |
|------|------|
| `401` + `content-type: application/json` | 可用 |
| `200` + `content-type: text/html` | **失效**，返回的是网页，需从配置中移除 |
| 超时无响应 | 不可达 |

配置位置为 `/etc/docker/daemon.json` 的 `registry-mirrors` 字段，修改后需 `sudo systemctl restart docker`。

需注意：Docker 按顺序尝试各加速器，**若首位失效，构建仍会失败**（`docker pull` 会自动 fallback，但 `docker build` 的元数据解析不一定），因此应移除失效项而非仅追加。若使用 1Panel 等面板管理 Docker，其配置可能被面板覆盖，建议在面板界面中修改。

**问题二：构建阶段拉取 Go 依赖超时**

```
go.uber.org/multierr@v1.11.0: Get "https://proxy.golang.org/...": i/o timeout
```

Dockerfile 已默认使用国内代理（`goproxy.cn`），支持通过构建参数覆盖：

```bash
sudo docker build --build-arg GOPROXY=https://proxy.golang.org,direct -t ... .
```

同理，运行阶段的 Alpine 包源已替换为清华镜像，可用
`--build-arg APK_MIRROR=<地址>` 覆盖。

### 无法登录或推送 Docker Hub

现象：

```
Error logging in to endpoint ...
net/http: request canceled while waiting for connection
```

原因：`docker login` 与 `docker push` 访问的是 `registry-1.docker.io`，
**镜像加速器只代理拉取，不参与认证与推送**，国内网络往往无法直连。

可选方案：

1. **配置 HTTP 代理**：为 Docker daemon 设置 `HTTP_PROXY` / `HTTPS_PROXY`
2. **本地推送**：在可访问 Docker Hub 的机器上 `docker load` 后推送
   （服务器 `docker save` 导出，传输到本地后 `docker load`）
3. **使用 CI 构建**：在 GitHub Actions 等海外环境中自动构建并推送

> 通过 GitHub 账号注册的 Docker Hub 用户没有独立密码，
> `docker login` 的密码栏需填写 **Access Token**
> （Docker Hub → Account Settings → Security → New Access Token）。

## 附录 A：权限问题

**现象**：容器反复重启，日志报 `创建数据目录 /app/data 失败`。

**原因**：Docker 挂载宿主目录时，容器内能否写入取决于宿主机目录的属主，
镜像内对该路径的权限设置会被挂载覆盖。目录属主与容器进程 uid 不一致时即无写权限。

**首选方案**：启动时添加 `--user "$(id -u):$(id -g)"`。本文档各处的启动命令均已包含。

**替代方案**（图形界面不提供 `--user` 选项时）：

```bash
# 1. 将数据目录属主改为镜像默认用户（Dockerfile 中 uid=1000）
sudo chown -R 1000:1000 /opt/endfield-sync/data

# 2. 放宽目录权限
sudo chmod -R 777 /opt/endfield-sync/data

# 3. 以 root 身份运行（可用，但放弃非 root 隔离，不推荐）
sudo docker run ... --user 0 ... rolingg/endfield-gacha-core:latest
```

部分 NAS 的 Docker 界面提供 PUID / PGID 配置项，填入对应用户的 uid / gid，效果等同于 `--user`。

**诊断当前状态**：

```bash
ls -ld /opt/endfield-sync/data                                 # 目录属主
id -u                                                          # 当前用户 uid
sudo docker inspect endfield-sync --format '{{.Config.User}}'   # 容器运行身份
```

## 附录 B：不使用 Docker 的部署方式

适用于希望直接运行二进制的主机，无需安装 Docker。

### 编译

**方式一：在服务器上直接编译**（需要 Go 1.24 及以上）

```bash
cd <项目目录>
go build -o endfield-sync ./cmd/server
```

**方式二：本地交叉编译后上传**（目标主机无需 Go 环境）

```bash
# 输出名不要用 server —— 与 server/ 源码目录同名会冲突
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o endfield-sync ./cmd/server
```

ARM 主机改用 `GOARCH=arm64`。可用 `uname -m` 确认架构：`x86_64` 对应 `amd64`，`aarch64` 对应 `arm64`。

上传到服务器上的项目目录：

```bash
scp endfield-sync config.yaml <user>@<server>:<项目目录>/
```

### 后台运行

用 `nohup` 让程序在后台持续运行，退出 SSH 也不会中断。**在项目目录内执行**：

```bash
cd <项目目录>

nohup ./endfield-sync -config ./config.yaml > endfield.log 2>&1 &
```

命令说明：

| 部分 | 作用 |
|------|------|
| `nohup` | 忽略挂断信号，关闭 SSH 后进程继续运行 |
| `> endfield.log 2>&1` | 将标准输出与错误一并写入日志文件 |
| `&` | 放入后台执行 |

程序自身也会写一份日志到 `data/logs/endfield_gacha.log`（自动切割），上面的 `endfield.log` 只是兜住控制台输出，两者内容一致。

> **必须在项目目录内启动**。配置中的 `data_dir` 若为相对路径（如 `./data`），数据会落在**启动时的工作目录**下。从其他目录启动会导致数据落到意料之外的位置，或直接改成绝对路径来规避。

验证运行状态：

```bash
# 查看进程
ps aux | grep endfield-sync | grep -v grep

# 跟踪日志
tail -f endfield.log
```

日志中出现 `同步完成` 即运行正常。

停止服务：

```bash
pkill -f endfield-sync
```

> **注意**：`nohup` 方式在服务器重启后不会自动拉起，进程异常退出也不会自动重启。若需要这两项保障，改用下面的 systemd 方式。

### 注册为 systemd 服务（可选）

相比 `nohup`，systemd 额外提供开机自启与崩溃自动重启，适合长期运行。以下 `<项目目录>` 请替换为实际路径（必须是绝对路径）。

> systemd 的工作目录与当前 shell 不同，`data_dir` 必须使用绝对路径。

```bash
sudo tee /etc/systemd/system/endfield-sync.service > /dev/null <<'EOF'
[Unit]
Description=EndField Gacha Sync Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=<项目目录>
ExecStart=<项目目录>/endfield-sync -config <项目目录>/config.yaml
Restart=on-failure
RestartSec=10

# 停止时发送 SIGTERM，并留出时间供运行中的任务收尾
KillSignal=SIGTERM
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now endfield-sync
sudo journalctl -u endfield-sync -f
```

