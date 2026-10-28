# loomy2api（Go 版）

Loomy（讯飞）账号池 → OpenAI / Anthropic 兼容网关。本项目是 [loomy2api](../loomy2api) 的
逐文件移植：**纯 Go 1.27 标准库，零第三方依赖，单二进制**，配置与命令行行为与 Python 版一致。

## 构建

```bash
go build ./cmd/loomy2api          # 产出 loomy2api（Windows 为 loomy2api.exe）
```

`go.mod` 只有 `module loomy2api` + `go 1.27`，没有 require。

## 配置与运行

```bash
./loomy2api                             # 不带参数等价于 ./loomy2api serve，默认 http://127.0.0.1:17890
./loomy2api add main --phone 13800000000 --password '你的密码'
cp accounts.example.json accounts.json  # 存密码，已在 .gitignore 里（add 也会自动建）
```

首次启动时若 `config.json` 不存在（默认 `./config.json`，或 `-c/--config` 指定的路径），
程序会先自动生成一份带注释的默认配置再继续加载，所有子命令都一样。这份模板是
`internal/loomy/default-config.json`（`go:embed` 进二进制，内容同 `config.example.json`，
改其中一个记得同步另一个）。

优先级：**环境变量 > config.json > 内置默认值**；配置里的相对路径相对 config.json 所在目录。

常用命令（`./loomy2api <cmd> --help` 看详细用法）：

| 命令 | 作用 |
| --- | --- |
| `serve` | 启动网关（`--port` 覆盖配置端口；不带子命令时默认就是它） |
| `accounts` / `quota` | 账号池状态 / 各账号积分 |
| `add` / `remove` | 加账号 / 删账号（`remove` 无 `-y` 时会问一次） |
| `login [name ...]` | 强制重登并续期（缺省=全部） |
| `sms` / `verify` | 短信登录两步：发码 → 用验证码换 session |
| `identity <name>` | 查看设备标识（`--rebind` 换新设备并重登） |
| `models` / `chat "prompt"` | 列上游模型 / 单次对话自检 |

全局参数 `-c/--config <path>` 写在子命令之前（与 Python 版一致）。

环境变量一览：`LOOMY_HOST`、`LOOMY_PORT`、`LOOMY_UPSTREAM`、`LOOMY_ACCOUNT_BASE`、
`LOOMY_AK_ID`、`LOOMY_AK_SECRET`、`LOOMY_API_KEYS`（逗号分隔，空=不鉴权）、
`LOOMY_DEFAULT_MODEL`、`LOOMY_PROXY`（默认直连）、`LOOMY_ACCOUNTS_FILE`、
`LOOMY_LOG_DIR`、`LOOMY_STRATEGY`（`balance`/`round_robin`/`lru`）。

HTTP 端点：`POST /v1/chat/completions`（含 SSE 流式）、`POST /v1/messages`（Anthropic 兼容）、
`GET /v1/models`、`GET /v1/points`、`GET /health`、面板 `GET /panel`（`/` 即面板）。

## Docker

```bash
docker build -t loomy2api .
docker run -d --name loomy2api -p 17890:17890 -v "$PWD/data:/data" loomy2api
```

镜像多阶段构建：`golang:1.27` 编译出静态二进制，`alpine:3.21` 只带二进制、CA 证书与时区。
容器内固定 `LOOMY_HOST=0.0.0.0`、`LOOMY_ACCOUNTS_FILE=/data/accounts.json`、
`LOOMY_LOG_DIR=/data/logs`，所以把 `config.json`、`accounts.json` 放进挂载到 `/data` 的目录即可。

## 测试

```bash
go test ./...            # 或 go test ./internal/loomy -count=1
go vet ./...
```

测试全部跑在本地假上游（`internal/loomy/support_test.go` 的 `FakeUpstream` /
`FakeAccountClient`）上，不访问真实账号服务，也不花积分。

## 目录映射

| Python | Go |
| --- | --- |
| `loomy2api/config.py` / `constants.py` | `internal/loomy/config.go` / `constants.go` |
| `loomy2api/account.py` / `pool.py` | `internal/loomy/account.go` / `pool.go` |
| `loomy2api/upstream.py` / `httpc.py` | `internal/loomy/upstream.go` / `httpc.go` |
| `loomy2api/crypto.py` / `signer.py` / `scrypt.py` | `internal/loomy/crypto.go` / `signer.go` / `scrypt.go` |
| `loomy2api/server.py` | `internal/loomy/server.go`（`net/http` 取代 `http.server`） |
| `loomy2api/anthropic.py` | `internal/loomy/anthropic.go` |
| `loomy2api/panel.py` + `web/index.html` | `internal/loomy/panel.go` + `internal/loomy/web/index.html`（`go:embed`） |
| `loomy2api/cli.py` | `internal/loomy/cli.go` |
| `loomy2api/__main__.py` | `cmd/loomy2api/main.go` |
| `tests/` | `internal/loomy/*_test.go` |

## 已知差异

- 控制台编码：Python 版要把 stdout 强转 UTF-8（Windows 遗留代码页），Go 直接写 UTF-8，无需处理。
- 非 `PoolError`/`AccountError` 的异常：Python 打 traceback，Go 打一行 `错误：…`，退出码同为 1。
- `chat` 打印的 `usage:` JSON 由 Go 的 `encoding/json` 序列化：键按字典序、无 Python 默认空格，内容等价。
- 超时语义：`http.client` 的 socket 级超时换成「整体超时 + 响应头超时」（流式只卡响应头），见 `httpc.go` 注释。
- 面板/账号池的后台维护用 goroutine + channel 取代 Python 的线程与 `threading.Event`。

协议细节（登录、签名、SSE、账号额度）见原项目的 [docs/PROTOCOL.md](../loomy2api/docs/PROTOCOL.md)。
