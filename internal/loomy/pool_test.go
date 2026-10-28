package loomy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 移植自 tests/test_pool.py（配置用 support_test.go 的 makeConfig，替代 TemporaryDirectory
// 的是 t.TempDir）。Go 不能给接口字段塞一个只有 Fill 的临时类，所以下面定义了两个
// 最小的包级 fake，名字带 pool 前缀，避免和后续波次的 fake 撞名。

// poolFakeClient 是只记录 fill 的最小账号客户端（对应 test_expiring_session_triggers_relogin
// 里的 FakeClient）。
type poolFakeClient struct {
	fills *[]string
}

var _ AccountClientAPI = (*poolFakeClient)(nil)

func (c *poolFakeClient) IdentityMode() string { return "per_account" }

func (c *poolFakeClient) EnsureIdentity(acc *Account) map[string]any {
	if len(acc.Identity) == 0 {
		acc.Identity = NewIdentity(c.IdentityMode())
	}
	return acc.Identity
}

func (c *poolFakeClient) RebindIdentity(acc *Account) map[string]any {
	acc.Identity = NewIdentity(c.IdentityMode())
	return acc.Identity
}

func (c *poolFakeClient) Fill(acc *Account) error {
	if c.fills != nil {
		*c.fills = append(*c.fills, acc.Name)
	}
	acc.Session = "fresh"
	acc.ExpireAt = time.Now().Unix() + SessionExpireSeconds
	return nil
}

// poolFakeGateway 用一个函数实现 GatewayAPI（对应两个 tick 用例里的 FakeGateway）。
type poolFakeGateway struct {
	quota func(session string) (Quota, error)
}

var _ GatewayAPI = (*poolFakeGateway)(nil)

func (g *poolFakeGateway) Quota(session string) (Quota, error) {
	if g.quota == nil {
		return Quota{}, nil
	}
	return g.quota(session)
}

// poolInt 造一个 *int64（Account 的额度字段用指针区分「未知」与「零」）。
func poolInt(v int64) *int64 { return &v }

// poolAccountsPath 返回 makeConfig 使用的账号文件路径。
func poolAccountsPath(dir string) string { return filepath.Join(dir, "accounts.json") }

// poolReadAccounts 读回落盘的 {"accounts":[...]} 列表。
func poolReadAccounts(t *testing.T, path string) []any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	return jlist(obj, "accounts")
}

// poolSavedAccount 在落盘列表里按名字找一条记录。
func poolSavedAccount(t *testing.T, items []any, name string) map[string]any {
	t.Helper()
	for _, item := range items {
		if m, isMap := item.(map[string]any); isMap && jstr(m, "name") == name {
			return m
		}
	}
	t.Fatalf("落盘账号里没有 %q: %v", name, items)
	return nil
}

// poolExpireIn 生成「从现在起 seconds 秒后过期」的 unix 秒。
func poolExpireIn(seconds int64) int64 { return time.Now().Unix() + seconds }

// ------------------------------------------------------------------ TestPool

func TestLoadAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "loginid": "13800000001", "password": "p",
			"session": "s1", "expireAt": poolExpireIn(14 * 86400)},
		{"name": "b", "session": "s2", "expireAt": poolExpireIn(14 * 86400)},
	})

	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	names := []string{}
	for _, acc := range pool.Accounts() {
		names = append(names, acc.Name)
	}
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("账号名 = %v, want [a b]", names)
	}

	snap := pool.Snapshot()
	if got := snap["count"]; got != 2 {
		t.Fatalf("snapshot count = %v, want 2", got)
	}
	if got := snap["usable"]; got != 2 {
		t.Fatalf("snapshot usable = %v, want 2", got)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(raw), `"password":`) {
		t.Fatalf("snapshot 泄露了密码: %s", raw)
	}
}

func TestBalanceStrategyPrefersRichest(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "poor", "session": "s1", "expireAt": poolExpireIn(14 * 86400)},
		{"name": "rich", "session": "s2", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	pool.Get("poor").Available = poolInt(10)
	pool.Get("rich").Available = poolInt(900)

	acc, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if acc.Name != "rich" {
		t.Fatalf("acquire = %q, want rich", acc.Name)
	}
}

func TestRoundRobinStrategy(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s1", "expireAt": poolExpireIn(14 * 86400)},
		{"name": "b", "session": "s2", "expireAt": poolExpireIn(14 * 86400)},
	})
	cfg := makeConfig(dir, "http://127.0.0.1:1/api/v1")
	cfg.Strategy = "round_robin"
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	picked := []string{}
	for i := 0; i < 4; i++ {
		acc, aerr := pool.Acquire()
		if aerr != nil {
			t.Fatalf("Acquire #%d: %v", i, aerr)
		}
		picked = append(picked, acc.Name)
	}
	if strings.Join(picked, ",") != "a,b,a,b" {
		t.Fatalf("round_robin = %v, want [a b a b]", picked)
	}
}

