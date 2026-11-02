package loomy

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// 对应 Python loomy2api/account.py —— 账号模型 + 讯飞账号服务客户端。
//
// 一个 Account 就是一个 Loomy/讯飞账号：手机号、密码，以及账号服务下发的 14 天会话。
// 会话正是模型网关要的 Bearer token，所以「服务端登录」之后就能直接当 API key 用。
//
// 登录路径（都在服务端完成，不依赖桌面客户端）：
//
//   - 密码：/login/account/getPuKey 返回 1024 位 RSA 公钥和 rcode（服务端 nonce，
//     不是验证码），密码用 RSA/PKCS#1 v1.5 加密后 POST 到 /login/account/byPwd。
//   - 短信：/login/phone/sendMsgCode → msgid，再 /login/phone/checkCode（需要人工读一次码）。
//
// 每个账号带一份 identity（见 NewIdentity）：协议里真正承载「设备」的字段只有请求信封里的
// devid / ua / modelid / version，外加每请求一个随机 traceid。给每个账号绑定不同的
// identity 只是让它们在服务端看起来是不同设备，并不改变网络出口。

// IdentityFields 列出构成账号 identity 的字段（对应 Python IDENTITY_FIELDS）。
// 注意 traceid 不在其中：真客户端每个请求都重新生成。
var IdentityFields = []string{"devid", "ua", "modelid", "version", "campus_device_id"}

// ------------------------------------------------------------------- 错误

// AccountError 是账号服务的业务错误（对应 Python AccountError）。
type AccountError struct {
	Msg       string
	Code      string
	Retryable bool
}

func (e *AccountError) Error() string {
	return e.Msg
}

// ---------------------------------------------------------------- identity

// NewIdentity 对应 Python new_identity：为一个账号构造设备 identity。
//
//   - mode=="per_account"（默认）：devid 为 web-<16 位十六进制>，campus_device_id 各不相同；
//     ua / modelid / version 保持真客户端的值，因为编造的 UA 比重复的 UA 更显眼。
//   - mode=="client"：完全照抄客户端（devid "web"，campus_device_id 留空）。
//
// traceid 故意不在 identity 里：真客户端每个请求都新生成一个。
func NewIdentity(mode string) map[string]any {
	devid := DeviceID
	campus := CampusDeviceIDPrefix + signerUUID()
	if mode == "client" {
		campus = ""
	} else {
		devid = "web-" + accountHex(16)
	}
	return map[string]any{
		"devid":            devid,
		"ua":               ClientUA,
		"modelid":          WebModelID,
		"version":          ClientVersion,
		"campus_device_id": campus,
		"created_at":       time.Now().Unix(),
	}
}

// accountHex 对应 Python 的 uuid.uuid4().hex[:n]。
func accountHex(n int) string {
	raw := strings.ReplaceAll(signerUUID(), "-", "")
	if n > len(raw) {
		n = len(raw)
	}
	if n < 0 {
		n = 0
	}
	return raw[:n]
}

// accountNow 对应 Python 的 time.time()：带小数的 unix 秒。
func accountNow() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// ---------------------------------------------------------------- Account

// Account 对应 account.py 的 dataclass：一个账号加上它缓存的会话 / 额度状态。
// 时间字段（ExpireAt / ObtainedAt / QuotaUpdatedAt）都是 unix 秒。
type Account struct {
	Name     string
	Loginid  string // 登录用的手机号
	Password string
	Enabled  bool
	Persist  bool // 由客户端派生的账号为 false

	Session    string
	Userid     string
	ExpireAt   int64
	ObtainedAt int64

	// Identity 是每账号的设备标识（devid / ua / campus_device_id / …）。
	Identity map[string]any

	// 额度缓存（由 /points/records 填充）。
	Balance        *int64
	DailyBalance   *int64
	Available      *int64
	Multiplier     float64 // 只用于展示，不参与选号
	QuotaUpdatedAt int64

	// 运行期状态，从不落盘。
	CooldownUntil float64
	LastUsed      float64
	Requests      int
	PointsUsed    int
	Failures      int
	LastError     string
	Source        string // config | client
}

// NewAccount 对应 Python 的 Account(name=...) 默认值：
// Enabled / Persist 为 true，Source 为 "config"，Identity 为空 map。
// 直接写 &Account{Name: "x"} 会拿到零值（Enabled=false），语义不同。
func NewAccount(name string) *Account {
	return &Account{
		Name:     name,
		Enabled:  true,
		Persist:  true,
		Identity: map[string]any{},
		Source:   "config",
	}
}

