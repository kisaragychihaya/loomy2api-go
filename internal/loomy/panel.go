package loomy

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
)

// 对应 Python loomy2api/panel.py —— Web 控制面板的 JSON API 门面。
//
// 页面本身是一张自包含的 HTML（embed.go 里 //go:embed 进来的 web/index.html），由
// server 在 /panel 上发出；这里的 Panel 只负责 server 处理函数要调用的状态查询与账号
// 写入，不关心 HTTP 细节（状态码映射在 server 层）。
//
// 与 Python 的差异：Python 的 Panel 持有一个 gateway 对象，从中取 cfg / pool / log /
// catalogue；这里改为构造时显式注入，测试可以直接装配。Python 的 gateway.log 既是可
// 调用的日志函数、又带 .path 属性，Go 把它们拆成 LogFunc 与 logFile 两个参数。

// Panel 对应 Python 的 Panel：账号池之上的面板门面。
type Panel struct {
	cfg       *Config
	pool      *Pool
	log       LogFunc
	logFile   string
	catalogue func() []map[string]any
}

// NewPanel 对应 Python Panel(gateway) 的构造。catalogue 是 server 的模型目录
// （State 的 models 计数用它）；为 nil 时按空目录处理。
func NewPanel(cfg *Config, pool *Pool, log LogFunc, logFile string, catalogue func() []map[string]any) *Panel {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if log == nil {
		log = func(string) {}
	}
	if catalogue == nil {
		catalogue = func() []map[string]any { return nil }
	}
	return &Panel{cfg: cfg, pool: pool, log: log, logFile: logFile, catalogue: catalogue}
}

// HTML 对应 Python 的 html：面板页面。Python 读文件失败时有兜底页，这里内容由
// go:embed 编进二进制，不存在读失败。
func (p *Panel) HTML() []byte {
	return panelHTML
}

// Docs 返回内嵌使用手册的 HTML 片段（web/docs.html 经 go:embed 编译进来）。
// 片段本身不含 html/head/body，前端直接注入 #docsBody；内容是随二进制发布的可信
// markup，所以不需要再做转义或清洗。
func (p *Panel) Docs() map[string]any {
	return map[string]any{"ok": true, "html": string(panelDocsHTML)}
}

// maskPhone 对应 Python 的 mask_phone：只留数字；不足 7 位原样返回，
// 否则「前三位****后四位」（138****0000）。
func maskPhone(value string) string {
	digits := make([]rune, 0, len(value))
	for _, ch := range value {
		if unicode.IsDigit(ch) {
			digits = append(digits, ch)
		}
	}
	if len(digits) < 7 {
		return value
	}
	return string(digits[:3]) + "****" + string(digits[len(digits)-4:])
}

// accountView 对应 Python 的 _account_view：public_dict 加上面板专用字段。
func (p *Panel) accountView(acc *Account) map[string]any {
	view := acc.PublicDict()
	view["loginid_masked"] = maskPhone(acc.Loginid)
	view["points_used"] = acc.PointsUsed
	mode := "empty"
	if acc.Loginid != "" && acc.Password != "" {
		mode = "password"
	} else if acc.Session != "" {
		mode = "session"
	}
	view["mode"] = mode
	return view
}

// quotaStale 对应 Python 的 _quota_stale：额度缓存超过 quota_refresh_minutes 就算过期
// （配置为 0 时按 30 分钟）。QuotaUpdatedAt==0 表示从没刷过，必然过期。
func (p *Panel) quotaStale(acc *Account) bool {
	minutes := p.cfg.QuotaRefreshMinutes
	if minutes == 0 {
		minutes = 30
	}
	return accountNow()-float64(acc.QuotaUpdatedAt) > minutes*60
}

// refreshAll 对应 Python 的 _refresh_all：刷新会话有效账号的额度，末尾落盘。
// RefreshQuota 自身永不失败（失败降级成「额度未知」并记日志），所以这里没有 try。
func (p *Panel) refreshAll(onlyStale bool) {
	for _, acc := range p.pool.Accounts() {
		if !acc.SessionValid() {
			continue
		}
		if onlyStale && !p.quotaStale(acc) {
			continue
		}
		p.pool.RefreshQuota(acc)
	}
	p.pool.Save(false)
}

