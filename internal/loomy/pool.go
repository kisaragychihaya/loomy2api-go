package loomy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 对应 Python loomy2api/pool.py —— 多账号池：会话生命周期、额度跟踪、轮换。
//
// 一个账号 = 一个 Loomy/讯飞账号 = 一个 14 天会话。会话在到期前用密码自动续期，
// 所以一个账号池可以无人值守地一直用下去。
//
// 选号策略（Cfg.Strategy）：
//
//	balance      可用积分最多的账号（默认）
//	round_robin  按顺序轮转
//	lru          最久未使用的账号
//
// 上游回 401/403 或提示积分耗尽的账号会被丢进冷却期，冷却结束前跳过，网关随后
// 换下一个账号重试。账号池状态（会话、额度）原子地写回 accounts.json —— 密码也在
// 同一个文件里，注意不要提交进 git。
//
// 与 Python 的差异：Python 用一个 RLock 同时保护账号列表和 save 节流状态，这里用
// 一个 RWMutex 承担同样的职责；keeper 用 chan + goroutine 代替 threading.Event +
// Thread。Save 的写盘错误无法上抛（签名不带 error），改为记日志。

// PoolError 对应 Python 的 PoolError：账号池层面的可预期错误（配置/凭据/文件）。
type PoolError struct {
	Msg string
}

func (e *PoolError) Error() string {
	return e.Msg
}

// Pool 对应 Python 的 AccountPool。
type Pool struct {
	Cfg     *Config
	Log     LogFunc
	Client  AccountClientAPI // 可注入 fake（测试用）
	Gateway GatewayAPI       // 可注入 fake（测试用）

	mu       sync.RWMutex // 保护 accounts / lastSave / dirty（对应 Python 的 _lock）
	accounts []*Account

	rrMu    sync.Mutex // 对应 Python 的 _rr_lock
	rrIndex int

	keeperMu      sync.Mutex
	keeperRunning bool
	stopCh        chan struct{}

	file     string
	lastSave float64
	dirty    bool
}

// NewPool 对应 Python AccountPool(cfg, logger)：构造即 Load。
// 账号文件解析失败返回 *PoolError（Pool 为 nil，不要使用）。
func NewPool(cfg *Config, log LogFunc) (*Pool, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if log == nil {
		log = func(string) {}
	}
	file := cfg.AccountsFile
	if file == "" {
		file = "accounts.json"
	}
	p := &Pool{
		Cfg:     cfg,
		Log:     log,
		Client:  NewAccountClient(cfg),
		Gateway: NewModelGateway(cfg),
		file:    cfg.Path(file),
		stopCh:  make(chan struct{}),
	}
	if err := p.Load(); err != nil {
		return nil, err
	}
	return p, nil
}

// ---------------------------------------------------------------- 存储