// SessionValid 对应 Python session_valid：会话非空且未过期（ExpireAt==0 视为长期有效）。
func (a *Account) SessionValid() bool {
	return a.Session != "" && (a.ExpireAt == 0 || a.ExpireAt > time.Now().Unix())
}

// DaysLeft 对应 Python days_left：返回 (剩余天数, ExpireAt 是否已知)。
// ExpireAt==0 时返回 (0, false)。
func (a *Account) DaysLeft() (float64, bool) {
	if a.ExpireAt == 0 {
		return 0, false
	}
	return (float64(a.ExpireAt) - accountNow()) / 86400, true
}

// InCooldown 对应 Python in_cooldown。
func (a *Account) InCooldown() bool {
	return a.CooldownUntil > accountNow()
}

// Cooldown 对应 Python cooldown：至少冷却 1 秒，记一次失败，可附原因到 LastError。
func (a *Account) Cooldown(seconds float64, reason string) {
	if seconds < 1 {
		seconds = 1
	}
	a.CooldownUntil = accountNow() + seconds
	a.Failures++
	if reason != "" {
		a.LastError = reason
	}
}

// NewAccountFromDict 对应 Python Account.from_dict。
//
// name 依次回落 name → loginid → "account<index+1>"；enabled 只有显式 JSON false 才是
// false（Python 的 `raw.get("enabled", True) is not False`）；expire/obtained 同时兼容
// 驼峰与下划线两种键名。
func NewAccountFromDict(raw map[string]any, index int) *Account {
	name := jstr(raw, "name")
	if name == "" {
		name = jstr(raw, "loginid")
	}
	if name == "" {
		name = "account" + strconv.Itoa(index+1)
	}

	loginid := jstr(raw, "loginid")
	if loginid == "" {
		loginid = jstr(raw, "phone")
	}

	enabled := true
	if v, ok := raw["enabled"]; ok {
		if b, isBool := v.(bool); isBool && !b {
			enabled = false
		}
	}

	identity := map[string]any{}
	if ident, isMap := raw["identity"].(map[string]any); isMap {
		for k, v := range ident {
			identity[k] = v
		}
	}

	expireAt := pyInt64(raw["expireAt"])
	if expireAt == 0 {
		expireAt = pyInt64(raw["expire_at"])
	}
	obtainedAt := pyInt64(raw["obtainedAt"])
	if obtainedAt == 0 {
		obtainedAt = pyInt64(raw["obtained_at"])
	}

	return &Account{
		Name:       name,
		Loginid:    loginid,
		Password:   jstr(raw, "password"),
		Enabled:    enabled,
		Persist:    true,
		Session:    jstr(raw, "session"),
		Userid:     jstr(raw, "userid"),
		ExpireAt:   expireAt,
		ObtainedAt: obtainedAt,
		Identity:   identity,
		Source:     "config",
	}
}

// ToDict 对应 Python to_dict：只写出非空字段，键名保持客户端的驼峰写法。
func (a *Account) ToDict() map[string]any {
	out := map[string]any{
		"name":    a.Name,
		"enabled": a.Enabled,
	}
	if a.Loginid != "" {
		out["loginid"] = a.Loginid
	}
	if a.Password != "" {
		out["password"] = a.Password
	}
	if a.Session != "" {
		out["session"] = a.Session
	}
	if a.Userid != "" {
		out["userid"] = a.Userid
	}
	if a.ExpireAt != 0 {
		out["expireAt"] = a.ExpireAt
	}
	if a.ObtainedAt != 0 {
		out["obtainedAt"] = a.ObtainedAt
	}
	if len(a.Identity) > 0 {
		out["identity"] = a.Identity
	}
	return out
}

// IdentityView 对应 Python identity_view：面板 / admin API 展示用的 identity，
// campus_device_id 超过 32 字符时截断并加省略号（里面没有秘密）。
func (a *Account) IdentityView() map[string]any {
	ident := a.Identity
	if ident == nil {
		ident = map[string]any{}
	}

	campus := jstr(ident, "campus_device_id")
	campusView := any(campus)
	if runes := []rune(campus); len(runes) > 32 {
		campusView = string(runes[:32]) + "…"
	}

	createdAt := any(int64(0))
	if v, ok := ident["created_at"]; ok {
		createdAt = v
	}

	return map[string]any{
		"devid":            jstr(ident, "devid"),
		"ua":               jstr(ident, "ua"),
		"modelid":          jstr(ident, "modelid"),
		"version":          jstr(ident, "version"),
		"campus_device_id": campusView,
		"created_at":       createdAt,
		"bound":            len(ident) > 0,
	}
}

