package loomy

import (
	"strings"
	"testing"
	"time"
)

// 移植自 loomy2api PR #2 的 onboarding 语义（Python 版没有单独的测试文件，
// 这里按本仓库惯例把实测行为钉成自封闭用例）：任务表、纯接口领取、幂等跳过、
// auth 失败中断、请求体只带 key。

// obTestCalls 取出假上游上 onboarding 相关的请求记录。
func obTestCalls(fake *FakeUpstream, suffix string) []map[string]any {
	out := []map[string]any{}
	for _, call := range fake.CallsCopy() {
		if strings.HasSuffix(jstr(call, "path"), suffix) {
			out = append(out, call)
		}
	}
	return out
}

// TestOnboardingTaskTable：8 个任务的 key 唯一、积分合计恰为 10000，
// 顺序与官方客户端一致（fetch → 领取都按这个顺序）。
func TestOnboardingTaskTable(t *testing.T) {
	if len(OnboardingTasks) != 8 {
		t.Fatalf("任务数 = %d, want 8", len(OnboardingTasks))
	}
	sum := 0
	seen := map[string]bool{}
	for _, task := range OnboardingTasks {
		if seen[task.Key] {
			t.Fatalf("任务 key 重复: %s", task.Key)
		}
		seen[task.Key] = true
		if task.Title == "" {
			t.Fatalf("任务 %s 缺少中文标题", task.Key)
		}
		if OnboardingTaskPoints[task.Key] != task.Points {
			t.Fatalf("OnboardingTaskPoints[%q] = %d, want %d", task.Key, OnboardingTaskPoints[task.Key], task.Points)
		}
		if OnboardingTaskTitles[task.Key] != task.Title {
			t.Fatalf("OnboardingTaskTitles[%q] = %q, want %q", task.Key, OnboardingTaskTitles[task.Key], task.Title)
		}
		sum += task.Points
	}
	if sum != OnboardingTotal {
		t.Fatalf("积分合计 = %d, want %d", sum, OnboardingTotal)
	}
}

// TestOnboardingComputeEarned：本地重算只认 8 个已知 key（未知 key 忽略）。
func TestOnboardingComputeEarned(t *testing.T) {
	if got := OnboardingComputeEarned(map[string]bool{}); got != 0 {
		t.Fatalf("空表 earned = %d, want 0", got)
	}
	all := map[string]bool{}
	for _, task := range OnboardingTasks {
		all[task.Key] = true
	}
	if got := OnboardingComputeEarned(all); got != OnboardingTotal {
		t.Fatalf("满表 earned = %d, want %d", got, OnboardingTotal)
	}
	if got := OnboardingComputeEarned(map[string]bool{"first_message": true}); got != 500 {
		t.Fatalf("单任务 earned = %d, want 500", got)
	}
}

// TestOnboardingNormalizeTasks：非对象输入一律补 false，非 true 值当 false。
func TestOnboardingNormalizeTasks(t *testing.T) {
	nilTasks := OnboardingNormalizeTasks(nil)
	if len(nilTasks) != len(OnboardingTasks) {
		t.Fatalf("len = %d, want %d", len(nilTasks), len(OnboardingTasks))
	}
	for _, task := range OnboardingTasks {
		if nilTasks[task.Key] {
			t.Fatalf("%s 应为 false", task.Key)
		}
	}
	mixed := OnboardingNormalizeTasks(map[string]any{
		"first_message": true,
		"pick_skill":    "yes", // 客户端发疯给字符串也得兜住
		"share_soul":    true,
		"unknown_key":   true, // 未知 key 不入表
	})
	if !mixed["first_message"] || !mixed["share_soul"] {
		t.Fatalf("true 值丢失: %v", mixed)
	}
	if mixed["pick_skill"] {
		t.Fatalf("非 true 值应为 false: %v", mixed)
	}
	if len(mixed) != len(OnboardingTasks) {
		t.Fatalf("未知 key 混进来了: %v", mixed)
	}
}