// Load 对应 Python load：读账号文件，重建账号列表并保留同名账号的运行期统计。
//
// 文件不存在按空池处理（不是错误）；解析失败返回 *PoolError。既接受
// {"accounts":[...]} 也接受裸数组。SessionsFromClient 时追加桌面客户端导入的
// 只读账号。
func (p *Pool) Load() error {
	var raw any
	data, err := os.ReadFile(p.file)
	if err == nil {
		if jerr := json.Unmarshal(data, &raw); jerr != nil {
			return &PoolError{Msg: fmt.Sprintf("%s 解析失败 / parse error: %v", p.file, jerr)}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		// Python 把 read_text 也包在同一个 try 里，读失败同样是「解析失败」。
		return &PoolError{Msg: fmt.Sprintf("%s 解析失败 / parse error: %v", p.file, err)}
	}

	var items []any
	if obj, isObj := raw.(map[string]any); isObj {
		items = jlist(obj, "accounts")
	} else if arr, isList := raw.([]any); isList {
		items = arr
	}

	accounts := []*Account{}
	for idx, item := range items {
		if m, isMap := item.(map[string]any); isMap {
			accounts = append(accounts, NewAccountFromDict(m, idx))
		}
	}

	if p.Cfg.SessionsFromClient {
		accounts = append(accounts, p.clientAccounts(accounts)...)
	}

	p.mu.Lock()
	// 同名账号保留运行期统计，避免一次重载把计数清零。
	previous := make(map[string]*Account, len(p.accounts))
	for _, a := range p.accounts {
		previous[a.Name] = a
	}
	for _, acc := range accounts {
		old, ok := previous[acc.Name]
		if !ok {
			continue
		}
		acc.Requests, acc.PointsUsed, acc.Failures = old.Requests, old.PointsUsed, old.Failures
		acc.LastUsed, acc.LastError = old.LastUsed, old.LastError
		if acc.ExpireAt == 0 && old.ExpireAt != 0 {
			acc.Session, acc.ExpireAt, acc.Userid = old.Session, old.ExpireAt, old.Userid
		}
	}
	p.accounts = accounts
	p.mu.Unlock()

	msg := fmt.Sprintf("账号池载入 %d 个账号", len(accounts))
	if len(accounts) > 0 {
		names := make([]string, 0, len(accounts))
		for _, a := range accounts {
			names = append(names, a.Name)
		}
		msg += "（" + strings.Join(names, ", ") + "）"
	}
	p.Log(msg)
	return nil
}

// clientAccounts 对应 Python _client_accounts：从已安装的桌面客户端导入登录态。
//
// 这些是派生账号：永远不写回 accounts.json（那个文件归客户端所有），并且在
// session / userid / name 任一维度与已配置账号撞车时跳过。
func (p *Pool) clientAccounts(existing []*Account) []*Account {
	root := p.Cfg.ClientRoot
	if root == "" {
		root = ClientPublicRoot
	}
	pattern := filepath.Join(root, "*", "userData", "auth-session.json")

	found := []*Account{}
	knownSessions := map[string]bool{}
	knownNames := map[string]bool{}
	// 同一个账号经两条会话（客户端的和我们自己的）到达时不能变成两条池记录：
	// 按 userid 去重。
	knownUsers := map[string]bool{}
	for _, a := range existing {
		if a.Session != "" {
			knownSessions[a.Session] = true
		}
		knownNames[a.Name] = true
		if a.Userid != "" {
			knownUsers[a.Userid] = true
		}
	}

	paths, _ := filepath.Glob(pattern)
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var raw map[string]any
		if jerr := json.Unmarshal(data, &raw); jerr != nil {
			continue
		}
		session := accountVal(raw, "session")
		userid := accountVal(raw, "userid")
		if session == "" || knownSessions[session] {
			continue
		}
		if userid != "" && knownUsers[userid] {
			continue
		}
		phone := accountVal(raw, "phone")
		label := phone
		if label == "" {
			label = "unknown"
		}
		name := "desktop-" + poolTail(label, 4)
		if knownNames[name] {
			continue
		}
		created := pyInt64(raw["updatedAt"])
		// 客户端存的是毫秒时间戳；会话有效期 14 天。
		expire := int64(0)
		if created != 0 {
			expire = created/1000 + SessionExpireSeconds
		}

		acc := NewAccount(name)
		acc.Loginid = phone
		acc.Session = session
		acc.Userid = userid
		acc.ExpireAt = expire
		acc.ObtainedAt = created / 1000
		acc.Source = "client"
		acc.Persist = false

		found = append(found, acc)
		knownSessions[session] = true
		knownNames[name] = true
		if userid != "" {
			knownUsers[userid] = true
		}
		p.Log("发现桌面客户端登录态：" + path)
	}
	return found
}

// Save 对应 Python save：把账号池写回 accounts.json（原子替换）。
//
// 非 force 且距上次落盘不足 5 秒时只置 dirty 返回（对应 Python 的节流）。
// payload 在锁内组装、文件在锁外写 —— 与 Python 的 `with self._lock` 只包住
// payload 构建一致，避免慢磁盘拖住整个账号池。
// 写盘失败没有返回值可上抛（Python 会抛异常），这里记日志。
func (p *Pool) Save(force bool) {
	now := accountNow()

	p.mu.Lock()
	if !force && (now-p.lastSave) < 5 {
		p.dirty = true
		p.mu.Unlock()
		return
	}
	p.dirty = false
	p.lastSave = now
	items := make([]any, 0, len(p.accounts))
	for _, a := range p.accounts {
		if a.Persist {
			items = append(items, a.ToDict())
		}
	}
	payload := map[string]any{"accounts": items}
	p.mu.Unlock()

	if dir := filepath.Dir(p.file); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			p.Log("账号文件目录创建失败：" + err.Error())
			return
		}
	}

	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // 与 Python 的 ensure_ascii=False 一致
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		p.Log("账号文件序列化失败：" + err.Error())
		return
	}
	text := strings.TrimRight(buf.String(), "\n") // Python 的 write_text 不带结尾换行

	tmp := p.file + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		p.Log("账号文件写入失败：" + err.Error())
		return
	}
	if err := os.Rename(tmp, p.file); err != nil {
		p.Log("账号文件替换失败：" + err.Error())
	}
}