func TestLRUStrategy(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "old", "session": "s1", "expireAt": poolExpireIn(14 * 86400)},
		{"name": "new", "session": "s2", "expireAt": poolExpireIn(14 * 86400)},
	})
	cfg := makeConfig(dir, "http://127.0.0.1:1/api/v1")
	cfg.Strategy = "lru"
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	pool.Get("old").LastUsed = 100
	pool.Get("new").LastUsed = 200

	acc, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if acc.Name != "old" {
		t.Fatalf("acquire = %q, want old", acc.Name)
	}
}

func TestCooldownSkipsAccountAndRecovers(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s1", "expireAt": poolExpireIn(14 * 86400)},
		{"name": "b", "session": "s2", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	pool.ReportFailure(pool.Get("a"), 401, "invalid session")

	acc, aerr := pool.Acquire()
	if aerr != nil {
		t.Fatalf("Acquire: %v", aerr)
	}
	if acc.Name != "b" {
		t.Fatalf("acquire = %q, want b", acc.Name)
	}

	usable := pool.Usable()
	if len(usable) != 1 || usable[0].Name != "b" {
		t.Fatalf("usable = %v, want [b]", usable)
	}
	// a 收到 401 后会话被清掉，要恢复必须靠账号密码重登。
	if pool.Get("a").SessionValid() {
		t.Fatal("a 的 session 应该已经被清掉")
	}
}

func TestExhaustedAccountIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s1", "expireAt": poolExpireIn(14 * 86400)},
		{"name": "b", "session": "s2", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	pool.Get("a").Available = poolInt(0)

	acc, aerr := pool.Acquire()
	if aerr != nil {
		t.Fatalf("Acquire: %v", aerr)
	}
	if acc.Name != "b" {
		t.Fatalf("acquire = %q, want b", acc.Name)
	}
}

func TestAcquireRaisesWhenPoolEmpty(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	_, aerr := pool.Acquire()
	var perr *PoolError
	if !errors.As(aerr, &perr) {
		t.Fatalf("err = %v, want *PoolError", aerr)
	}
}

func TestDisabledAccountIgnored(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s1", "expireAt": poolExpireIn(86400), "enabled": false},
		{"name": "b", "session": "s2", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	names := []string{}
	for _, acc := range pool.Usable() {
		names = append(names, acc.Name)
	}
	if strings.Join(names, ",") != "b" {
		t.Fatalf("usable = %v, want [b]", names)
	}
}

func TestAddRemovePersists(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	if _, err := pool.AddAccount("x", "13800000009", "pw", "", "", nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	pool.Flush()

	saved := poolSavedAccount(t, poolReadAccounts(t, poolAccountsPath(dir)), "x")
	if got := jstr(saved, "loginid"); got != "13800000009" {
		t.Fatalf("落盘 loginid = %q", got)
	}
	if !pool.RemoveAccount("x") {
		t.Fatal("RemoveAccount 第一次应返回 true")
	}
	if pool.RemoveAccount("x") {
		t.Fatal("RemoveAccount 第二次应返回 false")
	}
}

func TestDuplicateAddRejected(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "x", "session": "s", "expireAt": poolExpireIn(90000)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	_, aerr := pool.AddAccount("x", "", "", "", "", nil)
	var perr *PoolError
	if !errors.As(aerr, &perr) {
		t.Fatalf("err = %v, want *PoolError", aerr)
	}
}

func TestReportSuccessDecrementsQuota(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s", "expireAt": poolExpireIn(90000)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	acc := pool.Get("a")
	acc.Available = poolInt(100)
	pool.ReportSuccess(acc, map[string]any{"points_consumed": 3})

	if acc.PointsUsed != 3 {
		t.Fatalf("points_used = %d, want 3", acc.PointsUsed)
	}
	if acc.Available == nil || *acc.Available != 97 {
		t.Fatalf("available = %v, want 97", acc.Available)
	}
	if acc.Requests != 1 {
		t.Fatalf("requests = %d, want 1", acc.Requests)
	}
}

// -------------------------------------------------------- TestSessionLifecycle

func TestExpiringSessionTriggersRelogin(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "loginid": "13800000001", "password": "pw",
			"session": "stale", "expireAt": poolExpireIn(3600)}, // < 3 天
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	fills := []string{}
	pool.Client = &poolFakeClient{fills: &fills}

	if _, err := pool.Acquire(); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if strings.Join(fills, ",") != "a" {
		t.Fatalf("fills = %v, want [a]", fills)
	}
	if got := pool.Get("a").Session; got != "fresh" {
		t.Fatalf("session = %q, want fresh", got)
	}
}

func TestNoCredentialsRaisesActionableError(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{{"name": "a"}})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	_, aerr := pool.Acquire()
	var perr *PoolError
	if !errors.As(aerr, &perr) {
		t.Fatalf("err = %v, want *PoolError", aerr)
	}
	if !strings.Contains(perr.Msg, "loginid/password") {
		t.Fatalf("错误信息不可操作（缺 loginid/password 提示）: %q", perr.Msg)
	}
}

func TestTickRefreshesQuota(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	pool.Gateway = &poolFakeGateway{quota: func(session string) (Quota, error) {
		return Quota{Balance: poolInt(5), DailyBalance: poolInt(6), Available: poolInt(11)}, nil
	}}
	pool.Tick()

	acc := pool.Get("a")
	if acc.Balance == nil || *acc.Balance != 5 ||
		acc.DailyBalance == nil || *acc.DailyBalance != 6 ||
		acc.Available == nil || *acc.Available != 11 {
		t.Fatalf("quota = (%v, %v, %v), want (5, 6, 11)",
			acc.Balance, acc.DailyBalance, acc.Available)
	}
	if acc.QuotaUpdatedAt == 0 {
		t.Fatal("QuotaUpdatedAt 应该被刷新")
	}
}

func TestTickClearsSessionOn401(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	pool.Gateway = &poolFakeGateway{quota: func(session string) (Quota, error) {
		return Quota{}, &UpstreamError{Msg: "nope", Status: 401}
	}}
	pool.Tick()

	if pool.Get("a").SessionValid() {
		t.Fatal("401 之后 session 应该被清掉")
	}
}

// ------------------------------------------------------------ 补充用例（Go 侧）

// TestLoadParseErrorReturnsPoolError 覆盖 load 的解析失败分支（Python 无对应用例）。
func TestLoadParseErrorReturnsPoolError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(poolAccountsPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}

	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if pool != nil {
		t.Fatal("解析失败时不应返回可用的 Pool")
	}
	var perr *PoolError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *PoolError", err)
	}
	if !strings.Contains(perr.Msg, "parse error") {
		t.Fatalf("错误信息缺少 parse error: %q", perr.Msg)
	}
}

