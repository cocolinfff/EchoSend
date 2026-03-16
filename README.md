# EchoSend

> 局域网无中心全域同步引擎 · P2P 文件与消息广播守护进程

EchoSend 是一个用 **Go** 编写的单文件二进制工具，无需服务器、无需注册，开箱即用地在局域网内广播文本消息和文件。任意节点均可收发，文件传输自动校验 SHA-256 完整性，内存占用恒定（流式 IO，绝不将整个文件载入内存）。

```

```
    ┌──────────┐   UDP Gossip    ┌──────────┐
    │  Alice   │ ◄─────────────► │   Bob    │
    │ :7777/udp│                 │ :7777/udp│
    │ :7778/tcp│ ◄── TCP pull ── │ :7778/tcp│
    └──────────┘                 └──────────┘
         ▲                            ▲
         │  UDP Gossip flood          │
         └──────────── Carol ─────────┘
                      :7777/udp
```

```

---

## 目录

- [特性](#特性)
- [快速开始](#快速开始)
- [安装](#安装)
- [CLI 参考](#cli-参考)
  - [daemon — 启动守护进程](#daemon--启动守护进程)
  - [--send — 发送消息或文件](#--send--发送消息或文件)
  - [--pull — 手动重拉文件](#--pull--手动重拉文件)
  - [--history — 查看历史](#--history--查看历史)
  - [--add — 添加静态节点](#--add--添加静态节点)
  - [status — 查看节点状态](#status--查看节点状态)
- [配置文件](#配置文件)
- [后台运行](#后台运行)
- [多实例 / 多网段](#多实例--多网段)
- [架构与设计](#架构与设计)
- [构建](#构建)
- [性能约束实现](#性能约束实现)
- [目录结构](#目录结构)

---

## 特性

| 能力 | 说明 |
|------|------|
| **零配置启动** | 所有参数均有默认值，`echosend daemon` 即可启动 |
| **泛洪 Gossip** | UDP 广播 + 已知节点单播双保险，TTL 跳数防止无限循环 |
| **自动文件同步** | 收到 `FILE_META` 后按阈值自动拉取，无需人工干预 |
| **流式传输 + Hash 校验** | `io.TeeReader` 边落盘边计算 SHA-256，内存恒定 O(32KB) |
| **并发节流** | 全局信号量限制并发下载数，保护低端设备 IO |
| **跨子网探测** | 支持 CIDR 块批量单播探测，内置令牌桶限速（防爆缓冲区）|
| **BoltDB 持久化** | 纯 Go 嵌入式 KV 数据库，无 CGO，无外部依赖 |
| **单文件二进制** | 全平台静态编译，无运行时依赖 |

---

## 快速开始

### 第一步：安装

```bash
# Linux
chmod +x echosend-linux-amd64
sudo mv echosend-linux-amd64 /usr/local/bin/echosend
```

```bash
#Windows 在powershell中复制运行
[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12; $e="echosend-win32.exe"; $p="$env:APPDATA\EchoSend\data"; if(!(Test-Path $p)){ $null=New-Item -ItemType Directory -Force -Path $p }; if(!(Test-Path $e)){ Write-Host "Downloading..." -f Cyan; try{ Invoke-WebRequest "https://github.com/cocolinfff/EchoSend/releases/latest/download/$e" -OutFile $e -UseBasicParsing }catch{ Write-Host "Download failed." -f Red } }; if(Test-Path $e){ & .\$e daemon --storage $p }else{ Write-Host "Error: $e not found." -f Red }
```
---

### 第二步：启动守护进程

`echosend daemon` 是长期运行的后台服务，负责监听网络、收发数据。  
**它会占用当前终端**，有三种方式处理：

**① 后台运行（推荐日常使用）**

```bash
# 后台启动，日志写入文件，关闭终端后继续运行
nohup echosend daemon --name mynode > ~/echosend.log 2>&1 &

# 查看日志
tail -f ~/echosend.log

# 停止
pkill -f "echosend daemon"
```

**② 新开一个终端窗口运行**

```bash
echosend daemon --name mynode
```

然后在原终端使用 `--send`、`--history` 等命令。

**③ systemd 服务（服务器长期部署）**

```bash
sudo tee /etc/systemd/system/echosend.service > /dev/null <<EOF
[Unit]
Description=EchoSend P2P Daemon
After=network.target

[Service]
User=$USER
ExecStart=/usr/local/bin/echosend daemon --name $(hostname)
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now echosend
```

---

### 第三步：收发数据

守护进程启动后，在任意终端使用以下命令（局域网内所有节点约 5 秒内自动互相发现）：

```bash
# 查看已发现的节点
echosend status

# 广播一条消息（所有在线节点均可收到）
echosend --send -m "hello everyone"

# 分享一个文件（其他节点若文件 ≤ 100 MiB 则自动下载）
echosend --send -f ./report.pdf

# 手动重拉/重试某个历史文件（当自动下载跳过或失败时）
echosend --pull <sha256-file-hash>

# 查看消息和文件历史
echosend --history
```

---

### 跨子网 / 广播被屏蔽？

如果两台机器不在同一子网，或交换机屏蔽了广播包，手动指定对端 IP：

```bash
# 启动时指定，支持单 IP 或整个 CIDR 段
echosend daemon --name mynode --peers "192.168.2.100,10.0.0.0/24"

# 或对已运行的 daemon 动态添加
echosend --add 192.168.2.100
echosend --add 10.0.0.0/24
```

---

## 安装

### 下载预编译二进制

| 平台 | 文件 |
|------|------|
| Linux x86-64 | `echosend-linux-amd64` |
| Windows x86-64 | `echosend.exe` |

```bash
# Linux：赋予执行权限并移动到 PATH
chmod +x echosend-linux-amd64
sudo mv echosend-linux-amd64 /usr/local/bin/echosend

# 验证
echosend --version
```

Windows 无需安装，直接在终端运行 `.\echosend.exe` 即可。

### 从源码编译

依赖：Go ≥ 1.21，无 CGO，无系统依赖。

```bash
git clone https://github.com/yourname/echosend
cd echosend
go mod download
go build -ldflags "-X main.version=$(git describe --tags --always) -s -w" -o echosend .
```

---

## CLI 参考

所有命令均支持 `--help` 查看详细说明。

---

### `daemon` — 启动守护进程

```
echosend daemon [flags]
```

守护进程占用三个端口：UDP 广播端口、TCP 文件服务端口、本地 IPC HTTP 端口（仅监听 127.0.0.1）。

**所有 flag 均可选，有合理默认值：**

| Flag | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `--config` | path | 平台用户目录¹ | config.yaml 路径；不存在则自动创建 |
| `--name` | string | 主机名 | 在局域网内显示的节点名 |
| `--id` | string | 自动生成 | 节点唯一 ID，持久化到 config |
| `--udp-port` | int | `7777` | UDP Gossip 监听端口 |
| `--tcp-port` | int | `7778` | TCP 文件服务端口 |
| `--ipc-port` | int | `7779` | 本地 IPC HTTP 端口 |
| `--storage` | path | 平台用户目录¹ | 数据库与下载文件存储目录 |
| `--max-dl` | int | `100` | 自动下载阈值（MiB），`0` 禁用自动下载 |
| `--max-syncs` | int | `3` | 最大并发文件下载数 |
| `--peers` | string | — | 逗号分隔的静态节点，支持 IP 或 CIDR |
| `--heartbeat` | int | `5` | 心跳发送间隔（秒） |
| `--probe-rate` | int | `1000` | CIDR 探测限速（包/秒）|

> ¹ Windows: `%APPDATA%\EchoSend\`；Linux/macOS: `~/.echosend/`

**CLI flag 优先级高于 config 文件。** 启动后，最终合并值会回写到 `--config` 指定的文件，确保 NodeID 等跨重启持久化。

**示例：**

```bash
# 最简启动（全默认）
echosend daemon

# 自定义名称和端口
echosend daemon --name gateway-node --udp-port 8877 --tcp-port 8878 --ipc-port 8879

# 禁用自动下载，仅消息广播
echosend daemon --max-dl 0

# 跨子网，探测整个 C 段
echosend daemon --peers "192.168.2.0/24,10.0.1.0/24"

# 指定配置文件（适合多实例）
echosend daemon --config /etc/echosend/node-a.yaml
```

---

### `--send` — 发送消息或文件

```
echosend --send -m "消息内容"
echosend --send -f /path/to/file
```

要求守护进程已在运行。`-m` 和 `-f` 互斥。

| Flag | 说明 |
|------|------|
| `-m` | 广播文本消息 |
| `-f` | 广播文件（守护进程异步计算 SHA-256 后广播 FILE_META）|
| `--ipc-port` | 覆盖 IPC 端口（无需 config 文件） |
| `--config` | 指定 config 文件读取 IPC 端口 |

**示例：**

```bash
echosend --send -m "已部署 v2.3.1，请同步"
echosend --send -f ./dist/firmware-v2.3.1.bin
echosend --send -f ./logs.tar.gz --ipc-port 8879   # 多实例场景
```

---

### `--pull` — 手动重拉文件

```
echosend --pull <sha256-file-hash>
```

用于手动触发“按 Hash 重新拉取远端文件”，典型场景：

- 文件超过 `auto_download_max_mb`，被自动下载策略跳过
- 曾经下载失败（`[fail]`）需要重试
- 新设备刚同步到历史元数据但本地还没有文件

| Flag | 说明 |
|------|------|
| `--ipc-port` | 覆盖 IPC 端口（无需 config 文件） |
| `--config` | 指定 config 文件读取 IPC 端口 |

**示例：**

```bash
# 先通过 history 获取 hash，再手动重拉
echosend --history --limit 50
echosend --pull 3f8a1c09b2d70b7e2dbd7f9fbd3e2cce8e8b6c63f2a4e3e0fd3af1c8a9d4b2ef
```

> `--pull` 是触发命令，下载在 daemon 侧异步执行。可用 `echosend --history` 观察状态从 `[known]/[fail]` 变为 `[dl..]`、`[done]` 或 `[seed]`。

---

### `--history` — 查看历史

```
echosend --history [flags]
```

按时间倒序输出消息和文件记录，消息与文件混合显示。

| Flag | 默认 | 说明 |
|------|------|------|
| `--limit` | `20` | 最多显示条数（`0` = 全部）|
| `--json` | false | 输出原始 JSON |
| `--ipc-port` | — | 覆盖 IPC 端口 |
| `--config` | 平台默认 | 指定 config 文件 |

**示例输出：**

```
─── History (newest first) ───────────────────────────────────
  [2026-03-15 14:32:01] MSG  alice            已部署 v2.3.1，请同步
  [2026-03-15 14:30:55] FILE firmware-v2.3.1.bin    12.4MiB  [seed]  hash=3f8a1c09b2d7…
  [2026-03-15 09:11:20] MSG  bob              早上好
──────────────────────────────────────────────────────────────
```

文件状态标记：

| 标记 | 含义 |
|------|------|
| `[seed]` | 本节点为来源，正在做种 |
| `[done]` | 下载完成，SHA-256 校验通过 |
| `[dl..]` | 正在下载中 |
| `[fail]` | 下载失败或 Hash 不匹配 |
| `[known]` | 已知元数据，等待下载 |

---

### `--add` — 添加静态节点

```
echosend --add <ip 或 cidr>
```

将目标持久化到 config.yaml 的 `static_peers` 列表，并通知运行中的守护进程在下一个心跳周期开始探测。

```bash
echosend --add 192.168.2.50
echosend --add 10.0.0.0/24     # 探测整个 /24，254 个地址，限速 1000 包/秒
echosend --add 172.16.0.0/16   # /16 = 65534 地址，自动分 ~66 秒发完
```

---

### `status` — 查看节点状态

```
echosend status [--ipc-port PORT]
```

显示当前节点信息和已发现的对等节点列表：

```
Node  : alice
ID    : 6bfaccd952fc7fa9326d57392dd1b5a2
Peers : 2

NAME     ID              IP             UDP    TCP    LAST SEEN
────     ──              ──             ───    ───    ─────────
bob      a1b2c3d4e5f6…  192.168.1.42   7777   7778   14:31:08
charlie  9988776655aa…  192.168.1.55   7777   7778   14:30:52
```

---

## 配置文件

默认路径：

| 平台 | 路径 |
|------|------|
| Windows | `%APPDATA%\EchoSend\config.yaml` |
| Linux / macOS | `~/.echosend/config.yaml` |
| 环境变量覆盖 | `ECHOSEND_CONFIG=/path/to/config.yaml` |

**完整示例：**

```yaml
# 节点标识
node_name: "my-node"
node_id: ""                    # 留空则首次启动自动生成并回写

# 网络端口
daemon_port_udp: 7777          # UDP Gossip 广播
daemon_port_tcp: 7778          # TCP 文件传输
daemon_port_ipc: 7779          # 本地 IPC（仅 127.0.0.1）

# 文件同步策略
auto_download_max_mb: 100      # 0 = 禁用自动下载
max_concurrent_syncs: 3        # 并发下载上限

# 存储
storage_dir: "~/.echosend/data"

# 静态节点（跨子网 / 广播受限环境）
static_peers:
  - "192.168.2.100"
  - "10.0.0.0/24"

# 心跳与探测
heartbeat_interval_sec: 5
seen_packet_ttl_sec: 300       # 已见包 ID 缓存保留时长
probe_rate_per_sec: 1000       # CIDR 探测限速（防爆 UDP 发送缓冲区）
```

> **优先级：** CLI flag > config.yaml > 内置默认值

---

## 后台运行

`echosend daemon` 是守护进程，设计上会**持续占用终端**输出日志。根据使用场景选择合适的运行方式：

| 方式 | 命令 | 适用场景 |
|------|------|----------|
| 前台运行 | `echosend daemon` | 调试、观察日志 |
| 后台运行 | `nohup echosend daemon > echosend.log 2>&1 &` | 日常个人使用 |
| systemd 服务 | 见快速开始第二步 | 服务器长期部署 |

**查看后台进程：**

```bash
ps aux | grep echosend
```

**停止后台进程：**

```bash
pkill -f "echosend daemon"
# 或用 PID
kill $(pgrep -f "echosend daemon")
```

---

## 多实例 / 多网段

在同一台机器上运行多个实例时，使用不同端口和不同 config 文件：

```bash
# 实例 A：服务内网 192.168.1.0/24
echosend daemon --config /etc/echosend/a.yaml \
  --name "bridge-internal" \
  --udp-port 7777 --tcp-port 7778 --ipc-port 7779 \
  --storage /var/lib/echosend/a

# 实例 B：服务 VPN 网段 10.8.0.0/24
echosend daemon --config /etc/echosend/b.yaml \
  --name "bridge-vpn" \
  --udp-port 8877 --tcp-port 8878 --ipc-port 8879 \
  --storage /var/lib/echosend/b \
  --peers "10.8.0.0/24"

# 向实例 B 发送消息
echosend --send -m "跨 VPN 消息" --ipc-port 8879
```

---

## 架构与设计

```
┌─────────────────────────────────────────────────────────┐
│                      echosend daemon                    │
│                                                         │
│  ┌─────────────┐   ┌─────────────┐   ┌──────────────┐  │
│  │  UDPEngine  │   │  TCPServer  │   │  IPC Server  │  │
│  │ (Gossip)    │   │ (File Srv)  │   │ (127.0.0.1)  │  │
│  └──────┬──────┘   └──────┬──────┘   └──────┬───────┘  │
│         │                 │                  │          │
│  ┌──────▼──────────────────▼──────┐          │          │
│  │         Daemon Orchestrator    │◄─────────┘          │
│  │   handlePresence / handleMsg   │                     │
│  └──────┬──────────────────┬──────┘                     │
│         │                  │                            │
│  ┌──────▼──────┐   ┌───────▼──────┐                     │
│  │PeerRegistry │   │  Orchestrator│                     │
│  │ (in-memory) │   │  (filesync)  │                     │
│  └──────┬──────┘   └───────┬──────┘                     │
│         │                  │                            │
│  ┌──────▼──────────────────▼──────┐                     │
│  │          BoltDB Storage        │                     │
│  │  nodes │ messages │ files      │                     │
│  └────────────────────────────────┘                     │
└─────────────────────────────────────────────────────────┘
         ▲                        ▲
   CLI --send                echosend --history
   CLI --add                 echosend status
   (HTTP POST/GET to IPC)
```

### 核心数据流

**消息广播：**
```
CLI --send -m  →  IPC HTTP POST  →  Daemon  →  UDP Gossip 广播
                                         ↓
                              远端节点收到 → 去重 → 持久化 → 再广播（TTL-1）
```

**文件传输：**
```
CLI --send -f  →  IPC HTTP POST  →  Daemon 流式读取计算 SHA-256
                                         ↓
                              UDP 广播 FILE_META（含 Hash + TCP 端口）
                                         ↓
                     远端节点收到 → 判断大小 ≤ 阈值 → TCP 拉取
                                         ↓
                    io.TeeReader → 同步落盘 + 计算 Hash → 比对 → 原子重命名
```

### 关键设计决策

| 问题 | 方案 | 原因 |
|------|------|------|
| 无限广播循环 | PacketID + TTL 双重去重 | 环形拓扑安全终止 |
| 大文件 OOM | `io.TeeReader` 流式处理 | 内存恒定 O(32KB) |
| 高频 UDP GC 压力 | `sync.Pool` 复用收包缓冲区 | 减少 GC 停顿 |
| 并发 IO 过载 | 信号量（buffered channel）| 精确控制并发数 |
| 大 CIDR 探测爆栈 | Worker Pool + Ticker 限速 | 防止 UDP 发送缓冲区溢出 |
| Windows/Linux 兼容 | build-tag 拆分 sockopt | 无需外部依赖 |

---

## 构建

### 原生构建

```bash
go build -ldflags "-X main.version=$(git describe --tags --always) -s -w" -o echosend .
```

### 交叉编译

```bash
# Linux x86-64
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o echosend-linux-amd64 .

# macOS ARM (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -ldflags "-s -w" -o echosend-darwin-arm64 .

# Windows x86-64
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o echosend-windows-amd64.exe .
```

### GitHub Actions 手动发版（自动 +0.0.1）

仓库内置 workflow：`.github/workflows/release-manual.yml`

- 触发方式：GitHub Actions 页面手动触发 `Manual Build and Release`
- 版本策略：读取最新 `vX.Y.Z` tag，自动发布下一个补丁版本 `vX.Y.(Z+1)`
- 首次无 tag 时从 `v0.0.1` 开始
- 产物：
  - `echosend-win64.exe`
  - `echosend-win32.exe`
  - `echosend-linux64`
  - `echosend-linux32`
  - `SHA256SUMS.txt`

Release 会自动上传以上文件并生成 release notes。

### 使用 Makefile 一键构建全平台

```makefile
VERSION := $(shell git describe --tags --always --dirty)
LDFLAGS := -ldflags "-X main.version=$(VERSION) -s -w"

all: linux windows darwin

linux:
	GOOS=linux   GOARCH=amd64  go build $(LDFLAGS) -o dist/echosend-linux-amd64  .
	GOOS=linux   GOARCH=arm64  go build $(LDFLAGS) -o dist/echosend-linux-arm64  .

windows:
	GOOS=windows GOARCH=amd64  go build $(LDFLAGS) -o dist/echosend-windows-amd64.exe .

darwin:
	GOOS=darwin  GOARCH=amd64  go build $(LDFLAGS) -o dist/echosend-darwin-amd64  .
	GOOS=darwin  GOARCH=arm64  go build $(LDFLAGS) -o dist/echosend-darwin-arm64  .
```

### 依赖

| 包 | 用途 |
|---|------|
| `go.etcd.io/bbolt` | 嵌入式 KV 数据库（纯 Go，无 CGO）|
| `gopkg.in/yaml.v3` | YAML 配置解析 |

其余全部使用 Go 标准库。

---

## 性能约束实现

以下对应 todo.md 中的性能要求：

### 内存极简：禁止整体读入大文件

文件发送侧（`filesync/filesync.go`）：

```go
h := sha256.New()
buf := make([]byte, 64*1024)          // 固定 64KB 缓冲
io.CopyBuffer(h, f, buf)              // 边读边算 Hash，文件内容不驻留内存
```

文件接收侧（`network/tcp.go`）：

```go
tee := io.TeeReader(src, sha256Hash)  // 每次 Read 同时写入 Hash
io.CopyBuffer(destFile, tee, buf)     // 边落盘边校验，内存恒定
```

### CPU / GC 优化：sync.Pool 复用 UDP 缓冲区

```go
bufPool := sync.Pool{
    New: func() interface{} {
        b := make([]byte, 8192)
        return &b
    },
}
// 热路径：借用 → 读 → 拷贝内容 → 立即归还，池始终热
bufPtr := e.bufPool.Get().(*[]byte)
n, addr, _ := e.conn.ReadFromUDP(*bufPtr)
data := make([]byte, n)
copy(data, (*bufPtr)[:n])
e.bufPool.Put(bufPtr)            // 立即归还，不等 goroutine 完成
```

### 并发节流：信号量控制下载并发数

```go
sem := make(chan struct{}, cfg.MaxConcurrentSyncs)
for i := 0; i < cap; i++ { sem <- struct{}{} }

// 下载前 acquire
<-sem
defer func() { sem <- struct{}{} }()  // 完成后 release
```

### CIDR 探测限速：令牌桶 + Worker Pool

```go
delay   := time.Second / time.Duration(ratePerSec)  // e.g. 1ms/pkt @ 1000/s
ticker  := time.NewTicker(delay)
workCh  := make(chan string, workers*2)

// 产生 token
for _, ip := range targets {
    <-ticker.C          // 控制发送速率
    workCh <- ip
}
```

---

## 目录结构

```
EchoSend/
├── main.go                         # CLI 入口、子命令路由
├── config.yaml                     # 默认配置示例
├── go.mod / go.sum
│
└── internal/
    ├── models/
    │   └── models.go               # 核心数据结构（Packet, FileMeta, Message…）
    │
    ├── config/
    │   └── config.go               # 配置加载、校验、持久化
    │
    ├── storage/
    │   └── storage.go              # BoltDB 存储层（nodes/messages/files 三桶）
    │
    ├── utils/
    │   └── cidr.go                 # IP/CIDR 解析、广播地址枚举
    │
    ├── network/
    │   ├── peers.go                # 对等节点内存注册表
    │   ├── udp.go                  # UDP Gossip 引擎（收发、去重、泛洪、心跳）
    │   ├── udp_helpers.go          # crypto/rand 封装、原子计数器
    │   ├── udp_sockopt_windows.go  # Windows SO_BROADCAST（build tag）
    │   ├── udp_sockopt_unix.go     # Linux/macOS SO_BROADCAST（build tag）
    │   ├── tcp.go                  # TCP 文件服务器 + 客户端下载
    │   └── tcp_hash.go             # SHA-256 工厂函数
    │
    ├── filesync/
    │   └── filesync.go             # 文件同步编排（发布、自动下载、重试、播种）
    │
    ├── ipc/
    │   ├── server.go               # Daemon 侧 IPC HTTP 服务
    │   └── client.go               # CLI 侧 IPC HTTP 客户端
    │
    └── daemon/
        └── daemon.go               # 顶层编排器（启动、信号处理、优雅停机）
```

---

## 常见问题

**Q: 防火墙需要开放哪些端口？**

| 端口 | 协议 | 方向 | 用途 |
|------|------|------|------|
| 7777 | UDP | 双向 | Gossip 广播与单播 |
| 7778 | TCP | 入站 | 文件下载服务 |
| 7779 | TCP | 本机 | IPC，仅 127.0.0.1，无需开放 |

**Q: 文件下载到哪里？**

下载到 `storage_dir` 目录（默认 `~/.echosend/data/`）。完整性校验失败的临时文件（`.tmp` 后缀）会被自动删除。

**Q: 节点重启后消息/文件记录会丢失吗？**

不会。所有消息和文件元数据均持久化到 BoltDB（`echosend.db`），重启后从数据库恢复。

**Q: 同一 Hash 的文件会重复下载吗？**

不会。下载完成后状态变为 `SEEDING`，再次收到相同 `FILE_META` 时存储层返回 `ErrDuplicateFile` 并丢弃。

**Q: 支持 IPv6 吗？**

当前 UDP/TCP 监听仅绑定 `udp4`/`tcp4`。CIDR 解析层接受 IPv6 地址但不展开（直接作为单播目标返回）。完整 IPv6 支持为后续 roadmap。

---

## License

MIT