// Flush 对应 Python flush：有被节流挂起的写入时强制落盘。
func (p *Pool) Flush() {
	p.mu.RLock()
	dirty := p.dirty
	p.mu.RUnlock()
	if dirty {
		p.Save(true)
	}
}

// ---------------------------------------------------------------- 访问

// Accounts 对应 Python 的 accounts property：返回切片拷贝（Python 的 list()）。
func (p *Pool) Accounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

// Get 对应 Python get：按名字线性查找，找不到返回 nil。
func (p *Pool) Get(name string) *Account {
	for _, acc := range p.Accounts() {
		if acc.Name == name {
			return acc
		}
	}
	return nil
}

// AddAccount 对应 Python add_account：新建账号并落盘。
//
// identity 为 nil 时向账号客户端要一个，这样账号从第一个请求起就像同一台设备。
func (p *Pool) AddAccount(name, loginid, password, session, userid string, identity map[string]any) (*Account, error) {
	if p.Get(name) != nil {
		return nil, &PoolError{Msg: fmt.Sprintf("账号 %s 已存在 / account already exists", name)}
	}
	acc := NewAccount(name)
	acc.Loginid = loginid
	acc.Password = password
	acc.Session = session
	acc.Userid = userid
	if len(identity) > 0 {
		bound := make(map[string]any, len(identity))
		for k, v := range identity {
			bound[k] = v
		}
		acc.Identity = bound
	} else {
		acc.Identity = p.Client.EnsureIdentity(acc)
	}
	if session != "" && acc.ExpireAt == 0 {
		acc.ObtainedAt = time.Now().Unix()
		acc.ExpireAt = acc.ObtainedAt + SessionExpireSeconds
	}

	p.mu.Lock()
	p.accounts = append(p.accounts, acc)
	p.mu.Unlock()

	p.Save(true)
	return acc, nil
}

// UpdateAccount 对应 Python update_account：修补 loginid / password / enabled。
// 换密码意味着旧会话属于旧登录，必须清掉它，下次使用时重新登录。
func (p *Pool) UpdateAccount(name string, fields map[string]any) (*Account, error) {
	acc := p.Get(name)
	if acc == nil {
		return nil, &PoolError{Msg: fmt.Sprintf("没有名为 %s 的账号 / no such account: %s", name, name)}
	}
	for _, key := range []string{"loginid", "password"} {
		v, ok := fields[key]
		if !ok || v == nil {
			continue
		}
		if key == "loginid" {
			acc.Loginid = poolStr(v)
		} else {
			acc.Password = poolStr(v)
			acc.Session, acc.ExpireAt = "", 0
		}
	}
	if v, ok := fields["enabled"]; ok && v != nil {
		acc.Enabled = anthTruthy(v) // 对应 Python 的 bool(v)
	}
	p.Save(true)
	return acc, nil
}

// RebindIdentity 对应 Python rebind_identity：换一个全新的设备 identity，
// 有账号密码时顺带强制重登。
func (p *Pool) RebindIdentity(name string) (*Account, error) {
	acc := p.Get(name)
	if acc == nil {
		return nil, &PoolError{Msg: fmt.Sprintf("没有名为 %s 的账号 / no such account: %s", name, name)}
	}
	p.Client.RebindIdentity(acc)
	if acc.Loginid != "" && acc.Password != "" {
		if err := p.EnsureSession(acc, true); err != nil {
			return nil, err
		}
	}
	p.Save(true)
	return acc, nil
}