// TestLoadMissingFileIsEmpty 覆盖 load 的「文件不存在 = 空池」分支。
func TestLoadMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if len(pool.Accounts()) != 0 {
		t.Fatalf("空池应有 0 个账号, got %d", len(pool.Accounts()))
	}
	if got := pool.Snapshot()["count"]; got != 0 {
		t.Fatalf("snapshot count = %v, want 0", got)
	}
}

// TestClientSessionImported 覆盖桌面客户端登录态导入（派生账号不落盘）。
func TestClientSessionImported(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	clientDir := filepath.Join(root, "9f86d081884c", "userData")
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	updatedAt := int64(1700000000000) // 毫秒
	auth := fmt.Sprintf(`{"session":"client-s","userid":"u1","phone":"13800000001","updatedAt":%d}`, updatedAt)
	if err := os.WriteFile(filepath.Join(clientDir, "auth-session.json"), []byte(auth), 0o644); err != nil {
		t.Fatalf("写 auth-session.json: %v", err)
	}

	cfg := makeConfig(dir, "http://127.0.0.1:1/api/v1")
	cfg.SessionsFromClient = true
	cfg.ClientRoot = root
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	accounts := pool.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("导入账号数 = %d, want 1", len(accounts))
	}
	acc := accounts[0]
	if acc.Name != "desktop-0001" {
		t.Fatalf("name = %q, want desktop-0001", acc.Name)
	}
	if acc.Source != "client" || acc.Persist {
		t.Fatalf("source/persist = %q/%v, want client/false", acc.Source, acc.Persist)
	}
	if acc.Loginid != "13800000001" || acc.Userid != "u1" {
		t.Fatalf("loginid/userid = %q/%q", acc.Loginid, acc.Userid)
	}
	if want := updatedAt/1000 + SessionExpireSeconds; acc.ExpireAt != want {
		t.Fatalf("expire_at = %d, want %d", acc.ExpireAt, want)
	}

	// 派生账号由客户端拥有，永远不写回 accounts.json。
	pool.Save(true)
	if items := poolReadAccounts(t, poolAccountsPath(dir)); len(items) != 0 {
		t.Fatalf("客户端派生账号被写进了 accounts.json: %v", items)
	}
}

