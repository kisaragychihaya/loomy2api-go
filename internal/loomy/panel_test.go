package loomy

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 移植自 tests/test_panel.py 中方法级的 TestPanelState / TestPanelWrites。
//
// Python 的 PanelHarness 起一个真的 HTTP 服务再发请求；这里直接构造 Panel 调方法，
// 断言同样的状态与副作用（数值对齐 FakeUpstream.Quota = balance 100 / daily 50 /
// available 150）。HTTP 状态码映射属于 server 层，不在本文件范围内。
//
// 与 Python Harness 一致：所有用例都注入 FakeAccountClient，账号池在 setUp 里
// bootstrap 一次（账号 a 的会话有效，bootstrap 不会真的登录）。

// pnlTestHarness 对应 Python 的 PanelHarness：临时目录 + 假上游 + 一条已配置账号。
type pnlTestHarness struct {
	dir      string
	logPath  string
	upstream *FakeUpstream
	cfg      *Config
	pool     *Pool
	client   *FakeAccountClient
	panel    *Panel
}

// pnlTestNew 对应 PanelHarness.setUp。
func pnlTestNew(t *testing.T) *pnlTestHarness {
	t.Helper()
	dir := t.TempDir()

	upstream := &FakeUpstream{}
	base := upstream.Start()
	t.Cleanup(upstream.Stop)

	writeAccounts(t, filepath.Join(dir, "accounts.json"), []map[string]any{{
		"name": "a", "loginid": "13800000001", "password": "pw",
		"session": "s1", "userid": "u1",
		"expireAt": time.Now().Unix() + 14*86400,
		"identity": map[string]any{
			"devid":            "web-aaaaaaaaaaaaaaaa",
			"ua":               "UA",
			"modelid":          "Web",
			"version":          "1.0.0",
			"campus_device_id": "loomy-campus-fixed",
			"created_at":       1700000000,
		},
	}})

	cfg := makeConfig(dir, base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	client := &FakeAccountClient{}
	pool.Client = client
	pool.Bootstrap()

	logPath := filepath.Join(dir, "logs", "gateway.log")
	panel := NewPanel(cfg, pool, func(string) {}, logPath, func() []map[string]any { return nil })

	return &pnlTestHarness{
		dir: dir, logPath: logPath, upstream: upstream,
		cfg: cfg, pool: pool, client: client, panel: panel,
	}
}

// ------------------------------------------------------------------ 断言辅助

func pnlTestMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	out, isMap := m[key].(map[string]any)
	if !isMap {
		t.Fatalf("%q 不是对象: %v", key, m[key])
	}
	return out
}

func pnlTestList(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	out, isList := m[key].([]any)
	if !isList {
		t.Fatalf("%q 不是数组: %v", key, m[key])
	}
	return out
}

func pnlTestInt(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	if _, ok := m[key]; !ok {
		t.Fatalf("缺少字段 %q: %v", key, m)
	}
	return jint(m, key)
}

func pnlTestFloat(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	if _, ok := m[key]; !ok {
		t.Fatalf("缺少字段 %q: %v", key, m)
	}
	return jfloat(m, key)
}

// pnlTestLines 取日志行（Logs 返回 []string）。
func pnlTestLines(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	out, isList := m[key].([]string)
	if !isList {
		t.Fatalf("%q 不是字符串数组: %v", key, m[key])
	}
	return out
}

// pnlTestPoolError 断言 err 是 *PoolError 且消息包含 want。
func pnlTestPoolError(t *testing.T, err error, want string) *PoolError {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 *PoolError（含 %q），实际为 nil", want)
	}
	var perr *PoolError
	if !errors.As(err, &perr) {
		t.Fatalf("期望 *PoolError，实际为 %T: %v", err, err)
	}
	if !strings.Contains(perr.Msg, want) {
		t.Fatalf("错误信息 = %q，期望包含 %q", perr.Msg, want)
	}
	return perr
}

// pnlTestPointsCalls 数假上游收到的 points 查询次数（对应 Python 的
// `"points" in c["path"]` 计数）。
func pnlTestPointsCalls(up *FakeUpstream) int {
	count := 0
	for _, call := range up.CallsCopy() {
		if strings.Contains(jstr(call, "path"), "points") {
			count++
		}
	}
	return count
}

// pnlTestSavedAccounts 读回落盘的 {"accounts":[...]} 列表。
func pnlTestSavedAccounts(t *testing.T, dir string) []any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("读取 accounts.json: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("解析 accounts.json: %v", err)
	}
	return jlist(obj, "accounts")
}