// RemoveAccount 对应 Python remove_account：按名字过滤掉账号，返回是否有删除。
func (p *Pool) RemoveAccount(name string) bool {
	p.mu.Lock()
	kept := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if a.Name != name {
			kept = append(kept, a)
		}
	}
	removed := len(kept) != len(p.accounts)
	p.accounts = kept
	p.mu.Unlock()

	if removed {
		p.Save(true)
	}
	return removed
}

// ------------------------------------------------------------ 会话维护

// EnsureSession 对应 Python ensure_session：会话不可用或快到期时用密码重登。
// force 为 true 时无条件重登。
func (p *Pool) EnsureSession(acc *Account, force bool) error {
	if acc == nil {
		// Python 这里会抛 AttributeError；Go 显式返回错误，免得一个坏指针打挂 keeper。
		return &PoolError{Msg: "账号对象为空 / account is nil"}
	}

	renewDays := p.Cfg.SessionRenewBeforeDays
	if renewDays == 0 {
		renewDays = 3
	}
	needs := force || !acc.SessionValid()
	if !needs {
		if days, known := acc.DaysLeft(); known && days < renewDays {
			needs = true
			p.Log(fmt.Sprintf("账号 %s session 剩余 %.1f 天，提前续期", acc.Name, days))
		}
	}
	if !needs {
		return nil
	}
	if acc.Loginid == "" || acc.Password == "" {
		return &PoolError{Msg: fmt.Sprintf(
			"账号 %s 的 session 不可用且没有账号密码，无法自动重登"+
				"（请在 accounts.json 补 loginid/password，或用 `loomy2api login` 短信登录）", acc.Name)}
	}

	if err := p.Client.Fill(acc); err != nil {
		return err
	}
	p.Save(false)

	days, _ := acc.DaysLeft()
	p.Log(fmt.Sprintf("账号 %s 登录成功 userid=%s session=%s… 剩余 %.1f 天",
		acc.Name, acc.Userid, accountPrefix(acc.Session, 8), days))
	return nil
}

// RefreshQuota 对应 Python refresh_quota：刷新单账号积分，永不返回错误。
//
// 它会从面板、CLI 和后台 keeper 三个地方被调用，三者都可能离线、走不通的代理、
// 或者对上根本没启动的上游。刷新失败必须降级成「额度未知」，不能把调用方打挂
// （socket/DNS 错误不是 UpstreamError）。
func (p *Pool) RefreshQuota(acc *Account) {
	if acc == nil || acc.Session == "" {
		return
	}

	quota, err := p.Gateway.Quota(acc.Session)
	if err != nil {
		var uerr *UpstreamError
		if errors.As(err, &uerr) {
			if uerr.Status == 401 || uerr.Status == 403 {
				acc.Session, acc.ExpireAt = "", 0
				p.Log(fmt.Sprintf("账号 %s session 被上游拒绝（HTTP %d）", acc.Name, uerr.Status))
			} else {
				acc.LastError = "quota: " + err.Error()
				p.Log(fmt.Sprintf("账号 %s 额度查询失败：%v", acc.Name, err))
			}
			return
		}
		acc.LastError = "quota: " + err.Error()
		p.Log(fmt.Sprintf("账号 %s 额度查询异常（网络？）：%v", acc.Name, err))
		return
	}

	acc.Balance = quota.Balance
	acc.DailyBalance = quota.DailyBalance
	acc.Available = quota.Available
	acc.QuotaUpdatedAt = time.Now().Unix()
}

// ---------------------------------------------------------------- 选号

// Usable 对应 Python usable：启用、不在冷却期、未被排除、会话有效且额度未耗尽。
// Available 为 nil（未知）视为可用，只有明确的 0 才排除。
func (p *Pool) Usable(exclude ...string) []*Account {
	excluded := poolExcludeSet(exclude)
	out := []*Account{}
	for _, acc := range p.Accounts() {
		if !acc.Enabled || acc.InCooldown() || excluded[acc.Name] || !acc.SessionValid() {
			continue
		}
		if acc.Available != nil && *acc.Available == 0 {
			continue
		}
		out = append(out, acc)
	}
	return out
}