// PublicDict 对应 Python public_dict：/admin/accounts 用的状态视图，不含任何敏感字段。
// session 只保留前 8 个字符 + 省略号，cool down 剩余秒数保留 1 位小数。
func (a *Account) PublicDict() map[string]any {
	masked := ""
	if a.Session != "" {
		masked = accountPrefix(a.Session, 8) + "…"
	}

	var daysLeft any
	if days, ok := a.DaysLeft(); ok {
		daysLeft = accountRound(days, 2)
	}

	cooldownLeft := any(0)
	if a.InCooldown() {
		cooldownLeft = accountRound(a.CooldownUntil-accountNow(), 1)
	}

	return map[string]any{
		"name":                  a.Name,
		"loginid":               a.Loginid,
		"userid":                a.Userid,
		"enabled":               a.Enabled,
		"has_password":          a.Password != "",
		"session":               masked,
		"session_days_left":     daysLeft,
		"expire_at":             a.ExpireAt,
		"balance":               accountOptInt(a.Balance),
		"daily_balance":         accountOptInt(a.DailyBalance),
		"available":             accountOptInt(a.Available),
		"quota_updated_at":      a.QuotaUpdatedAt,
		"requests":              a.Requests,
		"points_used":           a.PointsUsed,
		"failures":              a.Failures,
		"in_cooldown":           a.InCooldown(),
		"cooldown_seconds_left": cooldownLeft,
		"source":                a.Source,
		"last_error":            a.LastError,
		"identity":              a.IdentityView(),
	}
}

// accountPrefix 对应 Python 的 s[:n]（按码点切，不按字节）。
func accountPrefix(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// accountRound 对应 Python round(value, digits)。
// Python 用银行家舍入，这里用 math.Round（半值远离零）；两者只在恰好半值时不同。
func accountRound(value float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(value*scale) / scale
}

// accountOptInt 把 *int64 展开成 any：nil → nil（JSON null / Python None），否则整数值。
func accountOptInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// accountVal 对应 Python 的 str(payload.get(key) or "")：
// nil / "" / 0 / false 一律当空串（Go 的 jstr 会把 0 变成 "0"，语义不同）。
func accountVal(m map[string]any, key string) string {
	if !anthTruthy(m[key]) {
		return ""
	}
	return jstr(m, key)
}

// pyInt64 对应 Python 的 int(v)：数字截断取整，字符串按十进制解析（允许前后空白）。
// 缺失、null 或无法解析时返回 0。
func pyInt64(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case float32:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
		return 0
	case string:
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f)
		}
		return 0
	default:
		return 0
	}
}

// ---------------------------------------------------------- AccountClientAPI

// AccountClientAPI 是账号池对 AccountClient 的最小依赖（pool 里 Client 字段的类型）。
// SendSMSCode / LoginBySMS 对应 Python 面板短信登录两步（见 panel.go 的 SendSMS / VerifySMS）。
type AccountClientAPI interface {
	IdentityMode() string
	EnsureIdentity(acc *Account) map[string]any
	RebindIdentity(acc *Account) map[string]any
	Fill(acc *Account) error
	SendSMSCode(phone string, identity map[string]any) (map[string]any, error)
	LoginBySMS(phone, code, msgid string, identity map[string]any) (map[string]string, error)
}

// ------------------------------------------------------------ AccountClient

// AccountClient 代表一个账号访问 account_base（登录 + userinfo）。
type AccountClient struct {
	cfg *Config
}

// 编译期确认 AccountClient 满足账号池的依赖接口。
var _ AccountClientAPI = (*AccountClient)(nil)

// NewAccountClient 对应 Python AccountClient(cfg)。
func NewAccountClient(cfg *Config) *AccountClient {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	return &AccountClient{cfg: cfg}
}

// IdentityMode 对应 Python identity_mode：未配置时按 per_account。
func (c *AccountClient) IdentityMode() string {
	if c.cfg.IdentityMode == "" {
		return "per_account"
	}
	return c.cfg.IdentityMode
}

