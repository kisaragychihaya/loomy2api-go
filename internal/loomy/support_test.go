package loomy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 移植自 tests/support.py —— 共享测试桩：本地假上游 + 假账号客户端 + 配置/文件工具。
//
// 全部跑在 httptest 起的本地假上游上，套件是自封闭的：不碰真的 Loomy 账号、不花积分、
// 也永远不会访问讯飞端点。后续波次（gateway/server/cli）的测试直接复用这里的类型。

// FakeUpstream 对应 tests/support.py 的 FakeUpstream：本地版的 Loomy 模型网关
// （+ 讯飞账号服务的额度接口）。行为开关让测试可以演练轮换（ValidSessions）、
// 积分耗尽（ExhaustedSessions）和各种错误路径（RejectWith）。
type FakeUpstream struct {
	// ValidSessions 为 nil 时接受任何 token（对齐 Python valid_sessions=None）；
	// 非 nil 的空切片表示全拒。
	ValidSessions     []string
	ExhaustedSessions []string
	// RejectWith 是「model id 子串 → 拒绝响应」，模拟上游抽风。
	RejectWith map[string]struct {
		Status  int
		Message string
	}
	// Calls 记录每个请求 {"path","token","body"}（GET 的 body 为 nil）。
	// 并发追加，整体读取请用 CallsCopy。
	Calls []map[string]any

	ModelsPayload map[string]any
	Quota         map[string]any

	// OnboardingTasks 记录新手任务的完成状态（onboarding 端点用）；nil 表示全部未完成。
	// 与 Quota 一样：测试在请求开始前写入、请求结束后读取，不加锁。
	OnboardingTasks map[string]bool
	// OnboardingCompleteCode 非空时，/onboarding/tasks/complete 固定回这个业务码
	// （HTTP 仍是 200，模拟 100002 session 失效等失败路径）。
	OnboardingCompleteCode string
	// OnboardingReplay 为 true 时，complete 对每个 key 都回 alreadyCompleted: true
	// （服务端已置位、客户端状态表还没跟上的重放场景）。
	OnboardingReplay bool
	// OnboardingTasksCode 非空时，GET /onboarding/tasks 固定回这个业务码
	// （模拟查询阶段就 auth 失败的路径）。
	OnboardingTasksCode string

	mu     sync.Mutex
	server *httptest.Server
	url    string
}

// Start 启动假上游并返回 baseURL（形如 http://127.0.0.1:PORT/api/v1）。
func (f *FakeUpstream) Start() string {
	if f.server != nil {
		return f.url
	}
	if f.ModelsPayload == nil {
		f.ModelsPayload = map[string]any{
			"object": "list",
			"data": []any{map[string]any{
				"id": "fake-model", "object": "model", "name": "Fake (x1.0)",
				"context_length": 1000,
				"capabilities": map[string]any{
					"reasoning":        true,
					"function_calling": true,
				},
			}},
		}
	}
	if f.Quota == nil {
		f.Quota = map[string]any{"balance": 100, "dailyBalance": 50, "availableBalance": 150}
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	f.url = f.server.URL + "/api/v1"
	return f.url
}

// Stop 关闭假上游（可重复调用）。
func (f *FakeUpstream) Stop() {
	if f.server != nil {
		f.server.Close()
	}
}

// URL 返回已启动时的 baseURL（未启动时为空串）。
func (f *FakeUpstream) URL() string {
	return f.url
}

// CallsCopy 返回 Calls 的拷贝，避免读取时和请求 goroutine 抢数据。
func (f *FakeUpstream) CallsCopy() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.Calls))
	copy(out, f.Calls)
	return out
}

// recordCall 对应 Python 的 fake.calls.append(...)。
func (f *FakeUpstream) recordCall(entry map[string]any) {
	f.mu.Lock()
	f.Calls = append(f.Calls, entry)
	f.mu.Unlock()
}