// Acquire 对应 Python acquire：返回下一个可用账号（并保证会话有效）。
//
// 一个健康账号都没有时退回到「全部启用账号」，让单账号部署有机会自愈（续期/重登）；
// 连启用账号都没有才报错。
func (p *Pool) Acquire(exclude ...string) (*Account, error) {
	candidates := p.Usable(exclude...)
	if len(candidates) == 0 {
		excluded := poolExcludeSet(exclude)
		allEnabled := []*Account{}
		for _, acc := range p.Accounts() {
			if acc.Enabled && !excluded[acc.Name] {
				allEnabled = append(allEnabled, acc)
			}
		}
		if len(allEnabled) == 0 {
			return nil, &PoolError{Msg: "账号池里没有可用账号（accounts.json 为空或全部禁用）"}
		}
		candidates = allEnabled
		p.Log("没有健康账号，尝试对现有账号续期/重登")
	}

	strategy := strings.ToLower(p.Cfg.Strategy)
	if strategy == "" {
		strategy = "balance"
	}

	var acc *Account
	switch strategy {
	case "round_robin":
		p.rrMu.Lock()
		acc = candidates[p.rrIndex%len(candidates)]
		next := len(candidates)
		if next < 1 {
			next = 1
		}
		p.rrIndex = (p.rrIndex + 1) % next
		p.rrMu.Unlock()
	case "lru":
		acc = candidates[0]
		for _, cand := range candidates[1:] {
			if cand.LastUsed < acc.LastUsed {
				acc = cand
			}
		}
	default: // balance
		acc = candidates[0]
		bestAvail, bestLast := poolBalanceKey(acc)
		for _, cand := range candidates[1:] {
			avail, last := poolBalanceKey(cand)
			// Python 的 max(key=(available 未知→1e9, -last_used))
			if avail > bestAvail || (avail == bestAvail && last > bestLast) {
				acc, bestAvail, bestLast = cand, avail, last
			}
		}
	}

	if err := p.EnsureSession(acc, false); err != nil {
		return nil, err
	}
	acc.LastUsed = accountNow()
	return acc, nil
}

// ReportSuccess 对应 Python report_success：记一次成功，扣减积分，清空 last_error。
func (p *Pool) ReportSuccess(acc *Account, usage map[string]any) {
	if acc == nil {
		return
	}
	acc.Requests++
	if points, isNum := poolNum(usage["points_consumed"]); isNum {
		acc.PointsUsed += int(points)
		if acc.Available != nil {
			next := *acc.Available - points
			if next < 0 {
				next = 0
			}
			acc.Available = &next
		}
	}
	acc.LastError = ""
	p.Save(false)
}

// ReportFailure 对应 Python report_failure：记一次失败并进入冷却期。
// 401/403 说明会话已经作废，直接清掉，等下次启用时用账号密码重登。
func (p *Pool) ReportFailure(acc *Account, status int, reason string) {
	if acc == nil {
		return
	}
	acc.Failures++
	if reason != "" {
		acc.LastError = reason
	} else {
		acc.LastError = fmt.Sprintf("HTTP %d", status)
	}
	if status == 401 || status == 403 {
		acc.Session, acc.ExpireAt = "", 0
	}
	cooldown := float64(p.Cfg.CooldownSeconds)
	if cooldown == 0 {
		cooldown = 300
	}
	acc.Cooldown(cooldown, acc.LastError)
	p.Log(fmt.Sprintf("账号 %s 失败 HTTP %d → 冷却 %.0fs（%s）",
		acc.Name, status, cooldown, acc.LastError))
	p.Save(false)
}

// Snapshot 对应 Python snapshot：面板 / admin API 用的状态视图（不含秘密）。
func (p *Pool) Snapshot() map[string]any {
	accounts := p.Accounts()
	public := make([]any, 0, len(accounts))
	for _, acc := range accounts {
		public = append(public, acc.PublicDict())
	}
	return map[string]any{
		"strategy": p.Cfg.Strategy,
		"count":    len(accounts),
		"usable":   len(p.Usable()),
		"accounts": public,
	}
}

// ---------------------------------------------------------------- 后台维护

// Start 对应 Python start：启动 keeper goroutine（续期 + 额度刷新）。
// 已经在跑就直接返回，避免重复启动。
func (p *Pool) Start() {
	p.keeperMu.Lock()
	if p.keeperRunning {
		p.keeperMu.Unlock()
		return
	}
	p.keeperRunning = true
	stop := make(chan struct{})
	p.stopCh = stop
	p.keeperMu.Unlock()

	p.Log("账号守护线程已启动（续期 + 额度刷新）")
	go p.keeperLoop(stop)
}