// State 对应 Python 的 state：账号 + 额度 + 汇总 + 配置。
// refresh 为 true 时无视缓存强制刷新额度。
func (p *Panel) State(refresh bool) map[string]any {
	p.refreshAll(!refresh)

	accounts := p.pool.Accounts()
	views := make([]any, 0, len(accounts))
	requests, pointsUsed := 0, 0
	for _, acc := range accounts {
		views = append(views, p.accountView(acc))
		requests += acc.Requests
		pointsUsed += acc.PointsUsed
	}

	// 一个账号可能同时有我们和客户端的会话 —— 按 userid（缺省用 name）去重，
	// 每个账号只算一次额度。
	seen := make(map[string]int64, len(accounts))
	for _, acc := range accounts {
		if acc.Available == nil {
			continue
		}
		key := acc.Userid
		if key == "" {
			key = acc.Name
		}
		if cur, ok := seen[key]; !ok || *acc.Available > cur {
			seen[key] = *acc.Available
		}
	}
	var available int64
	for _, value := range seen {
		available += value
	}

	return map[string]any{
		"ok":  true,
		"now": time.Now().Unix(),
		"totals": map[string]any{
			"accounts":        len(views),
			"usable":          len(p.pool.Usable()),
			"available":       available,
			"unique_accounts": len(seen),
			"requests":        requests,
			"points_used":     pointsUsed,
		},
		"config": map[string]any{
			"upstream":              p.cfg.Upstream,
			"strategy":              p.cfg.Strategy,
			"default_model":         p.cfg.DefaultModel,
			"identity_mode":         p.cfg.IdentityMode,
			"models":                len(p.catalogue()),
			"auth_required":         len(p.cfg.APIKeys) > 0,
			"quota_refresh_minutes": p.cfg.QuotaRefreshMinutes,
		},
		"accounts": views,
	}
}

// Models 返回模型目录（面板「模型」页用）。目录来自构造时注入的回调，与 State 里
// 的 models 计数同源；catalogue 为 nil（或返回 nil 切片）时按空目录处理。
func (p *Panel) Models() map[string]any {
	models := []map[string]any{}
	if p.catalogue != nil {
		if got := p.catalogue(); got != nil {
			models = got
		}
	}
	return map[string]any{
		"ok":            true,
		"now":           time.Now().Unix(),
		"upstream":      p.cfg.Upstream,
		"default_model": p.cfg.DefaultModel,
		"count":         len(models),
		"models":        models,
	}
}

// Refresh 对应 Python 的 refresh：强制刷新每个账号的额度，返回刷新后的状态。
func (p *Panel) Refresh(payload map[string]any) (map[string]any, error) {
	p.refreshAll(false)
	return p.State(false), nil
}

// pnlLoginRequested 对应 Python 的 payload.get("login", True)：缺省视为要登录。
func pnlLoginRequested(payload map[string]any) bool {
	v, ok := payload["login"]
	if !ok {
		return true
	}
	return anthTruthy(v)
}

// pnlKnownError 判断 err 是否属于 Python 里被 except 的 (AccountError, PoolError)。
// Python 只捕获这两类，其余异常继续上抛；Go 侧保持一致。
func pnlKnownError(err error) bool {
	var perr *PoolError
	var aerr *AccountError
	return errors.As(err, &perr) || errors.As(err, &aerr)
}

// AddAccount 对应 Python 的 add_account：（可选）立刻用密码登录并刷新额度。
func (p *Panel) AddAccount(payload map[string]any) (map[string]any, error) {
	name := strings.TrimSpace(accountVal(payload, "name"))
	loginid := strings.TrimSpace(accountVal(payload, "loginid"))
	password := accountVal(payload, "password")
	session := strings.TrimSpace(accountVal(payload, "session"))

	if name == "" {
		return nil, &PoolError{Msg: "账号名不能为空 / name is required"}
	}
	if loginid == "" && session == "" {
		return nil, &PoolError{Msg: "需要手机号和密码，或直接提供一个 session / " +
			"provide phone+password, or a session"}
	}
	if loginid != "" && password == "" && session == "" {
		return nil, &PoolError{Msg: "只给手机号时还需要密码 / password required with phone"}
	}

	acc, err := p.pool.AddAccount(name, loginid, password, session, "", nil)
	if err != nil {
		return nil, err
	}

	result := map[string]any{"ok": true, "name": acc.Name, "identity": acc.IdentityView()}
	if loginid != "" && password != "" && pnlLoginRequested(payload) {
		if serr := p.pool.EnsureSession(acc, true); serr != nil {
			if !pnlKnownError(serr) {
				return nil, serr
			}
			result["logged_in"] = false
			result["error"] = serr.Error()
		} else {
			p.pool.RefreshQuota(acc)
			result["logged_in"] = true
			result["userid"] = acc.Userid
		}
		p.pool.Save(true)
	}
	p.pool.Save(true)
	result["state"] = p.State(false)
	return result, nil
}

