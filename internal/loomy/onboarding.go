package loomy

// 对应 Python loomy2api/onboarding.py —— Loomy 新手任务：10 000 积分，纯接口直领。
//
// 上游不校验前置行为：完成条件在官方 Electron 客户端（onboarding-service.js）本地
// 判定，服务端只做「幂等置位 + 加积分」。实测 2026-09-26：8 个裸 POST /complete 就把
// 新号从 0 领到 10 000，一个模型 token 都不花。所以这里是纯 API 领取。
//
// 端点（模型网关 loomyad.xunfei.cn/api/v1）：
//
//	GET  /onboarding/tasks          → {tasks: {key: bool}, earned, total}
//	POST /onboarding/tasks/complete body {key} → {alreadyCompleted, balance}
//
// 从参考实现原样带过来的三条语义（每条都是实测的坑，不是风格选择）：
//
//   - 幂等以响应体的 alreadyCompleted 为准 —— 不看 HTTP 状态、不看 code（重放算成功）；
//   - 服务端的 earned 不可信 —— 积分用 TASK_POINTS 在本地重算（官方客户端同样如此）；
//   - 上报体只带 key —— 不带设备指纹、不带版本、不带渠道。

import (
	"encoding/json"
	"fmt"
)

// 业务码（对应 Python 的 OK_CODE / BAD_REQUEST_CODE / AUTH_ERROR_CODE）。
const (
	OnboardingOKCode         = "000000"
	OnboardingBadRequestCode = "100001"
	// OnboardingAuthErrorCode：session 失效 / 未登录 —— 必须中断整批，而不是继续发注定失败的请求。
	OnboardingAuthErrorCode = "100002"
)

// OnboardingTotal 是 8 个新手任务的积分总和（满分 10 000，对应对应 Python 的 ONBOARDING_TOTAL）。
const OnboardingTotal = 10000

// OnboardingTask 是新手任务表的一行（key → 积分与中文标题）。
// 顺序即官方客户端的领取顺序，ClaimAll 按这个顺序串行请求。
type OnboardingTask struct {
	Key    string
	Points int
	Title  string
}

// OnboardingTasks 对应 Python 的 TASK_POINTS + TASK_TITLES（有序表，合计恰为 10 000）。
var OnboardingTasks = []OnboardingTask{
	{"first_message", 500, "发送你的第一条消息"},
	{"pick_skill", 1000, "试试选择一个技能"},
	{"generate_ppt", 1500, "生成第一份 PPT"},
	{"set_schedule", 1000, "设置定时任务"},
	{"install_skill", 1500, "在技能广场安装一个技能"},
	{"configure_remote", 1000, "配置远程控制"},
	{"create_soul", 1500, "创建你的第一个搭子"},
	{"share_soul", 2000, "把搭子分享给朋友"},
}

// OnboardingTaskPoints 对应 Python 的 TASK_POINTS：key → 积分。
var OnboardingTaskPoints = func() map[string]int {
	out := make(map[string]int, len(OnboardingTasks))
	for _, task := range OnboardingTasks {
		out[task.Key] = task.Points
	}
	return out
}()

// OnboardingTaskTitles 对应 Python 的 TASK_TITLES：key → 客户端注册表里的中文标签。
var OnboardingTaskTitles = func() map[string]string {
	out := make(map[string]string, len(OnboardingTasks))
	for _, task := range OnboardingTasks {
		out[task.Key] = task.Title
	}
	return out
}()

// OnboardingError 是业务级失败（对应 Python 的 OnboardingError）。
// Loomy 永远回 HTTP 200，业务码在响应体里。
type OnboardingError struct {
	Msg       string
	Code      string
	Retryable bool
}

func (e *OnboardingError) Error() string { return e.Msg }

// AuthFailed 对应 Python 的 auth_failed 属性：session 已失效，调用方应中断整批。
func (e *OnboardingError) AuthFailed() bool { return e.Code == OnboardingAuthErrorCode }

// ---------------------------------------------------------------- helpers