// base 对应 Python _base：账号服务请求信封。
// identity 为空时按 identity_mode 现场生成一个；traceid 每次都重新生成
// （真客户端每个请求都换，绝不复用）。
func (c *AccountClient) base(identity map[string]any) map[string]string {
	ident := identity
	if len(ident) == 0 {
		ident = NewIdentity(c.IdentityMode())
	}
	appid := c.cfg.AccountAppID
	if appid == "" {
		appid = DefaultAccountAppID
	}
	return map[string]string{
		"appid":   appid,
		"modelid": accountOr(ident, "modelid", WebModelID),
		"version": accountOr(ident, "version", ClientVersion),
		"devid":   accountOr(ident, "devid", DeviceID),
		"ua":      accountOr(ident, "ua", ClientUA),
		"traceid": accountHex(32),
	}
}

// accountOr 对应 Python 的 str(ident.get(key) or default)。
func accountOr(ident map[string]any, key, def string) string {
	if v := jstr(ident, key); v != "" {
		return v
	}
	return def
}

// EnsureIdentity 对应 Python ensure_identity：没有 identity 时补一个，之后一直复用。
func (c *AccountClient) EnsureIdentity(acc *Account) map[string]any {
	if len(acc.Identity) == 0 {
		acc.Identity = NewIdentity(c.IdentityMode())
	}
	return acc.Identity
}

// RebindIdentity 对应 Python rebind_identity：换一个全新的 identity（新的「设备」）。
func (c *AccountClient) RebindIdentity(acc *Account) map[string]any {
	acc.Identity = NewIdentity(c.IdentityMode())
	return acc.Identity
}

// Call 调用账号服务的一个 path。
//
// body 为空时不发送请求体（对应 Python 的 `if body else ""`）；签名头用
// cfg.AKID()/cfg.AKSecret() 现算；URL = account_base 去尾斜杠 + path。
// 业务错误 / HTTP 错误 / 网络错误分别映射成不同的 AccountError（见 Python call 的分支）。
//
// identity 与 Python 版一样不参与请求（identity 只通过 body 里的 base 进入），
// 保留这个参数是为了和移植来源的调用点一一对应。
func (c *AccountClient) Call(path string, body map[string]any, identity map[string]any, timeout time.Duration) (map[string]any, error) {
	bodyStr := ""
	if len(body) > 0 {
		text, err := jsonDumps(body)
		if err != nil {
			return nil, err
		}
		bodyStr = text
	}
	headers := BuildHeaders(c.cfg.AKID(), c.cfg.AKSecret(), "POST", path, bodyStr, nil, "", nil)

	url := strings.TrimRight(c.cfg.AccountBase, "/") + path
	var payload []byte
	if bodyStr != "" {
		payload = []byte(bodyStr)
	}

	status, _, data, err := Request(url, "POST", headers, payload, accountTimeout(timeout), c.cfg.Proxy)
	if err != nil {
		return nil, &AccountError{
			Msg:       "account service unreachable: " + err.Error(),
			Code:      "NETWORK",
			Retryable: true,
		}
	}

	out := map[string]any{}
	if jsonErr := json.Unmarshal(data, &out); jsonErr != nil {
		return nil, &AccountError{
			Msg:       fmt.Sprintf("account service returned HTTP %d: %s", status, httpcText(data, 200)),
			Code:      fmt.Sprintf("HTTP_%d", status),
			Retryable: status >= 500,
		}
	}

	code := accountVal(out, "code")
	if code == "" {
		code = accountVal(out, "errorCode")
	}
	if code != "" && code != "000000" {
		msg := accountVal(out, "desc")
		if msg == "" {
			msg = accountVal(out, "message")
		}
		if msg == "" {
			msg = code
		}
		return nil, &AccountError{
			Msg:       path + " failed: " + msg,
			Code:      code,
			Retryable: code == "100001" || code == "100002",
		}
	}
	if status >= 400 {
		if _, hasMessage := out["message"]; hasMessage {
			return nil, &AccountError{
				Msg:       fmt.Sprintf("%s HTTP %d: %s", path, status, jstr(out, "message")),
				Code:      fmt.Sprintf("HTTP_%d", status),
				Retryable: status >= 500,
			}
		}
	}
	return out, nil
}

// accountTimeout 对应 Python call(timeout=30.0) 的默认值：非正数取 30 秒。
func accountTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 30 * time.Second
	}
	return timeout
}

// GetPublicKey 对应 Python get_public_key：→ (pukey, rcode)。零副作用，适合当探针。
func (c *AccountClient) GetPublicKey(identity map[string]any) (string, string, error) {
	payload, err := c.Call("/login/account/getPuKey", map[string]any{"base": c.base(identity)}, identity, 0)
	if err != nil {
		return "", "", err
	}
	data := jmap(payload, "data")
	pukey := jstr(data, "pukey")
	rcode := jstr(data, "rcode")
	if pukey == "" {
		return "", "", &AccountError{Msg: "getPuKey returned no public key", Code: "NO_PUKEY"}
	}
	return pukey, rcode, nil
}