// Stop 对应 Python stop：通知 keeper 退出。幂等（重复关闭 channel 会 panic，
// 这里先探测再关）。
func (p *Pool) Stop() {
	p.keeperMu.Lock()
	defer p.keeperMu.Unlock()
	if p.stopCh == nil {
		return
	}
	select {
	case <-p.stopCh:
	default:
		close(p.stopCh)
	}
}

// keeperLoop 对应 Python _loop：每 interval 秒跑一次 tick。
// stop 是本次运行的停止信号；退出时只清自己那一代的 running 标记，
// 避免 Stop 后立刻 Start 时旧 goroutine 把新一代的状态清掉。
func (p *Pool) keeperLoop(stop chan struct{}) {
	defer func() {
		p.keeperMu.Lock()
		if p.stopCh == stop {
			p.keeperRunning = false
		}
		p.keeperMu.Unlock()
	}()

	interval := p.keeperInterval()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			p.safeTick()
			timer.Reset(interval)
		}
	}
}

// keeperInterval 对应 Python _loop 的 interval：max(60, quota_refresh_minutes*60) 秒。
func (p *Pool) keeperInterval() time.Duration {
	minutes := p.Cfg.QuotaRefreshMinutes
	if minutes == 0 {
		minutes = 30
	}
	seconds := int(minutes * 60)
	if seconds < 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

// safeTick 跑一轮维护并把 panic 降级成日志（对应 Python 的 except Exception）。
func (p *Pool) safeTick() {
	defer func() {
		if r := recover(); r != nil {
			p.Log(fmt.Sprintf("账号守护线程异常：%v", r))
		}
	}()
	p.Tick()
}

// Tick 对应 Python tick：一轮维护 —— 续期会话、刷新额度。
// RefreshQuota 自身不会失败；EnsureSession 的错误打日志后继续下一个账号
// （与 Python 的 try 包住两者一致：续期失败就不会再查额度）。
func (p *Pool) Tick() {
	for _, acc := range p.Accounts() {
		if !acc.Enabled {
			continue
		}
		if err := p.EnsureSession(acc, false); err != nil {
			var perr *PoolError
			var aerr *AccountError
			if errors.As(err, &perr) || errors.As(err, &aerr) {
				p.Log(fmt.Sprintf("账号 %s 维护失败：%v", acc.Name, err))
			} else {
				p.Log(fmt.Sprintf("账号 %s 维护异常：%v", acc.Name, err))
			}
			continue
		}
		p.RefreshQuota(acc)
	}
	p.Save(false)
}

// Bootstrap 对应 Python bootstrap：启动时把没有可用会话的账号登上，末尾落盘。
func (p *Pool) Bootstrap() {
	for _, acc := range p.Accounts() {
		if acc.Enabled && !acc.SessionValid() {
			if err := p.EnsureSession(acc, false); err != nil {
				p.Log(fmt.Sprintf("账号 %s 启动登录失败：%v", acc.Name, err))
			}
		}
	}
	p.Save(false)
}

// ---------------------------------------------------------------- 小工具

// poolExcludeSet 把 variadic exclude 转成集合（对应 Python 的 `name not in exclude`）。
func poolExcludeSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// poolBalanceKey 对应 balance 策略的排序键 (available 未知→1e9, -last_used)。
func poolBalanceKey(a *Account) (float64, float64) {
	available := 1e9
	if a.Available != nil {
		available = float64(*a.Available)
	}
	return available, -a.LastUsed
}

// poolNum 对应 Python 的 isinstance(v, (int, float)) 并把值转成整数。
// JSON 解码只有 float64，所以这里统一按 int64 处理。
func poolNum(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case float32:
		return int64(t), true
	case float64:
		return int64(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

// poolStr 对应 Python 的 str(v)（update_account 里把字段转成字符串）。
// 大整数手机号在 JSON 里是 float64，必须按定点打印，不能被 fmt 写成科学计数法。
func poolStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

// poolTail 对应 Python 的 s[-n:]（按码点切）。
func poolTail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}