// OnboardingComputeEarned 对应 Python compute_earned：已领取积分在本地重算（未知 key 忽略）。
func OnboardingComputeEarned(tasks map[string]bool) int {
	earned := 0
	for _, task := range OnboardingTasks {
		if tasks[task.Key] {
			earned += task.Points
		}
	}
	return earned
}

// OnboardingNormalizeTasks 对应 Python normalize_tasks：只留 8 个已知 key，
// 一律强转成 bool，缺失的补 false。
func OnboardingNormalizeTasks(raw any) map[string]bool {
	source, isMap := raw.(map[string]any)
	out := make(map[string]bool, len(OnboardingTasks))
	for _, task := range OnboardingTasks {
		out[task.Key] = isMap && source[task.Key] == true
	}
	return out
}

// onboardingRequest 对应 Python _request：一次业务调用，拆 {code, desc, data} 信封。
//
// AUTH_ERROR_CODE 原样抛出（Retryable 置位与 Python 一致），让调用方中断整批而不是
// 连发一串注定失败的请求。返回 parsed["data"]（非对象响应对应 Python 的直接返回 parsed）。
func onboardingRequest(gw GatewayAPI, session, path string, payload map[string]any, method string) (any, error) {
	status, _, body, err := gw.Call(session, path, payload, nil, method)
	if err != nil {
		return nil, &OnboardingError{Msg: fmt.Sprintf("%s 请求失败：%v", path, err),
			Code: "NETWORK", Retryable: true}
	}

	var parsed any
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return nil, &OnboardingError{
			Msg: fmt.Sprintf("%s 响应不是 JSON（HTTP %d）：%s", path, status,
				httpcText(body, 200)),
			Code:      fmt.Sprintf("HTTP_%d", status),
			Retryable: status >= 500,
		}
	}

	obj, isObj := parsed.(map[string]any)
	if !isObj {
		return parsed, nil
	}

	code := accountVal(obj, "code")
	if code == "" {
		code = accountVal(obj, "errorCode")
	}
	if code != "" && code != OnboardingOKCode {
		message := accountVal(obj, "desc")
		if message == "" {
			message = accountVal(obj, "message")
		}
		if message == "" {
			message = code
		}
		return nil, &OnboardingError{Msg: fmt.Sprintf("%s 失败：%s", path, message),
			Code:      code,
			Retryable: code == OnboardingBadRequestCode || code == OnboardingAuthErrorCode,
		}
	}
	return obj["data"], nil
}

// ---------------------------------------------------------------- API

// OnboardingFetchTasks 对应 Python fetch_tasks：查询新手任务状态。
//
// earned 在本地重算 —— 服务端的值只是参考，历史上与客户端的表对不上。
// 返回 {"tasks": map[string]bool, "earned": int, "total": int}。
func OnboardingFetchTasks(gw GatewayAPI, session string) (map[string]any, error) {
	data, err := onboardingRequest(gw, session, "onboarding/tasks", nil, "GET")
	if err != nil {
		return nil, err
	}
	record, isMap := data.(map[string]any)
	if !isMap {
		record = map[string]any{}
	}
	tasks := OnboardingNormalizeTasks(record["tasks"])
	total := OnboardingTotal
	if value := pyInt64(record["total"]); value != 0 {
		total = int(value)
	}
	return map[string]any{
		"tasks":  tasks,
		"earned": OnboardingComputeEarned(tasks),
		"total":  total,
	}, nil
}

// OnboardingCompleteTask 对应 Python complete_task：领取单个任务。
// 重放（alreadyCompleted: true）同样算成功（幂等）。
// 返回 {"key","points","title","already_completed","balance"}。
func OnboardingCompleteTask(gw GatewayAPI, session, key string) (map[string]any, error) {
	points, known := OnboardingTaskPoints[key]
	if !known {
		// 与官方客户端一致：本地直接拒绝，不发注定失败的请求。
		return nil, &OnboardingError{Msg: "未知的 task key: " + key, Code: OnboardingBadRequestCode}
	}
	data, err := onboardingRequest(gw, session, "onboarding/tasks/complete",
		map[string]any{"key": key}, "POST")
	if err != nil {
		return nil, err
	}
	record, isMap := data.(map[string]any)
	if !isMap {
		record = map[string]any{}
	}
	var balance any
	if value, ok := record["balance"]; ok && value != nil {
		if _, isNum := poolNum(value); isNum {
			balance = pyInt64(value)
		}
	}
	return map[string]any{
		"key":               key,
		"points":            points,
		"title":             OnboardingTaskTitles[key],
		"already_completed": record["alreadyCompleted"] == true,
		"balance":           balance,
	}, nil
}

