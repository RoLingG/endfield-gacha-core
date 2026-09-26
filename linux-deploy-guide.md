# Linux 部署指南

本文档说明如何在 Linux 服务器上以 Docker 方式部署本服务。以下步骤假设目标主机已安装
Docker（可通过 `docker --version` 验证）。

> token 获取方式、配置项说明、HTTP 接口用法见 [README.md](README.md)。
>
> **在 Windows 上使用 Docker Desktop 测试**：请先阅读
> [附录 C](#附录-cwindows--docker-desktop-注意事项)，Git Bash 的路径转换会导致挂载静默失败。

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

以下命令统一使用 `/opt/endfield-sync` 作为部署目录，可按需替换。

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

参数说明：

| 参数 | 作用 |
|------|------|
| `--restart unless-stopped` | 开机自启，异常退出后自动重启 |
| `--user "$(id -u):$(id -g)"` | 以当前用户身份运行，避免挂载目录权限问题 |
| `-v .../config.yaml:/app/config.yaml:ro` | 挂载配置文件，`:ro` 表示容器内只读 |
| `-v .../data:/app/data` | 挂载数据目录，容器重建后数据保留 |
| `-e TZ=Asia/Shanghai` | 设定时区，使日志时间戳为本地时间 |

### 关于 `--user`

Docker 挂载宿主目录时，容器内进程能否写入取决于**宿主机目录的属主**，
镜像内对该路径的权限设置会被挂载覆盖。若目录属主与容器内进程的 uid 不一致，
将因权限不足导致启动失败。

`$(id -u):$(id -g)` 取当前 shell 用户的 uid 与 gid，使容器进程与宿主机目录属主一致，
从而无需 `chown`，也不依赖任何固定数值。

命令中 `sudo docker` 的 `$(id -u)` 由 shell 在调用 `sudo` 之前展开，取到的是
**当前用户**的 uid 而非 root，符合预期。若当前用户已在 `docker` 组内，
可省略 `sudo`。

若 `--user` 不适用（例如部分 NAS 的图形界面不提供该选项），参考
[附录 A：权限问题](#附录-a权限问题)。

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

```bash
# 修改配置后重启
sudo docker restart endfield-sync

# 查看日志
sudo docker logs --tail 100 endfield-sync

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

需重建容器以映射端口。此处将端口绑定到 `127.0.0.1` 而非 `0.0.0.0`，
使接口仅本机可访问：

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
curl -X POST -H "Authorization: Bearer $KEY" http://127.0.0.1:8080/api/sync   # 手动触发同步
curl      -H "Authorization: Bearer $KEY" http://127.0.0.1:8080/api/sync/status # 查询同步状态
curl                                         http://127.0.0.1:8080/healthz     # 健康检查
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

若日志出现 `短 token 已失效`，说明 token 已过期。重新获取后更新配置，
执行 `sudo docker restart endfield-sync` 即可恢复。

### 日志时间戳时区不正确

确认启动参数包含 `-e TZ=Asia/Shanghai`。

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

部分 NAS 的 Docker 界面提供 PUID / PGID 配置项，填入对应用户的 uid / gid，
效果等同于 `--user`。

**诊断当前状态**：

```bash
ls -ld /opt/endfield-sync/data                                 # 目录属主
id -u                                                          # 当前用户 uid
sudo docker inspect endfield-sync --format '{{.Config.User}}'   # 容器运行身份
```

## 附录 B：不使用 Docker 的部署方式

适用于具备 systemd 且希望直接运行二进制的主机。

### 编译

在本地交叉编译，产物为静态二进制，目标主机无需 Go 环境：

```bash
# 输出名不要用 server —— 与 server/ 源码目录同名会冲突
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o endfield-sync ./cmd/server
```

ARM 主机改用 `GOARCH=arm64`。可用 `uname -m` 确认架构：
`x86_64` 对应 `amd64`，`aarch64` 对应 `arm64`。

```bash
scp endfield-sync config.yaml <user>@<server>:/opt/endfield-sync/
```

> systemd 的工作目录与当前 shell 不同，`data_dir` 必须使用绝对路径。

### 注册为 systemd 服务

```bash
sudo tee /etc/systemd/system/endfield-sync.service > /dev/null <<'EOF'
[Unit]
Description=EndField Gacha Sync Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/endfield-sync
ExecStart=/opt/endfield-sync/endfield-sync -config /opt/endfield-sync/config.yaml
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

## 附录 C：Windows / Docker Desktop 注意事项

在 Windows 上使用 Docker Desktop 验证部署流程时，**Git Bash（MSYS2）会自动转换路径**，
导致挂载参数失效。

### 问题现象

按标准命令挂载后，程序报「未找到配置文件」：

```bash
# Git Bash 中执行
docker run -v "$(pwd)/config.yaml:/app/config.yaml:ro" ...
#   → 提示：未找到配置文件 config.yaml，将仅使用环境变量启动
```

原因是 `$(pwd)` 展开为 `/d/GoLand/endfield-gacha-core`（MSYS 路径格式），
Docker Desktop 无法识别该路径，**挂载静默失败**——不报错，只是容器内没有这个文件。

### 解决办法

**方式一：禁用路径转换**（推荐，命令可直接复制到 Linux）

```bash
MSYS_NO_PATHCONV=1 docker run -d --name endfield-sync --restart unless-stopped \
  -v "D:\path\to\endfield-sync\config.yaml:/app/config.yaml:ro" \
  -v "D:\path\to\endfield-sync\data:/app/data" \
  -e TZ=Asia/Shanghai \
  rolingg/endfield-gacha-core:latest
```

注意挂载源改为 **Windows 绝对路径**（反斜杠）。

**方式二：使用 PowerShell**

PowerShell 不做路径转换，`${PWD}` 可直接使用：

```powershell
docker run -d --name endfield-sync --restart unless-stopped `
  -v "${PWD}\config.yaml:/app/config.yaml:ro" `
  -v "${PWD}\data:/app/data" `
  -e TZ=Asia/Shanghai `
  rolingg/endfield-gacha-core:latest
```

**方式三：在 WSL2 中执行**

WSL 的路径行为与 Linux 一致，把项目放在 WSL 文件系统中执行即可。

### 验证挂载是否生效

挂载失败不会报错，务必主动确认。用一个临时容器检查文件是否存在：

```bash
# Windows（Git Bash）
MSYS_NO_PATHCONV=1 docker run --rm --entrypoint sh \
  -v "D:\path\to\config.yaml:/app/config.yaml:ro" \
  rolingg/endfield-gacha-core:latest -c "ls -la /app/config.yaml"
```

能看到文件即挂载成功；报 `No such file or directory` 说明挂载参数有问题。

### 关于 `--user`

文档中的 `--user "$(id -u):$(id -g)"` 是给 Linux 使用的。Windows 上 `id -u`
返回的不是 Linux uid，该参数无意义，本机测试时**请去掉**。