// TestSaveThrottleAndFlush 覆盖 save 的 5 秒节流与 flush。
func TestSaveThrottleAndFlush(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s1", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	acc, aerr := pool.AddAccount("b", "13800000002", "pw", "", "", nil)
	if aerr != nil {
		t.Fatalf("AddAccount: %v", aerr)
	}
	acc.Enabled = false

	// 距上次落盘不足 5 秒：只置 dirty，文件里仍是 enabled=true。
	pool.Save(false)
	saved := poolSavedAccount(t, poolReadAccounts(t, poolAccountsPath(dir)), "b")
	if enabled, _ := saved["enabled"].(bool); !enabled {
		t.Fatal("节流期内的 Save(false) 不应该落盘")
	}

	pool.Flush()
	saved = poolSavedAccount(t, poolReadAccounts(t, poolAccountsPath(dir)), "b")
	if enabled, _ := saved["enabled"].(bool); enabled {
		t.Fatal("Flush 之后 enabled=false 应该已经落盘")
	}
}

// TestKeeperStartStopIsIdempotent 覆盖 Start 的重复启动保护与 Stop 的幂等。
func TestKeeperStartStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	pool.Start()
	pool.Start() // 第二次必须直接返回
	pool.Stop()
	pool.Stop() // 重复 Stop 不能 panic（重复 close channel）
	poolTestWaitKeeperStopped(t, pool)

	// Stop 之后可以重新 Start（keeper 既不能重复启动，也不能再也起不来）。
	pool.Start()
	pool.Stop()
	poolTestWaitKeeperStopped(t, pool)
}

// poolTestWaitKeeperStopped 轮询等 keeper 退出。
func poolTestWaitKeeperStopped(t *testing.T, pool *Pool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.keeperMu.Lock()
		running := pool.keeperRunning
		pool.keeperMu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Stop 之后 keeper 仍在运行")
}

// TestTickSkipsRefreshAfterEnsureSessionError 覆盖 tick 的错误分支：
// 续期失败只打日志，不再查额度（对齐 Python 的 try 同时包住两者）。
func TestTickSkipsRefreshAfterEnsureSessionError(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{{"name": "a"}})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	refreshed := false
	pool.Gateway = &poolFakeGateway{quota: func(session string) (Quota, error) {
		refreshed = true
		return Quota{}, nil
	}}
	logs := []string{}
	pool.Log = func(msg string) { logs = append(logs, msg) }

	pool.Tick()

	if refreshed {
		t.Fatal("续期失败后不应再查额度")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "维护失败") {
		t.Fatalf("缺少维护失败日志: %v", logs)
	}
}

// TestUpdateAccountPatchesFields 覆盖 update_account：换密码清会话、enabled 开关、
// 不存在的账号报 PoolError（Python 无对应用例）。
func TestUpdateAccountPatchesFields(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "session": "s1", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	acc, aerr := pool.UpdateAccount("a", map[string]any{"password": "new", "enabled": false})
	if aerr != nil {
		t.Fatalf("UpdateAccount: %v", aerr)
	}
	if acc.Password != "new" {
		t.Fatalf("password = %q", acc.Password)
	}
	// 新密码属于新登录，旧会话必须作废。
	if acc.Session != "" || acc.ExpireAt != 0 {
		t.Fatalf("换密码后 session/expire 应清空, got %q/%d", acc.Session, acc.ExpireAt)
	}
	if acc.Enabled {
		t.Fatal("enabled 应为 false")
	}

	_, aerr = pool.UpdateAccount("nope", nil)
	var perr *PoolError
	if !errors.As(aerr, &perr) {
		t.Fatalf("err = %v, want *PoolError", aerr)
	}
}

// TestRebindIdentityLogsInAgain 覆盖 rebind_identity：换 identity + 有凭据时强制重登
// （顺带验证 support_test.go 的 FakeAccountClient 与 Pool 的接口对齐）。
func TestRebindIdentityLogsInAgain(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, poolAccountsPath(dir), []map[string]any{
		{"name": "a", "loginid": "13800000001", "password": "pw",
			"session": "s1", "expireAt": poolExpireIn(14 * 86400)},
	})
	pool, err := NewPool(makeConfig(dir, "http://127.0.0.1:1/api/v1"), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	client := &FakeAccountClient{}
	pool.Client = client

	acc, aerr := pool.RebindIdentity("a")
	if aerr != nil {
		t.Fatalf("RebindIdentity: %v", aerr)
	}
	if len(client.Rebinds) != 1 || client.Rebinds[0] != "a" {
		t.Fatalf("rebinds = %v, want [a]", client.Rebinds)
	}
	if len(client.Fills) != 1 || client.Fills[0] != "a" {
		t.Fatalf("fills = %v, want [a]（有凭据时应强制重登）", client.Fills)
	}
	if acc.Session != "testsession1" {
		t.Fatalf("session = %q, want testsession1", acc.Session)
	}
	if len(acc.Identity) == 0 {
		t.Fatal("rebind 之后 identity 不能为空")
	}
}