// OnboardingClaimAll 对应 Python claim_all：串行领取所有未完成任务，跳过已完成的。
//
// 故意串行（官方客户端亦然）。已完成的 key 不发请求直接跳过；auth 失败立即中断整批。
// 返回 {"claimed": [...], "skipped": [...], "failed": [...], "earned", "total", "claimed_points"}。
func OnboardingClaimAll(gw GatewayAPI, session string) (map[string]any, error) {
	state, err := OnboardingFetchTasks(gw, session)
	if err != nil {
		return nil, err
	}

	tasks := map[string]bool{}
	for key, done := range state["tasks"].(map[string]bool) {
		tasks[key] = done
	}
	claimed := []map[string]any{}
	skipped := []string{}
	failed := []map[string]any{}

	for _, task := range OnboardingTasks {
		if tasks[task.Key] {
			skipped = append(skipped, task.Key)
			continue
		}
		result, err := OnboardingCompleteTask(gw, session, task.Key)
		if err != nil {
			oerr, isOerr := err.(*OnboardingError)
			entry := map[string]any{"key": task.Key, "title": task.Title, "error": err.Error()}
			if isOerr {
				entry["code"] = oerr.Code
			}
			failed = append(failed, entry)
			if isOerr && oerr.AuthFailed() {
				// session 已死，后面的请求必然同样失败。
				break
			}
			continue
		}
		// 重放也计入 claimed（幂等）。
		claimed = append(claimed, result)
		tasks[task.Key] = true
	}

	claimedPoints := 0
	for _, item := range claimed {
		claimedPoints += item["points"].(int)
	}
	return map[string]any{
		"claimed":        claimed,
		"skipped":        skipped,
		"failed":         failed,
		"earned":         OnboardingComputeEarned(tasks),
		"total":          state["total"],
		"claimed_points": claimedPoints,
	}, nil
}

// OnboardingClaimForAccount 对应 Python claim_for_account：确保 session 新鲜 → 领取 → 刷新额度。
//
// 业务失败永不 panic / 返回 error —— 面板与 CLI 都要的是每账号一个的结果对象。
// 返回 {"name","ok","claimed","skipped","failed","claimed_points","earned","total"}
// 或失败时的 {"name","ok":false,"error"[,"code"]}。
func OnboardingClaimForAccount(pool *Pool, acc *Account, log LogFunc) map[string]any {
	if log == nil {
		log = func(string) {}
	}
	result := map[string]any{"name": acc.Name, "ok": false}

	if err := pool.EnsureSession(acc, false); err != nil {
		result["error"] = "session 不可用：" + err.Error()
		return result
	}

	outcome, err := OnboardingClaimAll(pool.Gateway, acc.Session)
	if err != nil {
		result["error"] = err.Error()
		if oerr, isOerr := err.(*OnboardingError); isOerr {
			result["code"] = oerr.Code
			if oerr.AuthFailed() {
				log(fmt.Sprintf("[新手任务] %s session 已失效，请重新登录", acc.Name))
			}
		}
		return result
	}

	result["ok"] = true
	for _, key := range []string{"claimed", "skipped", "failed", "claimed_points", "earned", "total"} {
		result[key] = outcome[key]
	}
	log(fmt.Sprintf("[新手任务] %s 领取完成：本次 +%d 积分，已完成 %d/%d，累计 %d/%d",
		acc.Name, outcome["claimed_points"], len(outcome["skipped"].([]string)),
		len(OnboardingTasks), outcome["earned"], outcome["total"]))

	pool.RefreshQuota(acc)
	pool.Save(false)
	return result
}
