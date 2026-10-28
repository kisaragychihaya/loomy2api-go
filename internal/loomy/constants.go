package loomy

// 对应 Python loomy2api/constants.py —— 默认端点与客户端常量。
// 这里的内容都可以在运行时通过 config.json / 环境变量覆盖，只是默认值。

// ---------------------------------------------------------------- upstreams

// DefaultModelBase 是模型网关地址（OpenAI 兼容：/chat/completions、/models ...）。
const DefaultModelBase = "https://loomyad.xunfei.cn/api/v1"

// DefaultAccountBase 是讯飞账号服务地址（登录 / userinfo）。
const DefaultAccountBase = "https://account.xfinfr.com"

// DefaultAccountAppID 是账号服务的 appid。
const DefaultAccountAppID = "GM3LOOMY"

// 官方客户端内置的账号服务签名常量。
const (
	DefaultAccessKeyID     = "2thryby66wxi53sk"
	DefaultAccessKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r"
)

// DefaultLoomyVersion 作为 loomy-version 请求头发送；网关只校验它存在。
const DefaultLoomyVersion = "0.9.38"

// 客户端混淆 resources/.env.prod 用的口令与魔数。
const (
	EnvCryptMagic      = "LOOMYENC1"
	EnvCryptPassphrase = "loomy::env::3f9c1d2a7b6e4f08::local-obfuscation::v1"
)

// ClientPublicRoot 是客户端在 Windows 上的数据目录（<root>/<sha256(user)[:12]>）。
const ClientPublicRoot = `C:\Users\Public\Loomy`

// ---------------------------------------------------------------- account

// SessionExpireSeconds：客户端总是申请 14 天会话，没有 refresh token。
const SessionExpireSeconds = 14 * 24 * 3600

const (
	DeviceID      = "web"
	WebModelID    = "Web"
	ClientVersion = "1.0.0"
	ClientUA      = "Loomy|Desktop|Electron|macOS"
)

// CampusDeviceIDPrefix 是客户端由机器指纹推导出的推广 device id 前缀
// （loomy-campus-fp-<sha256(machineId)>），只用于 /points/activation 与
// /points/first-login，从不随聊天请求发送。
const CampusDeviceIDPrefix = "loomy-campus-"

// RequestPurposes 是上游能识别的 X-Loomy-Request-Purpose 取值（共 17 个）。
var RequestPurposes = []string{
	"chat.message", "chat.title", "pet.comment", "pet.reward",
	"kb.vision", "kb.audio", "kb.summary", "kb.embed",
	"search.web", "image.generate", "assistant.completion", "assistant.asr-correct",
	"memory.extract", "memory.recall", "memory.dream",
	"skill.evolution", "skill.curator",
}

// ---------------------------------------------------------------- models

// FallbackModels 是上游 /models 调用失败时使用的兜底模型目录。
// multiplier 是客户端上报的每请求积分成本系数。
var FallbackModels = []map[string]any{
	{"id": "spark-x", "name": "Spark X2.5 (x0.1)", "multiplier": 0.1,
		"context_length": int64(1048576), "modalities": []any{"text"}},
	{"id": "GLM-5.3-Flash", "name": "GLM 5.3 Flash (x0.8)", "multiplier": 0.8,
		"context_length": int64(1048576), "modalities": []any{"text", "image", "video"}},
	{"id": "qwen3.8-flash", "name": "qwen 3.8 flash (x0.8)", "multiplier": 0.8,
		"context_length": int64(1000000), "modalities": []any{"text", "image", "video"}},
	{"id": "deepseek-v4-flash-0731", "name": "DeepSeek V4 Flash 0731 (x3.0)",
		"multiplier": 3.0, "context_length": int64(1048576), "modalities": []any{"text"}},
	{"id": "mimo-v2.5", "name": "MiMo V2.5 (x3.3)", "multiplier": 3.3,
		"context_length": int64(1048576), "modalities": []any{"text", "image", "audio", "video"}},
	{"id": "MiniMax-M3", "name": "MiniMax M3 (x4.0)", "multiplier": 4.0,
		"context_length": int64(1048576), "modalities": []any{"text", "image", "video"}},
	{"id": "Kimi-k2.6", "name": "Kimi k2.6 (x6.5)", "multiplier": 6.5,
		"context_length": int64(262144), "modalities": []any{"text", "image", "video"}},
	{"id": "qwen-3.8-max", "name": "Qwen 3.8 Max (x12.0)", "multiplier": 12.0,
		"context_length": int64(1000000), "modalities": []any{"text"}},
	{"id": "Hy-Image-3.5-preview", "name": "Hy image 3.5 preview",
		"multiplier": 0.0, "context_length": int64(100000), "modalities": []any{"text", "image"}},
	{"id": "doubao-seedream-5-lite", "name": "Doubao seedream 5 lite",
		"multiplier": 0.0, "context_length": int64(128000), "modalities": []any{"text", "image"}},
	{"id": "qwen-image-3.0-pro", "name": "qwen image 3.0 pro",
		"multiplier": 0.0, "context_length": int64(0), "modalities": []any{"text", "image"}},
}

// DefaultModel 是默认模型 id。
const DefaultModel = "deepseek-v4-flash-0731"

// 客户端加密模块要求的最小 AES-GCM tag / iv / salt 尺寸。
const (
	SaltLen = 16
	IVLen   = 12
	TagLen  = 16
	KeyLen  = 32
)

// IvLen 是 IVLen 的别名，方便沿用 IV_LEN 的机械转写写法。
const IvLen = IVLen