// fakeUpstreamToken 对应 Python 的 _token：Bearer 优先，其次 token 头。
func fakeUpstreamToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return r.Header.Get("token")
}

// sendJSON 对应 Python 的 _send_json。
func (f *FakeUpstream) sendJSON(w http.ResponseWriter, obj any, status int) {
	body, err := json.Marshal(obj)
	if err != nil {
		body = []byte("{}")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// reject 对应 Python 的 _reject：{"error":{"message":...}}。
func (f *FakeUpstream) reject(w http.ResponseWriter, status int, message string) {
	f.sendJSON(w, map[string]any{"error": map[string]any{"message": message}}, status)
}

// rejectInvalid 对应 Python 的鉴权前置：ValidSessions 非 nil 且不含该 token 时 401。
func (f *FakeUpstream) rejectInvalid(w http.ResponseWriter, token string) bool {
	if f.ValidSessions == nil {
		return false
	}
	for _, s := range f.ValidSessions {
		if s == token {
			return false
		}
	}
	f.reject(w, http.StatusUnauthorized, "invalid session")
	return true
}

// exhausted 对应 Python 的 `if token in fake.exhausted_sessions`。
func (f *FakeUpstream) exhausted(token string) bool {
	for _, s := range f.ExhaustedSessions {
		if s == token {
			return true
		}
	}
	return false
}

// handle 是假上游的唯一入口，行为逐条对齐 tests/support.py 的 do_GET / do_POST。
func (f *FakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	token := fakeUpstreamToken(r)
	path := r.URL.Path

	if r.Method == http.MethodGet {
		f.recordCall(map[string]any{"path": path, "token": token, "body": nil})
		if f.rejectInvalid(w, token) {
			return
		}
		switch {
		case strings.HasSuffix(path, "/models"):
			f.sendJSON(w, f.ModelsPayload, http.StatusOK)
		case strings.HasSuffix(path, "/points/records"):
			data := map[string]any{"list": []any{}}
			for k, v := range f.Quota {
				data[k] = v
			}
			f.sendJSON(w, map[string]any{"code": "000000", "desc": "成功", "data": data}, http.StatusOK)
		case strings.HasSuffix(path, "/onboarding/tasks"):
			tasks := map[string]any{}
			for _, task := range OnboardingTasks {
				tasks[task.Key] = f.OnboardingTasks[task.Key]
			}
			code := f.OnboardingTasksCode
			if code == "" {
				code = OnboardingOKCode
			}
			if code != OnboardingOKCode {
				f.sendJSON(w, map[string]any{"code": code, "desc": "失败"}, http.StatusOK)
				return
			}
			f.sendJSON(w, map[string]any{"code": OnboardingOKCode, "desc": "成功", "data": map[string]any{
				"tasks": tasks, "earned": 0, "total": OnboardingTotal,
			}}, http.StatusOK)
		default:
			f.reject(w, http.StatusNotFound, "not found")
		}
		return
	}

	raw, _ := io.ReadAll(r.Body)
	body := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			body = map[string]any{} // Python 的 json.loads 失败也当空 map
		}
	}
	f.recordCall(map[string]any{"path": path, "token": token, "body": body})
	if f.rejectInvalid(w, token) {
		return
	}
	if f.exhausted(token) {
		f.reject(w, http.StatusPaymentRequired, "insufficient points")
		return
	}

	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		model := "fake-model"
		if v, ok := body["model"]; ok && v != nil {
			model = anthStr(v)
		}
		for needle, rej := range f.RejectWith {
			if !strings.Contains(model, needle) {
				continue
			}
			status := rej.Status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			message := rej.Message
			if message == "" {
				message = "denied"
			}
			f.reject(w, status, message)
			return
		}
		if anthTruthy(body["stream"]) {
			f.stream(w, model)
			return
		}
		f.sendJSON(w, map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion", "model": model,
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "echo:" + fakeUpstreamEcho(body),
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": 3, "completion_tokens": 5, "points_consumed": 1,
			},
		}, http.StatusOK)
	case strings.HasSuffix(path, "/images/generations"):
		f.sendJSON(w, map[string]any{"data": []any{map[string]any{"url": "http://x/img.png"}}}, http.StatusOK)
	case strings.HasSuffix(path, "/embeddings"):
		f.sendJSON(w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []any{0.1}}}}, http.StatusOK)
	case strings.HasSuffix(path, "/onboarding/tasks/complete"):
		key := anthStr(body["key"])
		code := f.OnboardingCompleteCode
		if code == "" {
			code = OnboardingOKCode
		}
		if code != OnboardingOKCode {
			// 业务失败（如 100002）：不置位任务。
			f.sendJSON(w, map[string]any{"code": code, "desc": "失败"}, http.StatusOK)
			return
		}
		already := f.OnboardingTasks[key] || f.OnboardingReplay
		if f.OnboardingTasks == nil {
			f.OnboardingTasks = map[string]bool{}
		}
		f.OnboardingTasks[key] = true
		f.sendJSON(w, map[string]any{"code": OnboardingOKCode, "desc": "成功",
			"data": map[string]any{"alreadyCompleted": already, "balance": OnboardingTotal}}, http.StatusOK)
	default:
		f.reject(w, http.StatusNotFound, "not found")
	}
}