// pnlTestSavedAccount 在落盘列表里按名字找一条记录。
func pnlTestSavedAccount(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	items := pnlTestSavedAccounts(t, dir)
	for _, item := range items {
		if m, isMap := item.(map[string]any); isMap && jstr(m, "name") == name {
			return m
		}
	}
	t.Fatalf("落盘账号里没有 %q: %v", name, items)
	return nil
}

// ------------------------------------------------------------- TestPanelState

func TestStateListsAccountsWithQuota(t *testing.T) {
	h := pnlTestNew(t)
	state := h.panel.State(false)

	if !jbool(state, "ok") {
		t.Fatalf("state.ok = false: %v", state)
	}
	totals := pnlTestMap(t, state, "totals")
	for key, want := range map[string]int64{
		"accounts": 1, "usable": 1, "available": 150,
		"unique_accounts": 1, "requests": 0, "points_used": 0,
	} {
		if got := pnlTestInt(t, totals, key); got != want {
			t.Fatalf("totals[%s] = %d, want %d", key, got, want)
		}
	}

	accounts := pnlTestList(t, state, "accounts")
	if len(accounts) != 1 {
		t.Fatalf("accounts 数 = %d, want 1", len(accounts))
	}
	acc := accounts[0].(map[string]any)
	if got := jstr(acc, "name"); got != "a" {
		t.Fatalf("name = %q, want a", got)
	}
	for key, want := range map[string]int64{"available": 150, "balance": 100, "daily_balance": 50} {
		if got := pnlTestInt(t, acc, key); got != want {
			t.Fatalf("account[%s] = %d, want %d", key, got, want)
		}
	}
	if got := jstr(acc, "loginid_masked"); got != "138****0001" {
		t.Fatalf("loginid_masked = %q, want 138****0001", got)
	}
	if got := jstr(acc, "mode"); got != "password" {
		t.Fatalf("mode = %q, want password", got)
	}

	// 面板状态里不能出现密码字段。
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if strings.Contains(string(raw), `"password":`) {
		t.Fatalf("面板状态泄露了 password 字段: %s", raw)
	}
}

func TestStateReportsIdentity(t *testing.T) {
	h := pnlTestNew(t)
	state := h.panel.State(false)

	accounts := pnlTestList(t, state, "accounts")
	identity := pnlTestMap(t, accounts[0].(map[string]any), "identity")
	if !jbool(identity, "bound") {
		t.Fatalf("identity.bound = false: %v", identity)
	}
	if got := jstr(identity, "devid"); got != "web-aaaaaaaaaaaaaaaa" {
		t.Fatalf("identity.devid = %q", got)
	}
}

func TestStateReportsConfig(t *testing.T) {
	h := pnlTestNew(t)
	state := h.panel.State(false)

	cfg := pnlTestMap(t, state, "config")
	if got := jstr(cfg, "upstream"); got != h.upstream.URL() {
		t.Fatalf("config.upstream = %q, want %q", got, h.upstream.URL())
	}
	if got := jstr(cfg, "strategy"); got != "balance" {
		t.Fatalf("config.strategy = %q, want balance", got)
	}
	if got := jstr(cfg, "default_model"); got != h.cfg.DefaultModel {
		t.Fatalf("config.default_model = %q", got)
	}
	if got := jstr(cfg, "identity_mode"); got != "per_account" {
		t.Fatalf("config.identity_mode = %q, want per_account", got)
	}
	if got := pnlTestInt(t, cfg, "models"); got != 0 {
		t.Fatalf("config.models = %d, want 0（catalogue 返回空）", got)
	}
	if jbool(cfg, "auth_required") {
		t.Fatal("config.auth_required = true, 未配置 api_keys 时应为 false")
	}
	if got := pnlTestFloat(t, cfg, "quota_refresh_minutes"); got != 999 {
		t.Fatalf("config.quota_refresh_minutes = %v, want 999", got)
	}
}

func TestRepeatedStateDoesNotHitUpstreamAgain(t *testing.T) {
	h := pnlTestNew(t)

	h.panel.State(false)
	before := pnlTestPointsCalls(h.upstream)
	h.panel.State(false)
	after := pnlTestPointsCalls(h.upstream)

	if before != 1 {
		t.Fatalf("首次 state 的 points 调用次数 = %d, want 1", before)
	}
	if after != before {
		t.Fatalf("重复 state 又打了上游：%d → %d（额度缓存应被复用）", before, after)
	}
}