// LoginByPassword 对应 Python login_by_password：完整密码登录 → {session, userid, phone}。
func (c *AccountClient) LoginByPassword(loginid, password string, identity map[string]any) (map[string]string, error) {
	pukey, rcode, err := c.GetPublicKey(identity)
	if err != nil {
		return nil, err
	}
	encrypted, err := RsaEncryptPKCS1v15(pukey, password, nil)
	if err != nil {
		return nil, err
	}
	payload, err := c.Call("/login/account/byPwd", map[string]any{
		"base": c.base(identity),
		"param": map[string]any{
			"loginid":  loginid,
			"password": encrypted,
			"rcode":    rcode,
			"type":     1,
			"expire":   SessionExpireSeconds,
		},
	}, identity, 0)
	if err != nil {
		return nil, err
	}
	return accountExtractSession(payload)
}

// SendSMSCode 对应 Python send_sms_code。
func (c *AccountClient) SendSMSCode(phone string, identity map[string]any) (map[string]any, error) {
	return c.Call("/login/phone/sendMsgCode", map[string]any{
		"base": c.base(identity),
		"param": map[string]any{
			"ccode":  "86",
			"phone":  phone,
			"expire": 300,
		},
	}, identity, 0)
}

// LoginBySMS 对应 Python login_by_sms → {session, userid, phone}。
func (c *AccountClient) LoginBySMS(phone, code, msgid string, identity map[string]any) (map[string]string, error) {
	payload, err := c.Call("/login/phone/checkCode", map[string]any{
		"base": c.base(identity),
		"param": map[string]any{
			"ccode":  "86",
			"phone":  phone,
			"mcode":  code,
			"msgid":  msgid,
			"expire": SessionExpireSeconds,
		},
	}, identity, 0)
	if err != nil {
		return nil, err
	}
	return accountExtractSession(payload)
}

// QueryBaseInfo 对应 Python query_base_info。
func (c *AccountClient) QueryBaseInfo(session string, identity map[string]any) (map[string]any, error) {
	return c.Call("/userinfo/query/baseInfo", map[string]any{
		"base":  c.base(identity),
		"param": map[string]any{"session": session},
	}, identity, 0)
}

// Logout 对应 Python logout。
func (c *AccountClient) Logout(session string, identity map[string]any) (map[string]any, error) {
	return c.Call("/login/account/logout", map[string]any{
		"base":  c.base(identity),
		"param": map[string]any{"session": session},
	}, identity, 0)
}

// Fill 对应 Python fill：用密码登录并刷新账号的会话字段。
// 同时把账号自己的 identity 绑定下来——首次登录创建，之后每个请求都复用它，
// 这样账号在服务端看起来始终是同一台设备。
func (c *AccountClient) Fill(acc *Account) error {
	if acc.Loginid == "" || acc.Password == "" {
		return &AccountError{
			Msg:  fmt.Sprintf("account %s: loginid/password missing", acc.Name),
			Code: "NO_CREDENTIALS",
		}
	}
	identity := c.EnsureIdentity(acc)
	got, err := c.LoginByPassword(acc.Loginid, acc.Password, identity)
	if err != nil {
		return err
	}
	acc.Session = got["session"]
	acc.Userid = got["userid"]
	acc.ObtainedAt = time.Now().Unix()
	acc.ExpireAt = acc.ObtainedAt + SessionExpireSeconds
	return nil
}

// accountExtractSession 对应 Python _extract_session：把 session/userid/phone
// 从服务端返回的各种形状里挖出来（data 是对象就下潜，否则用 payload 本身）。
func accountExtractSession(payload map[string]any) (map[string]string, error) {
	node := payload
	if data, isMap := payload["data"].(map[string]any); isMap {
		node = data
	}
	session := jstr(node, "session")
	if session == "" {
		return nil, &AccountError{
			Msg: fmt.Sprintf("login succeeded but returned no session: %v", payload),
		}
	}
	userid := jstr(node, "userid")
	if userid == "" {
		userid = jstr(node, "userId")
	}
	phone := jstr(node, "phone")
	if phone == "" {
		phone = jstr(node, "loginid")
	}
	return map[string]string{"session": session, "userid": userid, "phone": phone}, nil
}