// fakeUpstreamEcho 对应 Python 的 f"echo:{body.get('messages', [{}])[-1].get('content')}"。
func fakeUpstreamEcho(body map[string]any) string {
	messages := jlist(body, "messages")
	if len(messages) == 0 {
		return "None" // Python: [{}][-1].get("content") → None → str(None)
	}
	last, isMap := messages[len(messages)-1].(map[string]any)
	if !isMap {
		return "None"
	}
	v, ok := last["content"]
	if !ok || v == nil {
		return "None"
	}
	return anthStr(v)
}

// stream 对应 Python 的 _stream：三个 chunk + [DONE]，每个 chunk 带 id 与 model。
func (f *FakeUpstream) stream(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	chunks := []map[string]any{
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"role": "assistant", "reasoning_content": "think "}}}},
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"content": "hello"}}}},
		{"choices": []any{}, "usage": map[string]any{
			"prompt_tokens": 2, "completion_tokens": 4, "points_consumed": 1}},
	}
	for _, chunk := range chunks {
		payload := map[string]any{"id": "chatcmpl-fake", "model": model}
		for k, v := range chunk {
			payload[k] = v
		}
		line, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ------------------------------------------------------------- 假账号客户端

// FakeAccountClient 对应 tests/support.py 的 FakeAccountClient：AccountClient 的替身
// （不发网络请求）。它记录登录时机，让测试能断言「什么时候续了会话」，并仿造
// identity 绑定。
type FakeAccountClient struct {
	Fills         []string
	Rebinds       []string
	Counter       int
	SessionPrefix string // 默认 "testsession"

	// SMSErr 非 nil 时 SendSMSCode / LoginBySMS 直接返回它（面板短信登录的错误路径用）。
	SMSErr error
	// SMSNoMsgid 为 true 时 SendSMSCode 返回缺少 msgid 的响应。
	SMSNoMsgid bool
}

var _ AccountClientAPI = (*FakeAccountClient)(nil)

// IdentityMode 对应 Python identity_mode。
func (f *FakeAccountClient) IdentityMode() string { return "per_account" }

// EnsureIdentity 对应 Python ensure_identity：没有 identity 时补一个，之后复用。
func (f *FakeAccountClient) EnsureIdentity(acc *Account) map[string]any {
	if len(acc.Identity) == 0 {
		acc.Identity = NewIdentity(f.IdentityMode())
	}
	return acc.Identity
}

// RebindIdentity 对应 Python rebind_identity：换一个全新的设备 identity。
func (f *FakeAccountClient) RebindIdentity(acc *Account) map[string]any {
	acc.Identity = NewIdentity(f.IdentityMode())
	f.Rebinds = append(f.Rebinds, acc.Name)
	return acc.Identity
}

// Fill 对应 Python fill：伪造一次密码登录。
func (f *FakeAccountClient) Fill(acc *Account) error {
	f.Counter++
	f.Fills = append(f.Fills, acc.Name)
	prefix := f.SessionPrefix
	if prefix == "" {
		prefix = "testsession"
	}
	acc.Session = prefix + strconv.Itoa(f.Counter)
	if acc.Userid == "" {
		acc.Userid = "uid-" + acc.Loginid
	}
	acc.ObtainedAt = time.Now().Unix()
	acc.ExpireAt = acc.ObtainedAt + SessionExpireSeconds
	return nil
}

// SendSMSCode 对应 Python send_sms_code（CLI 的短信登录路径用）。
func (f *FakeAccountClient) SendSMSCode(phone string, identity map[string]any) (map[string]any, error) {
	if f.SMSErr != nil {
		return nil, f.SMSErr
	}
	if f.SMSNoMsgid {
		return map[string]any{"code": "000000", "data": map[string]any{}}, nil
	}
	return map[string]any{"code": "000000", "data": map[string]any{"msgid": "msgid-test"}}, nil
}

// LoginBySMS 对应 Python login_by_sms。
func (f *FakeAccountClient) LoginBySMS(phone, code, msgid string, identity map[string]any) (map[string]string, error) {
	if f.SMSErr != nil {
		return nil, f.SMSErr
	}
	return map[string]string{"session": "sms-session", "userid": "uid-sms", "phone": phone}, nil
}

// LoginByPassword 对应 Python login_by_password。
func (f *FakeAccountClient) LoginByPassword(loginid, password string, identity map[string]any) (map[string]string, error) {
	return map[string]string{"session": "pwd-session", "userid": "uid-" + loginid, "phone": loginid}, nil
}

// GetPublicKey 对应 Python get_public_key。
func (f *FakeAccountClient) GetPublicKey(identity map[string]any) (string, string, error) {
	return "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDN", "rcode-test", nil
}

// ------------------------------------------------------------------ cfg/pool

// writeAccounts 对应 tests/support.py 的 write_accounts：写 {"accounts": [...]} 到磁盘。
func writeAccounts(t *testing.T, path string, accounts []map[string]any) {
	t.Helper()
	if accounts == nil {
		accounts = []map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"accounts": accounts})
	if err != nil {
		t.Fatalf("writeAccounts: %v", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("writeAccounts mkdir: %v", err)
		}
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("writeAccounts: %v", err)
	}
}