func TestRefreshFlagForcesUpstreamCall(t *testing.T) {
	h := pnlTestNew(t)

	h.panel.State(false)
	before := pnlTestPointsCalls(h.upstream)
	h.panel.State(true)
	after := pnlTestPointsCalls(h.upstream)

	if after != before+1 {
		t.Fatalf("refresh 后 points 调用次数 = %d, want %d", after, before+1)
	}
}

// ------------------------------------------------------------ TestPanelWrites

func TestAddAccountWithPasswordLogsInAndBindsIdentity(t *testing.T) {
	h := pnlTestNew(t)

	result, err := h.panel.AddAccount(map[string]any{
		"name": "new", "loginid": "13900000002", "password": "pw", "login": true,
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if !jbool(result, "ok") {
		t.Fatalf("result.ok = false: %v", result)
	}
	if !jbool(result, "logged_in") {
		t.Fatalf("result.logged_in = false: %v", result)
	}
	identity := pnlTestMap(t, result, "identity")
	devid := jstr(identity, "devid")
	if !strings.HasPrefix(devid, "web-") {
		t.Fatalf("identity.devid = %q, 期望 web- 前缀", devid)
	}

	entry := pnlTestSavedAccount(t, h.dir, "new")
	if got := jstr(jmap(entry, "identity"), "devid"); got != devid {
		t.Fatalf("落盘 identity.devid = %q, want %q", got, devid)
	}
	if len(h.client.Fills) != 1 || h.client.Fills[0] != "new" {
		t.Fatalf("client.Fills = %v, want [new]", h.client.Fills)
	}
}

func TestAddAccountWithSessionOnly(t *testing.T) {
	h := pnlTestNew(t)

	result, err := h.panel.AddAccount(map[string]any{"name": "shared", "session": "borrowed-session"})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if !jbool(result, "ok") {
		t.Fatalf("result.ok = false: %v", result)
	}
	if _, has := result["logged_in"]; has {
		t.Fatalf("只给 session 时不应有 logged_in: %v", result)
	}

	acc := h.pool.Get("shared")
	if acc == nil {
		t.Fatal("账号 shared 没有进池")
	}
	if acc.Session != "borrowed-session" {
		t.Fatalf("session = %q, want borrowed-session", acc.Session)
	}
	if len(acc.Identity) == 0 {
		t.Fatal("创建账号时就该绑定 identity")
	}
}

func TestAddWithoutCredentialsIsRejected(t *testing.T) {
	h := pnlTestNew(t)

	_, err := h.panel.AddAccount(map[string]any{"name": "x"})
	pnlTestPoolError(t, err, "提供")

	// 缺名字优先报名字。
	_, err = h.panel.AddAccount(map[string]any{"loginid": "13900000002"})
	pnlTestPoolError(t, err, "name is required")

	// 只给手机号没密码。
	_, err = h.panel.AddAccount(map[string]any{"name": "y", "loginid": "13900000002"})
	pnlTestPoolError(t, err, "password required with phone")
}

func TestDuplicateNameIsRejected(t *testing.T) {
	h := pnlTestNew(t)

	_, err := h.panel.AddAccount(map[string]any{"name": "a", "session": "s"})
	pnlTestPoolError(t, err, "已存在")
}

func TestRenewRefreshesSessionAndClearsCooldown(t *testing.T) {
	h := pnlTestNew(t)

	acc := h.pool.Get("a")
	acc.Cooldown(600, "test")
	if !acc.InCooldown() {
		t.Fatal("冷却没有生效")
	}

	result, err := h.panel.RenewAccount(map[string]any{"name": "a"})
	if err != nil {
		t.Fatalf("RenewAccount: %v", err)
	}
	if !jbool(result, "ok") {
		t.Fatalf("result.ok = false: %v", result)
	}

	acc = h.pool.Get("a")
	if acc.InCooldown() {
		t.Fatal("renew 之后仍在冷却期")
	}
	days, known := acc.DaysLeft()
	if !known || math.Abs(days-14) > 0.1 {
		t.Fatalf("session_days_left = %v（known=%v）, want ≈14", days, known)
	}
	if got := pnlTestFloat(t, result, "session_days_left"); math.Abs(got-14) > 0.1 {
		t.Fatalf("result.session_days_left = %v, want ≈14", got)
	}
	if got := jstr(result, "userid"); got != "u1" {
		// FakeAccountClient 只在 userid 为空时才补 uid-<loginid>，这里沿用原有的 u1。
		t.Fatalf("result.userid = %q, want u1", got)
	}
	if len(h.client.Fills) != 1 || h.client.Fills[0] != "a" {
		t.Fatalf("client.Fills = %v, want [a]", h.client.Fills)
	}
}

func TestRenewUnknownAccountIsRejected(t *testing.T) {
	h := pnlTestNew(t)

	_, err := h.panel.RenewAccount(map[string]any{"name": "nope"})
	pnlTestPoolError(t, err, "no such account: nope")
}

func TestPanelRebindIdentityChangesDevid(t *testing.T) {
	h := pnlTestNew(t)

	before := jstr(h.pool.Get("a").Identity, "devid")
	result, err := h.panel.Identity(map[string]any{"name": "a", "regenerate": true})
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	after := jstr(pnlTestMap(t, result, "identity"), "devid")
	if after == before {
		t.Fatalf("regenerate 后 devid 未变: %q", after)
	}
	if !strings.HasPrefix(after, "web-") {
		t.Fatalf("devid = %q, 期望 web- 前缀", after)
	}
	if len(h.client.Rebinds) != 1 || h.client.Rebinds[0] != "a" {
		t.Fatalf("client.Rebinds = %v, want [a]", h.client.Rebinds)
	}
}

func TestIdentityInspectDoesNotRegenerate(t *testing.T) {
	h := pnlTestNew(t)

	before := jstr(h.pool.Get("a").Identity, "devid")
	result, err := h.panel.Identity(map[string]any{"name": "a"})
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	after := jstr(pnlTestMap(t, result, "identity"), "devid")
	if after != before {
		t.Fatalf("只查看时 devid 变了：%q → %q", before, after)
	}
	if len(h.client.Rebinds) != 0 {
		t.Fatalf("只查看时不该 rebind: %v", h.client.Rebinds)
	}
}

func TestToggleEnabled(t *testing.T) {
	h := pnlTestNew(t)

	result, err := h.panel.UpdateAccount(map[string]any{"name": "a", "enabled": false})
	if err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	if !jbool(result, "ok") {
		t.Fatalf("result.ok = false: %v", result)
	}
	if h.pool.Get("a").Enabled {
		t.Fatal("enabled 没有关闭")
	}
	totals := pnlTestMap(t, pnlTestMap(t, result, "state"), "totals")
	if got := pnlTestInt(t, totals, "usable"); got != 0 {
		t.Fatalf("禁用后 totals.usable = %d, want 0", got)
	}
}

func TestUpdatePasswordForcesNewLogin(t *testing.T) {
	h := pnlTestNew(t)

	if _, err := h.panel.UpdateAccount(map[string]any{
		"name": "a", "password": "new-pw", "login": true,
	}); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}

	acc := h.pool.Get("a")
	if acc.Password != "new-pw" {
		t.Fatalf("password = %q, want new-pw", acc.Password)
	}
	if !acc.SessionValid() {
		t.Fatal("改密码后应重新登录拿到有效会话")
	}
	if len(h.client.Fills) != 1 || h.client.Fills[0] != "a" {
		t.Fatalf("client.Fills = %v, want [a]", h.client.Fills)
	}
}

func TestRemoveAccount(t *testing.T) {
	h := pnlTestNew(t)

	result, err := h.panel.RemoveAccount(map[string]any{"name": "a"})
	if err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if !jbool(result, "ok") {
		t.Fatalf("result.ok = false: %v", result)
	}
	if got := jstr(result, "removed"); got != "a" {
		t.Fatalf("result.removed = %q, want a", got)
	}
	if h.pool.Get("a") != nil {
		t.Fatal("账号 a 还在池里")
	}
	if saved := pnlTestSavedAccounts(t, h.dir); len(saved) != 0 {
		t.Fatalf("accounts.json 里还有账号: %v", saved)
	}
}

func TestRemoveUnknownAccountIsRejected(t *testing.T) {
	h := pnlTestNew(t)

	_, err := h.panel.RemoveAccount(map[string]any{"name": "nope"})
	pnlTestPoolError(t, err, "no such account: nope")
}

// ------------------------------------------------------- 面板自身的小方法

func TestPanelMethodHTMLReturnsEmbeddedPage(t *testing.T) {
	h := pnlTestNew(t)

	page := string(h.panel.HTML())
	if page == "" {
		t.Fatal("HTML() 为空")
	}
	for _, want := range []string{"loomy2api", "/api/panel/state"} {
		if !strings.Contains(page, want) {
			t.Fatalf("面板页面缺少 %q", want)
		}
	}
}

func TestPanelMethodLogsTailsLastLines(t *testing.T) {
	h := pnlTestNew(t)

	// 文件不存在 → 空列表。
	if got := pnlTestLines(t, h.panel.Logs(20), "lines"); len(got) != 0 {
		t.Fatalf("日志文件不存在时 lines = %v, want 空", got)
	}

	if err := os.MkdirAll(filepath.Dir(h.logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		lines = append(lines, "line-"+strconv.Itoa(i))
	}
	if err := os.WriteFile(h.logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("写日志: %v", err)
	}

	tail := h.panel.Logs(20)
	if got := jstr(tail, "path"); got != h.logPath {
		t.Fatalf("logs.path = %q, want %q", got, h.logPath)
	}
	got := pnlTestLines(t, tail, "lines")
	if len(got) != 20 {
		t.Fatalf("lines 数 = %d, want 20", len(got))
	}
	if last := got[len(got)-1]; last != "line-24" {
		t.Fatalf("最后一行 = %q, want line-24", last)
	}

	// 少于 10 行的请求被抬到 10 行；超过 2000 被压到 2000。
	if got := pnlTestLines(t, h.panel.Logs(1), "lines"); len(got) != 10 {
		t.Fatalf("Logs(1) 返回 %d 行, want 10（下限）", len(got))
	}
	if got := pnlTestLines(t, h.panel.Logs(5000), "lines"); len(got) != 25 {
		t.Fatalf("Logs(5000) 返回 %d 行, want 25（文件只有 25 行）", len(got))
	}
}

func TestPanelMethodRefreshForcesQuotaCalls(t *testing.T) {
	h := pnlTestNew(t)

	state, err := h.panel.Refresh(nil)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !jbool(state, "ok") {
		t.Fatalf("Refresh 直接返回状态对象（不是包装）：%v", state)
	}
	first := pnlTestPointsCalls(h.upstream)
	if _, err := h.panel.Refresh(nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	second := pnlTestPointsCalls(h.upstream)

	if first != 1 || second != 2 {
		t.Fatalf("Refresh 的 points 调用次数 = %d/%d, want 1/2", first, second)
	}
}

func TestPanelMethodStateDedupesTotalsByUserid(t *testing.T) {
	h := pnlTestNew(t)
	expireAt := time.Now().Unix() + 14*86400
	// 同一账号的两份会话（我们自己的 + 客户端的）：额度只算一次。
	writeAccounts(t, filepath.Join(h.dir, "accounts.json"), []map[string]any{
		{"name": "a", "loginid": "13800000001", "password": "pw",
			"session": "s1", "userid": "u1", "expireAt": expireAt},
		{"name": "a-client", "session": "s2", "userid": "u1", "expireAt": expireAt},
	})
	if err := h.pool.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	totals := pnlTestMap(t, h.panel.State(true), "totals")
	for key, want := range map[string]int64{
		"accounts": 2, "usable": 2, "unique_accounts": 1, "available": 150,
	} {
		if got := pnlTestInt(t, totals, key); got != want {
			t.Fatalf("totals[%s] = %d, want %d", key, got, want)
		}
	}
}

func TestPanelMethodAddAccountSkipsLoginWhenDisabled(t *testing.T) {
	h := pnlTestNew(t)

	result, err := h.panel.AddAccount(map[string]any{
		"name": "quiet", "loginid": "13900000003", "password": "pw", "login": false,
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if _, has := result["logged_in"]; has {
		t.Fatalf("login=false 时不该登录: %v", result)
	}
	if len(h.client.Fills) != 0 {
		t.Fatalf("login=false 时不该调用 Fill: %v", h.client.Fills)
	}
	if acc := h.pool.Get("quiet"); acc == nil || acc.Session != "" {
		t.Fatalf("login=false 时不该有会话: %+v", acc)
	}
}

func TestPanelMethodQuotaStaleDefaultsToThirtyMinutes(t *testing.T) {
	h := pnlTestNew(t)
	h.cfg.QuotaRefreshMinutes = 0 // 按 30 分钟算

	acc := h.pool.Get("a")
	acc.QuotaUpdatedAt = time.Now().Unix() - 29*60
	before := pnlTestPointsCalls(h.upstream)
	h.panel.State(false)
	if got := pnlTestPointsCalls(h.upstream); got != before {
		t.Fatalf("29 分钟前的额度缓存应被复用，points 调用次数 %d → %d", before, got)
	}

	acc.QuotaUpdatedAt = time.Now().Unix() - 31*60
	h.panel.State(false)
	if got := pnlTestPointsCalls(h.upstream); got != before+1 {
		t.Fatalf("31 分钟前的额度缓存应刷新，points 调用次数 %d → %d", before, got)
	}
}