// TestOnboardingClaimAllFreshAccount：新号 8 个任务全部领到 10000 分，
// 请求按官方顺序串行发出，且上报体只带 key。
func TestOnboardingClaimAllFreshAccount(t *testing.T) {
	fake := &FakeUpstream{}
	base := fake.Start()
	defer fake.Stop()

	cfg := makeConfig(t.TempDir(), base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	outcome, err := OnboardingClaimAll(pool.Gateway, "s1")
	if err != nil {
		t.Fatalf("ClaimAll: %v", err)
	}
	if got := jint(outcome, "claimed_points"); got != OnboardingTotal {
		t.Fatalf("claimed_points = %d, want %d", got, OnboardingTotal)
	}
	if got := jint(outcome, "earned"); got != OnboardingTotal {
		t.Fatalf("earned = %d, want %d", got, OnboardingTotal)
	}
	if got := jint(outcome, "total"); got != OnboardingTotal {
		t.Fatalf("total = %d, want %d", got, OnboardingTotal)
	}
	if got := len(outcome["claimed"].([]map[string]any)); got != 8 {
		t.Fatalf("claimed = %d 项, want 8", got)
	}
	if got := len(outcome["skipped"].([]string)); got != 0 {
		t.Fatalf("skipped = %d 项, want 0", got)
	}
	if got := len(outcome["failed"].([]map[string]any)); got != 0 {
		t.Fatalf("failed = %d 项, want 0", got)
	}

	posts := obTestCalls(fake, "/onboarding/tasks/complete")
	if len(posts) != 8 {
		t.Fatalf("complete 请求数 = %d, want 8", len(posts))
	}
	for i, task := range OnboardingTasks {
		if got := jstr(posts[i]["body"].(map[string]any), "key"); got != task.Key {
			t.Fatalf("第 %d 个请求 key = %q, want %q（官方顺序）", i, got, task.Key)
		}
		if len(posts[i]["body"].(map[string]any)) != 1 {
			t.Fatalf("上报体必须只带 key: %v", posts[i]["body"])
		}
	}
}

// TestOnboardingClaimAllSkipsCompleted：已完成的 3 个任务不发请求直接跳过，
// 剩余 5 个按顺序领取，积分本地重算。
func TestOnboardingClaimAllSkipsCompleted(t *testing.T) {
	fake := &FakeUpstream{OnboardingTasks: map[string]bool{
		"first_message": true,
		"pick_skill":    true,
		"share_soul":    true,
	}}
	base := fake.Start()
	defer fake.Stop()

	cfg := makeConfig(t.TempDir(), base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	outcome, err := OnboardingClaimAll(pool.Gateway, "s1")
	if err != nil {
		t.Fatalf("ClaimAll: %v", err)
	}
	if got := len(outcome["skipped"].([]string)); got != 3 {
		t.Fatalf("skipped = %d, want 3", got)
	}
	claimed := outcome["claimed"].([]map[string]any)
	if len(claimed) != 5 {
		t.Fatalf("claimed = %d, want 5", len(claimed))
	}
	// 500+1000+2000 已完成 → 本次应领 10000-3500 = 6500。
	if got := jint(outcome, "claimed_points"); got != 6500 {
		t.Fatalf("claimed_points = %d, want 6500", got)
	}
	if got := jint(outcome, "earned"); got != OnboardingTotal {
		t.Fatalf("earned = %d, want %d", got, OnboardingTotal)
	}

	posts := obTestCalls(fake, "/onboarding/tasks/complete")
	if len(posts) != 5 {
		t.Fatalf("complete 请求数 = %d, want 5（已完成的不该发请求）", len(posts))
	}
	wantKeys := []string{"generate_ppt", "set_schedule", "install_skill", "configure_remote", "create_soul"}
	for i, key := range wantKeys {
		if got := jstr(posts[i]["body"].(map[string]any), "key"); got != key {
			t.Fatalf("第 %d 个请求 key = %q, want %q", i, got, key)
		}
	}
}

// TestOnboardingClaimAllAuthAborts：complete 回 100002（HTTP 200）时整批立即中断，
// 失败项带业务码，不产生后续请求。
func TestOnboardingClaimAllAuthAborts(t *testing.T) {
	fake := &FakeUpstream{OnboardingCompleteCode: OnboardingAuthErrorCode}
	base := fake.Start()
	defer fake.Stop()

	cfg := makeConfig(t.TempDir(), base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	outcome, err := OnboardingClaimAll(pool.Gateway, "s1")
	if err != nil {
		t.Fatalf("ClaimAll: %v", err)
	}
	failed := outcome["failed"].([]map[string]any)
	if len(failed) != 1 {
		t.Fatalf("failed = %d, want 1（auth 失败立即中断）", len(failed))
	}
	if got := jstr(failed[0], "code"); got != OnboardingAuthErrorCode {
		t.Fatalf("code = %q, want %q", got, OnboardingAuthErrorCode)
	}
	if got := len(outcome["claimed"].([]map[string]any)); got != 0 {
		t.Fatalf("claimed = %d, want 0", got)
	}
	if got := len(obTestCalls(fake, "/onboarding/tasks/complete")); got != 1 {
		t.Fatalf("complete 请求数 = %d, want 1", got)
	}
}

// TestOnboardingClaimAllReplayCountsAsClaimed：服务端已置位但客户端状态表还没跟上时
// （alreadyCompleted: true 的重放）同样计入 claimed（幂等）。
func TestOnboardingClaimAllReplayCountsAsClaimed(t *testing.T) {
	fake := &FakeUpstream{OnboardingReplay: true}
	base := fake.Start()
	defer fake.Stop()

	cfg := makeConfig(t.TempDir(), base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	outcome, err := OnboardingClaimAll(pool.Gateway, "s1")
	if err != nil {
		t.Fatalf("ClaimAll: %v", err)
	}
	claimed := outcome["claimed"].([]map[string]any)
	if len(claimed) != 8 {
		t.Fatalf("claimed = %d, want 8（重放算成功）", len(claimed))
	}
	for _, item := range claimed {
		if !jbool(item, "already_completed") {
			t.Fatalf("already_completed = false, want true: %v", item)
		}
	}
	if got := jint(outcome, "claimed_points"); got != OnboardingTotal {
		t.Fatalf("claimed_points = %d, want %d", got, OnboardingTotal)
	}
	if got := jint(outcome, "earned"); got != OnboardingTotal {
		t.Fatalf("earned = %d, want %d", got, OnboardingTotal)
	}
}

// TestOnboardingFetchTasksIgnoresServerEarned：服务端的 earned 不可信，本地重算覆盖。
func TestOnboardingFetchTasksIgnoresServerEarned(t *testing.T) {
	fake := &FakeUpstream{OnboardingTasks: map[string]bool{"first_message": true, "share_soul": true}}
	base := fake.Start()
	defer fake.Stop()

	cfg := makeConfig(t.TempDir(), base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	// 假上游 GET 恒回 earned=0；本地重算应为 500+2000=2500。
	state, err := OnboardingFetchTasks(pool.Gateway, "s1")
	if err != nil {
		t.Fatalf("FetchTasks: %v", err)
	}
	if got := jint(state, "earned"); got != 2500 {
		t.Fatalf("earned = %d, want 2500（本地重算）", got)
	}
	if got := jint(state, "total"); got != OnboardingTotal {
		t.Fatalf("total = %d, want %d", got, OnboardingTotal)
	}
}

// TestOnboardingClaimForAccount：session 有效时不重登，领取后刷新额度并落盘。
func TestOnboardingClaimForAccount(t *testing.T) {
	fake := &FakeUpstream{}
	base := fake.Start()
	defer fake.Stop()

	dir := t.TempDir()
	cfg := makeConfig(dir, base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	client := &FakeAccountClient{}
	pool.Client = client

	acc := NewAccount("fresh")
	acc.Loginid = "13800000000"
	acc.Session = "s1"
	acc.ExpireAt = time.Now().Unix() + 14*86400
	_, err = pool.AddAccount(acc.Name, acc.Loginid, "", acc.Session, "", nil)
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	stored := pool.Get("fresh")

	logs := []string{}
	result := OnboardingClaimForAccount(pool, stored, func(msg string) { logs = append(logs, msg) })

	if !jbool(result, "ok") {
		t.Fatalf("ok = false: %v", result)
	}
	if got := jint(result, "claimed_points"); got != OnboardingTotal {
		t.Fatalf("claimed_points = %d, want %d", got, OnboardingTotal)
	}
	// session 有效 → 不需要重新登录。
	if len(client.Fills) != 0 {
		t.Fatalf("Fills = %v, want 空（session 有效不该重登）", client.Fills)
	}
	// 领取后刷新了额度（假上游 balance=100）。
	if stored.Balance == nil || *stored.Balance != 100 {
		t.Fatalf("Balance = %v, want 100", stored.Balance)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "领取完成") {
		t.Fatalf("日志 = %v, want 一条「领取完成」", logs)
	}
}

// TestOnboardingClaimForAccountSessionDead：session 不可用且没有密码时返回 error 结果，
// 不会 panic。
func TestOnboardingClaimForAccountSessionDead(t *testing.T) {
	fake := &FakeUpstream{}
	base := fake.Start()
	defer fake.Stop()

	cfg := makeConfig(t.TempDir(), base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	pool.Client = &FakeAccountClient{} // Fill 会成功，但 EnsureSession 要求 loginid+password

	acc, err := pool.AddAccount("dead", "", "", "", "", nil)
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	_ = acc

	result := OnboardingClaimForAccount(pool, pool.Get("dead"), nil)
	if jbool(result, "ok") {
		t.Fatalf("ok = true, want false: %v", result)
	}
	if got := jstr(result, "error"); !strings.Contains(got, "session 不可用") {
		t.Fatalf("error = %q, want 含「session 不可用」", got)
	}
}

// TestOnboardingCompleteTaskUnknownKey：未知 key 本地直接拒绝，不发请求。
func TestOnboardingCompleteTaskUnknownKey(t *testing.T) {
	fake := &FakeUpstream{}
	base := fake.Start()
	defer fake.Stop()

	cfg := makeConfig(t.TempDir(), base)
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	_, err = OnboardingCompleteTask(pool.Gateway, "s1", "no_such_task")
	oerr, isOerr := err.(*OnboardingError)
	if !isOerr {
		t.Fatalf("err = %v, want *OnboardingError", err)
	}
	if oerr.Code != OnboardingBadRequestCode {
		t.Fatalf("code = %q, want %q", oerr.Code, OnboardingBadRequestCode)
	}
	if len(fake.CallsCopy()) != 0 {
		t.Fatalf("不该发出任何请求: %v", fake.CallsCopy())
	}
}