// makeConfig 对应 tests/support.py 的 make_config：以 dir 为根的一份测试配置。
// 账号服务固定指向 127.0.0.1:1（这些测试从不联系它）。
func makeConfig(dir, upstream string) *Config {
	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 0
	cfg.Upstream = upstream
	cfg.AccountBase = "http://127.0.0.1:1"
	cfg.AccountsFile = filepath.Join(dir, "accounts.json")
	cfg.LogDir = filepath.Join(dir, "logs")
	cfg.SessionsFromClient = false
	cfg.QuotaRefreshMinutes = 999
	cfg.MaxRetries = 2
	cfg.CooldownSeconds = 60
	cfg.LogConsole = false
	cfg.root = dir
	return cfg
}

// freePort 对应 tests/support.py 的 free_port：借内核挑一个空闲端口。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitForPort 对应 tests/support.py 的 wait_for_port：轮询直到端口可连。
func waitForPort(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// ------------------------------------------------------------ 假上游自检

// TestFakeUpstreamServesRequests 验证假上游本身：鉴权、models/points、非流式回显、
// SSE、积分耗尽、RejectWith。后续波次靠它当上游，先在这里钉住行为。
func TestFakeUpstreamServesRequests(t *testing.T) {
	fake := &FakeUpstream{}
	base := fake.Start()
	defer fake.Stop()

	headers := map[string]string{
		"Authorization": "Bearer tok",
		"token":         "tok",
		"Content-Type":  "application/json",
	}
	post := func(path, body string) (int, []byte) {
		t.Helper()
		status, _, data, err := Request(base+path, "POST", headers, []byte(body), 5*time.Second, "")
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return status, data
	}

	status, data := post("/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", status, data)
	}
	var chat map[string]any
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("chat body: %v", err)
	}
	choice := jlist(chat, "choices")[0].(map[string]any)
	message := jmap(choice, "message")
	if got := jstr(message, "content"); got != "echo:hi" {
		t.Fatalf("content = %q, want echo:hi", got)
	}
	if got := jint(jmap(chat, "usage"), "points_consumed"); got != 1 {
		t.Fatalf("points_consumed = %d, want 1", got)
	}

	status, _, models, err := Request(base+"/models", "GET", headers, nil, 5*time.Second, "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("models: status=%d err=%v", status, err)
	}
	var modelsObj map[string]any
	if err := json.Unmarshal(models, &modelsObj); err != nil {
		t.Fatalf("models body: %v", err)
	}
	if got := jstr(jlist(modelsObj, "data")[0].(map[string]any), "id"); got != "fake-model" {
		t.Fatalf("models[0].id = %q", got)
	}

	status, _, points, err := Request(base+"/points/records?page_size=1", "GET", headers, nil, 5*time.Second, "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("points: status=%d err=%v", status, err)
	}
	var pointsObj map[string]any
	if err := json.Unmarshal(points, &pointsObj); err != nil {
		t.Fatalf("points body: %v", err)
	}
	if got := jint(jmap(pointsObj, "data"), "availableBalance"); got != 150 {
		t.Fatalf("availableBalance = %d, want 150", got)
	}

	status, streamed := post("/chat/completions", `{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("stream status = %d", status)
	}
	text := string(streamed)
	for _, want := range []string{`"reasoning_content":"think "`, `"content":"hello"`, `"points_consumed":1`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("SSE 缺少 %q：%s", want, text)
		}
	}

	// token 不匹配 → 401。
	fake.ValidSessions = []string{"other"}
	if status, _ := post("/chat/completions", `{"model":"fake-model"}`); status != http.StatusUnauthorized {
		t.Fatalf("invalid session status = %d, want 401", status)
	}
	fake.ValidSessions = nil

	// 积分耗尽 → 402。
	fake.ExhaustedSessions = []string{"tok"}
	if status, _ := post("/chat/completions", `{"model":"fake-model"}`); status != http.StatusPaymentRequired {
		t.Fatalf("exhausted status = %d, want 402", status)
	}
	fake.ExhaustedSessions = nil

	// model 子串命中 RejectWith → 指定状态码与消息。
	fake.RejectWith = map[string]struct {
		Status  int
		Message string
	}{"boom": {Status: http.StatusBadGateway, Message: "kaboom"}}
	status, data = post("/chat/completions", `{"model":"boom-1"}`)
	if status != http.StatusBadGateway {
		t.Fatalf("reject status = %d, want 502", status)
	}
	if !strings.Contains(string(data), "kaboom") {
		t.Fatalf("reject body = %s", data)
	}

	// Calls 记录：path 去掉 query、GET 的 body 为 nil。
	calls := fake.CallsCopy()
	if len(calls) == 0 {
		t.Fatal("Calls 为空")
	}
	for _, call := range calls {
		if strings.Contains(jstr(call, "path"), "?") {
			t.Fatalf("path 未去掉 query: %v", call["path"])
		}
	}
	if calls[1]["body"] != nil {
		t.Fatalf("GET 的 body 应为 nil, got %v", calls[1]["body"])
	}
}
