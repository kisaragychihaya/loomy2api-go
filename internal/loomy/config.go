package loomy

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 对应 Python loomy2api/config.py —— 配置加载。
// 优先级：环境变量 > config.json > 内置默认值。
// 配置里的相对路径相对 root（持有 config.json 的目录 / 当前工作目录）解析。

// Config 是运行期配置，字段与 config.py 的 DEFAULTS / ENV_MAP 一一对应。
type Config struct {
	Host                   string
	Port                   int
	Upstream               string
	AccountBase            string
	AccountAppID           string
	AccessKeyID            string
	AccessKeySecret        string
	LoomyVersion           string
	Proxy                  string
	APIKeys                []string
	DefaultModel           string
	Timeout                int // 秒，默认 1200
	RequestPurpose         string
	LogDir                 string
	LogRequests            bool
	LogConsole             bool
	StripModelPrefix       bool
	AccountsFile           string
	Strategy               string
	MaxRetries             int
	CooldownSeconds        int
	SessionRenewBeforeDays float64
	QuotaRefreshMinutes    float64
	SessionsFromClient     bool
	ClientRoot             string
	IdentityMode           string
	SourcePath             string // 实际读取/尝试的 config.json 路径（热重载用）
	root                   string // 非导出：相对路径解析根目录
}

// DefaultConfig 返回与 config.py DEFAULTS 完全一致的默认配置。
// root 取当前工作目录；Go 的布尔零值是 false，故显式把四个 true 默认值写出来。
func DefaultConfig() *Config {
	root, err := os.Getwd()
	if err != nil {
		root = "."
	}
	return &Config{
		Host:                   "127.0.0.1",
		Port:                   17890,
		Upstream:               DefaultModelBase,
		AccountBase:            DefaultAccountBase,
		AccountAppID:           DefaultAccountAppID,
		AccessKeyID:            "", // 空 → 使用客户端内置常量
		AccessKeySecret:        "",
		LoomyVersion:           DefaultLoomyVersion,
		Proxy:                  "", // "" = 直连
		APIKeys:                []string{},
		DefaultModel:           DefaultModel,
		Timeout:                1200,
		RequestPurpose:         "chat.message",
		LogDir:                 "logs",
		LogRequests:            true,
		LogConsole:             true,
		StripModelPrefix:       true,
		AccountsFile:           "accounts.json",
		Strategy:               "balance",
		MaxRetries:             2,
		CooldownSeconds:        300,
		SessionRenewBeforeDays: 3,
		QuotaRefreshMinutes:    30,
		SessionsFromClient:     true,
		ClientRoot:             "", // 覆盖客户端数据目录（测试用）
		IdentityMode:           "per_account",
		root:                   root,
	}
}

//go:embed default-config.json
var defaultConfigJSON []byte