// UpdateAccount 对应 Python 的 update_account：修补 loginid / password / enabled，
// 换了密码且要求登录时立刻重登。
func (p *Panel) UpdateAccount(payload map[string]any) (map[string]any, error) {
	name := strings.TrimSpace(accountVal(payload, "name"))

	fields := map[string]any{}
	if v, ok := payload["loginid"]; ok {
		fields["loginid"] = v
	}
	if anthTruthy(payload["password"]) {
		fields["password"] = payload["password"]
	}
	if v, ok := payload["enabled"]; ok {
		fields["enabled"] = anthTruthy(v)
	}

	acc, err := p.pool.UpdateAccount(name, fields)
	if err != nil {
		return nil, err
	}

	if anthTruthy(payload["login"]) && acc.Loginid != "" && acc.Password != "" {
		if serr := p.pool.EnsureSession(acc, true); serr != nil {
			if !pnlKnownError(serr) {
				return nil, serr
			}
			p.log(fmt.Sprintf("[panel] %s 重新登录失败：%v", name, serr))
		} else {
			p.pool.RefreshQuota(acc)
		}
		p.pool.Save(true)
	}
	return map[string]any{"ok": true, "name": acc.Name, "state": p.State(false)}, nil
}

// RemoveAccount 对应 Python 的 remove_account：按名字删账号（不存在是错误）。
func (p *Panel) RemoveAccount(payload map[string]any) (map[string]any, error) {
	name := strings.TrimSpace(accountVal(payload, "name"))
	if !p.pool.RemoveAccount(name) {
		return nil, &PoolError{Msg: fmt.Sprintf("没有名为 %s 的账号 / no such account: %s", name, name)}
	}
	p.log(fmt.Sprintf("[panel] 已删除账号 %s", name))
	return map[string]any{"ok": true, "removed": name, "state": p.State(false)}, nil
}

// RenewAccount 对应 Python 的 renew_account：强制重登 + 刷新额度 + 解除冷却。
func (p *Panel) RenewAccount(payload map[string]any) (map[string]any, error) {
	name := strings.TrimSpace(accountVal(payload, "name"))
	acc := p.pool.Get(name)
	if acc == nil {
		return nil, &PoolError{Msg: fmt.Sprintf("没有名为 %s 的账号 / no such account: %s", name, name)}
	}

	result := map[string]any{"ok": true, "name": name}
	if serr := p.pool.EnsureSession(acc, true); serr != nil {
		if !pnlKnownError(serr) {
			return nil, serr
		}
		result["ok"] = false
		result["error"] = serr.Error()
	} else {
		p.pool.RefreshQuota(acc)
		acc.CooldownUntil = 0
		days, _ := acc.DaysLeft()
		result["userid"] = acc.Userid
		result["session_days_left"] = days
	}
	p.pool.Save(true)
	result["state"] = p.State(false)
	return result, nil
}

// Identity 对应 Python 的 identity：检查（regenerate 为假）或重新绑定（为真）设备标识。
func (p *Panel) Identity(payload map[string]any) (map[string]any, error) {
	name := strings.TrimSpace(accountVal(payload, "name"))
	acc := p.pool.Get(name)
	if acc == nil {
		return nil, &PoolError{Msg: fmt.Sprintf("没有名为 %s 的账号 / no such account: %s", name, name)}
	}

	if anthTruthy(payload["regenerate"]) {
		if _, err := p.pool.RebindIdentity(name); err != nil {
			return nil, err
		}
		p.log(fmt.Sprintf("[panel] %s 已重新绑定设备标识 devid=%s",
			name, upStr(acc.Identity["devid"])))
	} else if len(acc.Identity) == 0 {
		p.pool.Client.EnsureIdentity(acc)
		p.pool.Save(true)
	}
	return map[string]any{
		"ok":       true,
		"name":     name,
		"identity": acc.IdentityView(),
		"state":    p.State(false),
	}, nil
}

// Logs 对应 Python 的 logs：读日志文件的尾部 N 行。
// N 被夹在 10..2000 之间（Python 的 max(10, min(lines or 200, 2000))）。
func (p *Panel) Logs(lines int) map[string]any {
	limit := lines
	if limit == 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	if limit < 10 {
		limit = 10
	}

	content := []string{}
	if data, err := os.ReadFile(p.logFile); err == nil {
		content = pnlSplitLines(string(data))
	}
	if len(content) > limit {
		content = content[len(content)-limit:]
	}
	return map[string]any{"ok": true, "path": p.logFile, "lines": content}
}

// pnlSplitLines 对应 Python 的 str.splitlines（只处理 \r\n / \r / \n 三种换行；
// Python 还认 \v、\f 等少数控制符，日志文件里不会出现）。
func pnlSplitLines(text string) []string {
	if text == "" {
		return []string{}
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	out := strings.Split(normalized, "\n")
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