// configPath 按 LoadConfig 的约定解析配置文件路径：
// path=="" → <cwd>/config.json；否则转绝对路径（转失败时原样返回）。
func configPath(path string) string {
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		return filepath.Join(cwd, "config.json")
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// EnsureConfigFile 确保配置文件存在：不存在则写入内置默认模板并返回 created=true。
// path 为空时按 LoadConfig 的约定取 <cwd>/config.json；已存在则不改动。
// 模板是 config.example.json 的副本（带 _comment 注释键，加载器会忽略 _ 前缀键）。
func EnsureConfigFile(path string) (created bool, err error) {
	target := configPath(path)

	if _, serr := os.Stat(target); serr == nil {
		return false, nil
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return false, serr
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(target, defaultConfigJSON, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// LoadConfig 对应 Python load_config：
//   - path=="" 时读 <cwd>/config.json，文件不存在则静默沿用默认值；
//   - path!="" 时读该文件，解析失败返回错误；
//   - 之后用非空环境变量覆盖。
//
// root 为配置文件所在目录（显式 path）或 cwd（path==""）。
func LoadConfig(path string) (*Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	cfg := DefaultConfig()
	cfg.root = cwd

	cfgPath := configPath(path)
	if path != "" {
		cfg.root = filepath.Dir(cfgPath)
	}
	// 记录实际读取（或尝试读取）的路径：文件不存在也记录，供热重载 watcher 使用。
	cfg.SourcePath = cfgPath

	data, rerr := os.ReadFile(cfgPath)
	switch {
	case rerr == nil:
		var raw any
		if jerr := json.Unmarshal(data, &raw); jerr != nil {
			return nil, fmt.Errorf("config.json 解析失败 / parse error: %w", jerr)
		}
		// 非对象（null / 数组 / 标量）在 Python 里也是被忽略的，不报错。
		if obj, isObj := raw.(map[string]any); isObj {
			cfg.applyJSON(obj)
		}
	case errors.Is(rerr, fs.ErrNotExist):
		// 文件不存在：静默使用默认值（显式 path 亦同，与 Python 一致）。
	default:
		return nil, fmt.Errorf("config.json 解析失败 / parse error: %w", rerr)
	}

	cfg.applyEnv()
	return cfg, nil
}

// applyJSON 把 config.json 的键覆盖到默认值上：忽略 _ 前缀键，未知键忽略。
func (c *Config) applyJSON(raw map[string]any) {
	for k, v := range raw {
		if strings.HasPrefix(k, "_") || v == nil {
			continue
		}
		switch k {
		case "host":
			c.Host = cfgStr(v)
		case "port":
			c.Port = cfgInt(v, c.Port)
		case "upstream":
			c.Upstream = cfgStr(v)
		case "account_base":
			c.AccountBase = cfgStr(v)
		case "account_appid":
			c.AccountAppID = cfgStr(v)
		case "access_key_id":
			c.AccessKeyID = cfgStr(v)
		case "access_key_secret":
			c.AccessKeySecret = cfgStr(v)
		case "loomy_version":
			c.LoomyVersion = cfgStr(v)
		case "proxy":
			c.Proxy = cfgStr(v)
		case "api_keys":
			c.APIKeys = cfgKeys(v)
		case "default_model":
			c.DefaultModel = cfgStr(v)
		case "timeout":
			c.Timeout = cfgInt(v, c.Timeout)
		case "request_purpose":
			c.RequestPurpose = cfgStr(v)
		case "log_dir":
			c.LogDir = cfgStr(v)
		case "log_requests":
			c.LogRequests = cfgBool(v, c.LogRequests)
		case "log_console":
			c.LogConsole = cfgBool(v, c.LogConsole)
		case "strip_model_prefix":
			c.StripModelPrefix = cfgBool(v, c.StripModelPrefix)
		case "accounts_file":
			c.AccountsFile = cfgStr(v)
		case "strategy":
			c.Strategy = cfgStr(v)
		case "max_retries":
			c.MaxRetries = cfgInt(v, c.MaxRetries)
		case "cooldown_seconds":
			c.CooldownSeconds = cfgInt(v, c.CooldownSeconds)
		case "session_renew_before_days":
			c.SessionRenewBeforeDays = cfgFloat(v, c.SessionRenewBeforeDays)
		case "quota_refresh_minutes":
			c.QuotaRefreshMinutes = cfgFloat(v, c.QuotaRefreshMinutes)
		case "sessions_from_client":
			c.SessionsFromClient = cfgBool(v, c.SessionsFromClient)
		case "client_root":
			c.ClientRoot = cfgStr(v)
		case "identity_mode":
			c.IdentityMode = cfgStr(v)
		}
	}
}

// applyEnv 按 ENV_MAP 覆盖非空环境变量；int 解析失败时保持原值。
func (c *Config) applyEnv() {
	if v := os.Getenv("LOOMY_HOST"); v != "" {
		c.Host = v
	}
	if v := os.Getenv("LOOMY_PORT"); v != "" {
		c.Port = cfgAtoi(v, c.Port)
	}
	if v := os.Getenv("LOOMY_UPSTREAM"); v != "" {
		c.Upstream = v
	}
	if v := os.Getenv("LOOMY_ACCOUNT_BASE"); v != "" {
		c.AccountBase = v
	}
	if v := os.Getenv("LOOMY_AK_ID"); v != "" {
		c.AccessKeyID = v
	}
	if v := os.Getenv("LOOMY_AK_SECRET"); v != "" {
		c.AccessKeySecret = v
	}
	if v := os.Getenv("LOOMY_API_KEYS"); v != "" {
		c.APIKeys = splitKeys(v)
	}
	if v := os.Getenv("LOOMY_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("LOOMY_PROXY"); v != "" {
		c.Proxy = v
	}
	if v := os.Getenv("LOOMY_ACCOUNTS_FILE"); v != "" {
		c.AccountsFile = v
	}
	if v := os.Getenv("LOOMY_LOG_DIR"); v != "" {
		c.LogDir = v
	}
	if v := os.Getenv("LOOMY_STRATEGY"); v != "" {
		c.Strategy = v
	}
}

// AKID 对应 Python ak_id：AccessKeyID 为空时回退客户端内置常量。
func (c *Config) AKID() string {
	if c.AccessKeyID != "" {
		return c.AccessKeyID
	}
	return DefaultAccessKeyID
}

// AKSecret 对应 Python ak_secret：AccessKeySecret 为空时回退客户端内置常量。
func (c *Config) AKSecret() string {
	if c.AccessKeySecret != "" {
		return c.AccessKeySecret
	}
	return DefaultAccessKeySecret
}

// Path 把 p 按 root 解析：绝对路径原样返回，空字符串回退 root。
func (c *Config) Path(p string) string {
	if p == "" {
		return c.root
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.root, p)
}

// ---------------------------------------------------------------- 类型转换

// cfgStr 把 JSON 值转成字符串（float64 去掉无意义的 .0）。
func cfgStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// cfgInt 把 JSON 值转成 int，失败时返回 def。
func cfgInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	case string:
		return cfgAtoi(t, def)
	}
	return def
}

// cfgFloat 把 JSON 值转成 float64，失败时返回 def。
func cfgFloat(v any, def float64) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return def
}

// cfgBool 把 JSON 值转成 bool，失败时返回 def。
func cfgBool(v any, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(t)); err == nil {
			return b
		}
	}
	return def
}

// cfgKeys 解析 api_keys：既接受字符串数组，也接受逗号分隔的字符串。
func cfgKeys(v any) []string {
	out := []string{}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []string:
		for _, s := range t {
			if s != "" {
				out = append(out, s)
			}
		}
	case string:
		out = splitKeys(t)
	}
	return out
}

// splitKeys 按逗号拆分配置里的 api key 串，去空白、丢空项。
func splitKeys(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cfgAtoi 解析十进制整数，失败时返回 def。
func cfgAtoi(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return n
}
