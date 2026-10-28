package loomy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 移植自 tests/test_gateway.py 与 tests/test_panel.py 的 HTTP 层用例。
//
// 与 Python 版一一对应：FakeUpstream 顶替真上游，httptest.NewServer(gw.Handler())
// 顶替真端口上的 ThreadingHTTPServer + urllib。断言里的状态码、JSON 字段、SSE 事件
// 序列、错误消息子串与 auth 日志都保持与 Python 一致；唯一的形式差异是 Go 的
// FakeUpstream 用 encoding/json 输出紧凑 JSON（无 ", " 分隔符），所以流式断言按
// 紧凑写法检查。

// svrTestHarness 对应 Python 的 _Harness / PanelHarness。
type svrTestHarness struct {
	t        *testing.T
	dir      string
	upstream *FakeUpstream
	cfg      *Config
	gw       *Gateway
	server   *httptest.Server
	base     string
	logPath  string
}

// svrTestNewHarness 搭一套自带假上游的网关，并在随机的本地端口上跑真 HTTP。
func svrTestNewHarness(t *testing.T) *svrTestHarness {
	t.Helper()

	dir := t.TempDir()
	upstream := &FakeUpstream{}
	base := upstream.Start()
	t.Cleanup(upstream.Stop)

	expire := time.Now().Unix() + 14*86400
	writeAccounts(t, filepath.Join(dir, "accounts.json"), []map[string]any{
		{"name": "a", "session": "s1", "expireAt": expire},
		{"name": "b", "session": "s2", "expireAt": expire},
	})
	cfg := makeConfig(dir, base)

	logDir := filepath.Join(dir, "logs")
	gw, err := NewGateway(cfg, NewLogger(logDir, "", false))
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.Pool().Bootstrap()
	gw.RefreshModels()

	server := httptest.NewServer(gw.Handler())
	t.Cleanup(server.Close)

	return &svrTestHarness{
		t:        t,
		dir:      dir,
		upstream: upstream,
		cfg:      cfg,
		gw:       gw,
		server:   server,
		base:     server.URL,
		logPath:  filepath.Join(logDir, "gateway.log"),
	}
}

// do 发一个请求并读回完整响应（>=400 也算正常返回，和 Python 的 HTTPError 对应）。
func (h *svrTestHarness) do(method, path string, body []byte, headers map[string]string) (int, []byte, http.Header) {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.base+path, reader)
	if err != nil {
		h.t.Fatalf("%s %s: new request: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, data, resp.Header
}

func (h *svrTestHarness) post(path string, payload any, headers map[string]string) (int, []byte, http.Header) {
	h.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("POST %s: marshal payload: %v", path, err)
	}
	return h.do(http.MethodPost, path, body, headers)
}

func (h *svrTestHarness) get(path string, headers map[string]string) (int, []byte, http.Header) {
	h.t.Helper()
	return h.do(http.MethodGet, path, nil, headers)
}

func (h *svrTestHarness) getJSON(path string, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	status, body, _ := h.get(path, headers)
	return status, svrTestDecode(h.t, path, body)
}

// svrTestDecode 解析 JSON 响应体。
func svrTestDecode(t *testing.T, label string, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: body is not a JSON object: %v (%s)", label, err, body)
	}
	return out
}

// svrTestEvent 是解析出来的一个 SSE 帧。
type svrTestEvent struct {
	Event string
	Data  string
}

// svrTestParseSSE 对应 Python 的 _Harness.sse：按空行切块，取 event:/data: 两行。
func svrTestParseSSE(body []byte) []svrTestEvent {
	events := []svrTestEvent{}
	for _, block := range strings.Split(string(body), "\n\n") {
		event, data := "", ""
		found := false
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.HasPrefix(line, "event: ") {
				event, found = line[len("event: "):], true
			} else if strings.HasPrefix(line, "data: ") {
				data, found = line[len("data: "):], true
			}
		}
		if found {
			events = append(events, svrTestEvent{Event: event, Data: data})
		}
	}
	return events
}

// svrTestRawPost 用裸 TCP 发请求：Go 的 http.Client 会把头值前后空白 trim 掉，
// 而 Python 的 urllib 不会 —— 「带空白/引号的 key」这条用例必须走原始字节。
func svrTestRawPost(t *testing.T, base, path string, body []byte, headerLines []string) (int, []byte) {
	t.Helper()

	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("raw post: parse base %q: %v", base, err)
	}
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatalf("raw post: dial %s: %v", parsed.Host, err)
	}
	defer conn.Close()

	var head strings.Builder
	head.WriteString("POST " + path + " HTTP/1.1\r\n")
	head.WriteString("Host: " + parsed.Host + "\r\n")
	head.WriteString("Content-Type: application/json\r\n")
	for _, line := range headerLines {
		head.WriteString(line + "\r\n")
	}
	head.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	head.WriteString("Connection: close\r\n\r\n")
	if _, err := io.WriteString(conn, head.String()); err != nil {
		t.Fatalf("raw post: write head: %v", err)
	}
	if _, err := conn.Write(body); err != nil {
		t.Fatalf("raw post: write body: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("raw post: read response: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// svrTestInt64Ptr 造 *int64（账号额度用指针区分「未知」与「零」）。
func svrTestInt64Ptr(v int64) *int64 { return &v }

// ---------------------------------------------------------------- OpenAI

// TestHealth 对应 test_health。
func TestHealth(t *testing.T) {
	h := svrTestNewHarness(t)

	status, payload := h.getJSON("/health", nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := jstr(payload, "status"); got != "ok" {
		t.Fatalf("status field = %q, want ok", got)
	}
	if got := jint(payload, "accounts"); got != 2 {
		t.Fatalf("accounts = %d, want 2", got)
	}
	if got := jint(payload, "usable_accounts"); got != 2 {
		t.Fatalf("usable_accounts = %d, want 2", got)
	}
	if jbool(payload, "auth_required") {
		t.Fatal("auth_required = true, want false")
	}
}

// TestModelsListsTheUpstreamCatalogue 对应 test_models_lists_the_upstream_catalogue。
func TestModelsListsTheUpstreamCatalogue(t *testing.T) {
	h := svrTestNewHarness(t)

	_, payload := h.getJSON("/v1/models", nil)
	ids := []string{}
	for _, item := range jlist(payload, "data") {
		model, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		ids = append(ids, jstr(model, "id"))
		if owned := jstr(model, "owned_by"); owned == "alias" {
			t.Fatalf("model %v 带了 alias 的 owned_by", model["id"])
		}
	}
	found := false
	for _, id := range ids {
		if id == "fake-model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ids = %v, want 含 fake-model", ids)
	}
}

// TestChatNonStream 对应 test_chat_non_stream。
func TestChatNonStream(t *testing.T) {
	h := svrTestNewHarness(t)

	status, body, _ := h.post("/v1/chat/completions", map[string]any{
		"model":    "imodel/fake-model",
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	}, nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
	payload := svrTestDecode(t, "chat", body)
	if got := jstr(payload, "model"); got != "fake-model" {
		t.Fatalf("model = %q, want fake-model（前缀应被剥掉）", got)
	}
	choices := jlist(payload, "choices")
	if len(choices) == 0 {
		t.Fatalf("choices 为空：%s", body)
	}
	choice, _ := choices[0].(map[string]any)
	if got := jstr(jmap(choice, "message"), "content"); got != "echo:ping" {
		t.Fatalf("content = %q, want echo:ping", got)
	}
	if got := jint(jmap(payload, "usage"), "points_consumed"); got != 1 {
		t.Fatalf("points_consumed = %d, want 1", got)
	}
}

// TestModelNameIsPassedThroughVerbatim 对应 test_model_name_is_passed_through_verbatim。
func TestModelNameIsPassedThroughVerbatim(t *testing.T) {
	h := svrTestNewHarness(t)

	_, body, _ := h.post("/v1/chat/completions", map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}, nil)
	if got := jstr(svrTestDecode(t, "chat", body), "model"); got != "gpt-4o-mini" {
		t.Fatalf("model = %q, want gpt-4o-mini（没有别名表，原样透传）", got)
	}
}

// TestMissingModelFallsBackToDefault 对应 test_missing_model_falls_back_to_default。
func TestMissingModelFallsBackToDefault(t *testing.T) {
	h := svrTestNewHarness(t)

	_, body, _ := h.post("/v1/chat/completions", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}, nil)
	if got := jstr(svrTestDecode(t, "chat", body), "model"); got != h.cfg.DefaultModel {
		t.Fatalf("model = %q, want %q", got, h.cfg.DefaultModel)
	}
}

// TestChatStream 对应 test_chat_stream。
func TestChatStream(t *testing.T) {
	h := svrTestNewHarness(t)

	_, body, _ := h.post("/v1/chat/completions", map[string]any{
		"model":    "fake-model",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	text := string(body)
	for _, want := range []string{"data: ", `"content":"hello"`, `"points_consumed":1`} {
		if !strings.Contains(text, want) {
			t.Fatalf("SSE 缺少 %q：%s", want, text)
		}
	}
	if !strings.HasSuffix(strings.TrimRight(text, " \t\r\n"), "data: [DONE]") {
		t.Fatalf("SSE 没有以 data: [DONE] 结尾：%s", text)
	}
}

// TestMessagesNonStream 对应 test_messages_non_stream。
func TestMessagesNonStream(t *testing.T) {
	h := svrTestNewHarness(t)

	_, body, _ := h.post("/v1/messages", map[string]any{
		"model":      "fake-model",
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	payload := svrTestDecode(t, "messages", body)
	if got := jstr(payload, "type"); got != "message" {
		t.Fatalf("type = %q, want message", got)
	}
	content := jlist(payload, "content")
	if len(content) == 0 {
		t.Fatalf("content 为空：%s", body)
	}
	block, _ := content[0].(map[string]any)
	if got := jstr(block, "type"); got != "text" {
		t.Fatalf("content[0].type = %q, want text", got)
	}
	if got := jstr(block, "text"); got != "echo:hi" {
		t.Fatalf("content[0].text = %q, want echo:hi", got)
	}
	if got := jint(jmap(payload, "usage"), "output_tokens"); got != 5 {
		t.Fatalf("usage.output_tokens = %d, want 5", got)
	}
}

// TestMessagesStream 对应 test_messages_stream。
func TestMessagesStream(t *testing.T) {
	h := svrTestNewHarness(t)

	_, body, _ := h.post("/v1/messages", map[string]any{
		"model":      "fake-model",
		"stream":     true,
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	events := svrTestParseSSE(body)
	if len(events) == 0 {
		t.Fatalf("没有解析出任何 SSE 事件：%s", body)
	}
	names := make([]string, 0, len(events))
	for _, ev := range events {
		names = append(names, ev.Event)
	}
	if names[0] != "message_start" {
		t.Fatalf("第一个事件 = %q, want message_start（%v）", names[0], names)
	}
	if names[len(names)-1] != "message_stop" {
		t.Fatalf("最后一个事件 = %q, want message_stop（%v）", names[len(names)-1], names)
	}

	hasDelta := false
	for _, name := range names {
		if name == "content_block_delta" {
			hasDelta = true
		}
	}
	if !hasDelta {
		t.Fatalf("事件里没有 content_block_delta：%v", names)
	}

	thinking, text := false, false
	for _, ev := range events {
		if ev.Event != "content_block_delta" {
			continue
		}
		delta := jmap(svrTestDecode(t, "sse delta", []byte(ev.Data)), "delta")
		switch jstr(delta, "type") {
		case "thinking_delta":
			if got := jstr(delta, "thinking"); got == "think " {
				thinking = true
			}
		case "text_delta":
			if got := jstr(delta, "text"); got == "hello" {
				text = true
			}
		}
	}
	if !thinking {
		t.Fatal("没有 thinking_delta=think ")
	}
	if !text {
		t.Fatal("没有 text_delta=hello")
	}
}

// TestPointsEndpoint 对应 test_points_endpoint。
func TestPointsEndpoint(t *testing.T) {
	h := svrTestNewHarness(t)

	_, payload := h.getJSON("/v1/points", nil)
	if got := jint(payload, "total_available"); got != 300 { // 150 × 2 个账号
		t.Fatalf("total_available = %d, want 300", got)
	}
	if got := len(jlist(payload, "accounts")); got != 2 {
		t.Fatalf("accounts = %d 个, want 2", got)
	}
	if got := jint(payload, "unique_accounts"); got != 2 {
		t.Fatalf("unique_accounts = %d, want 2", got)
	}
}

// TestPointsDedupesSameUser 对应 test_points_dedupes_same_user。
func TestPointsDedupesSameUser(t *testing.T) {
	h := svrTestNewHarness(t)

	h.gw.Pool().Get("a").Userid = "u1"
	h.gw.Pool().Get("b").Userid = "u1"

	_, payload := h.getJSON("/v1/points", nil)
	if got := jint(payload, "total_available"); got != 150 {
		t.Fatalf("total_available = %d, want 150（同一账号只算一次）", got)
	}
	if got := jint(payload, "unique_accounts"); got != 1 {
		t.Fatalf("unique_accounts = %d, want 1", got)
	}
}

// TestEmbeddingsAndImagesPassthrough 对应 test_embeddings_and_images_passthrough。
func TestEmbeddingsAndImagesPassthrough(t *testing.T) {
	h := svrTestNewHarness(t)

	_, body, _ := h.post("/v1/embeddings", map[string]any{
		"model": "fake-model", "input": []any{"a"},
	}, nil)
	embeddings := jlist(svrTestDecode(t, "embeddings", body), "data")
	if len(embeddings) == 0 {
		t.Fatalf("embeddings.data 为空：%s", body)
	}
	first, _ := embeddings[0].(map[string]any)
	vector, _ := first["embedding"].([]any)
	if len(vector) != 1 || vector[0] != 0.1 {
		t.Fatalf("embedding = %v, want [0.1]", first["embedding"])
	}

	_, body, _ = h.post("/v1/images/generations", map[string]any{
		"model": "fake-model", "prompt": "cat",
	}, nil)
	images := jlist(svrTestDecode(t, "images", body), "data")
	if len(images) == 0 {
		t.Fatalf("images.data 为空：%s", body)
	}
	image, _ := images[0].(map[string]any)
	if jstr(image, "url") == "" {
		t.Fatalf("images.data[0] 没有 url：%v", image)
	}
}

// TestRotationOnRejectedSession 对应 test_rotation_on_rejected_session。
func TestRotationOnRejectedSession(t *testing.T) {
	h := svrTestNewHarness(t)

	// 把选号结果钉死：积分最多的账号先被试。
	h.gw.Pool().Get("a").Available = svrTestInt64Ptr(1000)
	h.gw.Pool().Get("b").Available = svrTestInt64Ptr(10)
	h.upstream.ValidSessions = []string{"s2"} // 账号 a 的会话已经作废

	status, body, _ := h.post("/v1/chat/completions", map[string]any{
		"model":    "fake-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}, nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
	choices := jlist(svrTestDecode(t, "chat", body), "choices")
	choice, _ := choices[0].(map[string]any)
	if got := jstr(jmap(choice, "message"), "content"); got != "echo:x" {
		t.Fatalf("content = %q, want echo:x", got)
	}

	accA := h.gw.Pool().Get("a")
	if accA.SessionValid() {
		t.Fatal("被拒绝的账号 a 应该丢掉会话")
	}
	if accA.Failures == 0 {
		t.Fatal("被拒绝的账号 a 应该记一次失败")
	}
	if !accA.InCooldown() {
		t.Fatal("被拒绝的账号 a 应该进入冷却")
	}
}

// TestUpstream5xxDoesNotCoolDown 对应 test_upstream_5xx_does_not_cool_down_the_account。
func TestUpstream5xxDoesNotCoolDown(t *testing.T) {
	h := svrTestNewHarness(t)

	h.upstream.RejectWith = map[string]struct {
		Status  int
		Message string
	}{"fake-model": {Status: 504, Message: "upstream hiccup"}}

	status, body, _ := h.post("/v1/chat/completions", map[string]any{
		"model":    "fake-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}, nil)
	if status != 504 {
		t.Fatalf("status = %d, want 504 (%s)", status, body)
	}
	accounts := h.gw.Pool().Accounts()
	if got := len(h.gw.Pool().Usable()); got != len(accounts) {
		t.Fatalf("可用账号 %d 个, want %d（5xx 不该影响可用性）", got, len(accounts))
	}
	for _, acc := range accounts {
		if acc.InCooldown() {
			t.Fatalf("账号 %s 进了冷却：5xx 不是账号的错", acc.Name)
		}
		if !acc.SessionValid() {
			t.Fatalf("账号 %s 的会话被丢掉了：5xx 不该丢会话", acc.Name)
		}
	}
}

// TestRotationWhenAccountQuotaExhausted 对应 test_rotation_when_account_quota_exhausted。
func TestRotationWhenAccountQuotaExhausted(t *testing.T) {
	h := svrTestNewHarness(t)

	h.upstream.ExhaustedSessions = []string{"s1", "s2"}
	status, body, _ := h.post("/v1/chat/completions", map[string]any{
		"model":    "fake-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}, nil)
	if status != 402 {
		t.Fatalf("status = %d, want 402 (%s)", status, body)
	}
}

// TestStreamFallsBackToHealthyAccount 对应 test_stream_falls_back_to_healthy_account。
func TestStreamFallsBackToHealthyAccount(t *testing.T) {
	h := svrTestNewHarness(t)

	h.upstream.ValidSessions = []string{"s2"}
	_, body, _ := h.post("/v1/chat/completions", map[string]any{
		"model":    "fake-model",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("流式响应里没有 hello：%s", body)
	}
}

// TestBadJSONReturns400 对应 test_bad_json_returns_400。
func TestBadJSONReturns400(t *testing.T) {
	h := svrTestNewHarness(t)

	status, body, _ := h.do(http.MethodPost, "/v1/chat/completions", []byte("{not json"), nil)
	if status != 400 {
		t.Fatalf("status = %d, want 400 (%s)", status, body)
	}
	if !strings.Contains(string(body), "JSON") {
		t.Fatalf("错误消息里没有 JSON：%s", body)
	}
}

// TestUnknownPath404 对应 test_unknown_path_404。
func TestUnknownPath404(t *testing.T) {
	h := svrTestNewHarness(t)

	status, body, _ := h.post("/v1/nope", map[string]any{}, nil)
	if status != 404 {
		t.Fatalf("status = %d, want 404 (%s)", status, body)
	}
}

// TestAdminAccounts 对应 test_admin_accounts。
func TestAdminAccounts(t *testing.T) {
	h := svrTestNewHarness(t)

	_, body, _ := h.get("/v1/admin/accounts", nil)
	payload := svrTestDecode(t, "admin accounts", body)
	if got := jint(payload, "count"); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	if strings.Contains(string(body), `"password":`) {
		t.Fatalf("admin 视图里不该出现 password：%s", body)
	}
}

// ------------------------------------------------------------------ auth

// svrTestAuthHarness 对应 Python 的 AuthTestCase：同一套网关，但配上 API key。
func svrTestAuthHarness(t *testing.T, keys ...string) *svrTestHarness {
	t.Helper()
	h := svrTestNewHarness(t)
	h.cfg.APIKeys = keys
	return h
}

func svrTestChatPayload() map[string]any {
	return map[string]any{
		"model":    "fake-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
}

// TestMissingKeyRejected 对应 test_missing_key_rejected。
func TestMissingKeyRejected(t *testing.T) {
	h := svrTestAuthHarness(t, "secret-key")

	status, body, _ := h.post("/v1/chat/completions", svrTestChatPayload(), nil)
	if status != 401 {
		t.Fatalf("status = %d, want 401 (%s)", status, body)
	}
}

// TestBearerKeyAccepted 对应 test_bearer_key_accepted。
func TestBearerKeyAccepted(t *testing.T) {
	h := svrTestAuthHarness(t, "secret-key")

	status, body, _ := h.post("/v1/chat/completions", svrTestChatPayload(),
		map[string]string{"Authorization": "Bearer secret-key"})
	if status != 200 {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
}

// TestXAPIKeyAccepted 对应 test_x_api_key_accepted。
func TestXAPIKeyAccepted(t *testing.T) {
	h := svrTestAuthHarness(t, "secret-key")

	status, body, _ := h.post("/v1/chat/completions", svrTestChatPayload(),
		map[string]string{"x-api-key": "secret-key"})
	if status != 200 {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
}

// TestHealthStaysPublic 对应 test_health_stays_public。
func TestHealthStaysPublic(t *testing.T) {
	h := svrTestAuthHarness(t, "secret-key")

	if status, _ := h.getJSON("/health", nil); status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
}

// TestBareAuthorizationWithoutBearer 对应 test_bare_authorization_without_bearer。
func TestBareAuthorizationWithoutBearer(t *testing.T) {
	h := svrTestAuthHarness(t, "secret-key")

	status, body, _ := h.post("/v1/chat/completions", svrTestChatPayload(),
		map[string]string{"Authorization": "secret-key"})
	if status != 200 {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
}

// TestQueryStringKey 对应 test_query_string_key。
func TestQueryStringKey(t *testing.T) {
	h := svrTestAuthHarness(t, "secret-key")

	status, body, _ := h.post("/v1/chat/completions?api_key=secret-key",
		svrTestChatPayload(), nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
}

// TestKeyWithStrayWhitespaceOrQuotes 对应 test_key_with_stray_whitespace_or_quotes。
func TestKeyWithStrayWhitespaceOrQuotes(t *testing.T) {
	h := svrTestAuthHarness(t, "secret-key")

	body, err := json.Marshal(svrTestChatPayload())
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	for _, raw := range []string{` "secret-key" `, "bearer secret-key ", " secret-key\t"} {
		status, respBody := svrTestRawPost(t, h.base, "/v1/chat/completions", body,
			[]string{"Authorization:  " + raw})
		if status != 200 {
			t.Fatalf("Authorization: %q → status = %d, want 200 (%s)", raw, status, respBody)
		}
	}
}

// TestWrongKeyRejectedAndLogged 对应 test_wrong_key_rejected_and_logged。
func TestWrongKeyRejectedAndLogged(t *testing.T) {
	h := svrTestAuthHarness(t, "secret-key")

	status, body, _ := h.post("/v1/chat/completions", svrTestChatPayload(),
		map[string]string{"Authorization": "Bearer wrong-key-123456"})
	if status != 401 {
		t.Fatalf("status = %d, want 401 (%s)", status, body)
	}

	raw, err := os.ReadFile(h.logPath)
	if err != nil {
		t.Fatalf("读日志失败 %s: %v", h.logPath, err)
	}
	logged := string(raw)
	if !strings.Contains(logged, "[auth] 拒绝") {
		t.Fatalf("日志里没有拒绝记录：%s", logged)
	}
	if !strings.Contains(logged, "authorization:bearer") {
		t.Fatalf("日志里没有来源：%s", logged)
	}
	if !strings.Contains(logged, "wrong-ke…") {
		t.Fatalf("日志里没有打码后的 key：%s", logged)
	}
	if strings.Contains(logged, "wrong-key-123456") {
		t.Fatalf("日志泄露了完整密钥：%s", logged)
	}
}

// ----------------------------------------------------------------- panel

// TestPageIsServed 对应 test_page_is_served。
func TestPageIsServed(t *testing.T) {
	h := svrTestNewHarness(t)

	status, body, headers := h.get("/panel", nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	text := string(body)
	if !strings.Contains(text, "loomy2api") {
		t.Fatalf("页面里没有 loomy2api：%s", text)
	}
	if !strings.Contains(text, "/api/panel/state") {
		t.Fatalf("页面里没有 /api/panel/state：%s", text)
	}
	if got := headers.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("Content-Type = %q, want 含 text/html", got)
	}
}

// TestRootServesThePage 对应 test_root_serves_the_page。
func TestRootServesThePage(t *testing.T) {
	h := svrTestNewHarness(t)

	status, body, _ := h.get("/", nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(string(body), "loomy2api") {
		t.Fatalf("根路径没有返回面板：%s", body)
	}
}

// TestHealthAdvertisesThePanel 对应 test_health_advertises_the_panel。
func TestHealthAdvertisesThePanel(t *testing.T) {
	h := svrTestNewHarness(t)

	_, payload := h.getJSON("/health", nil)
	panel := jstr(payload, "panel")
	if !strings.HasSuffix(panel, "/panel") {
		t.Fatalf("panel = %q, want 以 /panel 结尾", panel)
	}
}

// TestStateRequiresKey 对应 test_state_requires_key。
func TestStateRequiresKey(t *testing.T) {
	h := svrTestAuthHarness(t, "panel-key")

	status, _ := h.getJSON("/api/panel/state", nil)
	if status != 401 {
		t.Fatalf("status = %d, want 401", status)
	}
}

// TestStateWithKey 对应 test_state_with_key。
func TestStateWithKey(t *testing.T) {
	h := svrTestAuthHarness(t, "panel-key")

	status, payload := h.getJSON("/api/panel/state",
		map[string]string{"Authorization": "Bearer panel-key"})
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if !jbool(payload, "ok") {
		t.Fatalf("ok = false：%v", payload)
	}
	if !jbool(jmap(payload, "config"), "auth_required") {
		t.Fatalf("config.auth_required = false：%v", payload["config"])
	}
}

// TestPanelModelsListsCatalogue：面板「模型」页的 GET /api/panel/models 返回模型目录，
// 内容与 /v1/models 同源（假上游目录里含 fake-model）。
func TestPanelModelsListsCatalogue(t *testing.T) {
	h := svrTestNewHarness(t)

	status, payload := h.getJSON("/api/panel/models", nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if !jbool(payload, "ok") {
		t.Fatalf("ok = false：%v", payload)
	}
	models, isList := payload["models"].([]any)
	if !isList {
		t.Fatalf("models 不是数组：%v", payload["models"])
	}
	if got := jint(payload, "count"); got < 1 {
		t.Fatalf("count = %d, want >= 1", got)
	}
	if got, want := int64(len(models)), jint(payload, "count"); got != want {
		t.Fatalf("len(models) = %d, count = %d", got, want)
	}

	found := false
	for _, item := range models {
		model, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		if jstr(model, "id") == "fake-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("models = %v, want 含 id=fake-model", payload["models"])
	}
}

// TestPanelModelsRequiresKey：模型目录和别的 /api/panel/* 一样受 API Key 保护。
func TestPanelModelsRequiresKey(t *testing.T) {
	h := svrTestAuthHarness(t, "panel-key")

	status, _ := h.getJSON("/api/panel/models", nil)
	if status != 401 {
		t.Fatalf("status = %d, want 401", status)
	}
}

// TestPanelDocsServesHTML：面板「文档」页拿到的是一段完整、自包含的 HTML 片段
// （不含 html/head/body），首节是「快速开始」，并且带够了排版用的 d- 前缀 class。
func TestPanelDocsServesHTML(t *testing.T) {
	h := svrTestNewHarness(t)

	status, payload := h.getJSON("/api/panel/docs", nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if !jbool(payload, "ok") {
		t.Fatalf("ok = false：%v", payload)
	}
	html := jstr(payload, "html")
	if html == "" {
		t.Fatal("html 为空")
	}

	for _, want := range []string{"快速开始", `<div class="docs">`, "<h2", "接口速查"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html 里没有 %q", want)
		}
	}
	// 片段契约：不能是整页，也不能残留文档外壳。
	for _, bad := range []string{"<html", "<head", "<body", "<!DOCTYPE"} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Fatalf("html 里不该出现 %q（应是片段，不是整页）", bad)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(html), "</div>") {
		t.Fatal("html 不以 </div> 收尾，片段可能被截断")
	}
}

// TestPanelDocsRequiresKey：文档和别的 /api/panel/* 一样在 auth 门之后。
func TestPanelDocsRequiresKey(t *testing.T) {
	h := svrTestAuthHarness(t, "panel-key")

	status, _ := h.getJSON("/api/panel/docs", nil)
	if status != 401 {
		t.Fatalf("status = %d, want 401", status)
	}
}

// TestWritesRequireKey 对应 test_writes_require_key。
func TestWritesRequireKey(t *testing.T) {
	h := svrTestAuthHarness(t, "panel-key")

	status, body, _ := h.post("/api/panel/accounts/remove", map[string]any{"name": "a"}, nil)
	if status != 401 {
		t.Fatalf("status = %d, want 401 (%s)", status, body)
	}
	if h.gw.Pool().Get("a") == nil {
		t.Fatal("未鉴权的写请求不应该删掉账号")
	}
}

// TestPageItselfStaysPublic 对应 test_page_itself_stays_public。
func TestPageItselfStaysPublic(t *testing.T) {
	h := svrTestAuthHarness(t, "panel-key")

	if status, _, _ := h.get("/panel", nil); status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
}

// TestUnknownPanelEndpoint404 对应 test_unknown_panel_endpoint_404。
func TestUnknownPanelEndpoint404(t *testing.T) {
	h := svrTestNewHarness(t)

	status, body, _ := h.post("/api/panel/nonsense", map[string]any{}, nil)
	if status != 404 {
		t.Fatalf("status = %d, want 404 (%s)", status, body)
	}
}

// ------------------------------------------- 额外覆盖（Python 套件里没有对应用例）

// TestPathNormalization 覆盖 _path 的几条改写规则：缺 /v1 前缀要补、重复的 /v1 折叠、
// 尾部斜杠去掉，而面板 / api / health / admin / favicon 这些豁免面保持原样。
func TestPathNormalization(t *testing.T) {
	h := svrTestNewHarness(t)

	for _, path := range []string{"/chat/completions", "/v1/v1/chat/completions", "/v1/chat/completions/"} {
		status, body, _ := h.post(path, svrTestChatPayload(), nil)
		if status != 200 {
			t.Fatalf("POST %s → status %d, want 200 (%s)", path, status, body)
		}
	}

	// 豁免面不补前缀：/panels 不是 /panel，仍按未知路径处理。
	if status, _, _ := h.post("/panels", map[string]any{}, nil); status != 404 {
		t.Fatalf("POST /panels → status %d, want 404", status)
	}
	if status, body, _ := h.get("/health/", nil); status != 200 {
		t.Fatalf("GET /health/ → status %d, want 200 (%s)", status, body)
	}
}

// TestOptionsPreflightAndHead 覆盖 do_OPTIONS（204 + CORS）与 HEAD 按 GET 处理。
func TestOptionsPreflightAndHead(t *testing.T) {
	h := svrTestNewHarness(t)

	status, _, headers := h.do(http.MethodOptions, "/v1/chat/completions", nil, nil)
	if status != 204 {
		t.Fatalf("OPTIONS → status %d, want 204", status)
	}
	if got := headers.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("OPTIONS Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := headers.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("OPTIONS Access-Control-Allow-Methods = %q", got)
	}

	status, body, _ := h.do(http.MethodHead, "/health", nil, nil)
	if status != 200 {
		t.Fatalf("HEAD /health → status %d, want 200", status)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD 响应不应有 body，got %d 字节", len(body))
	}

	if status, _, _ := h.do(http.MethodPut, "/v1/chat/completions", nil, nil); status != 501 {
		t.Fatalf("PUT → status %d, want 501", status)
	}
}

// TestFaviconAndAdminReload 覆盖 /favicon.ico 的 404 与 /v1/admin/accounts/reload。
func TestFaviconAndAdminReload(t *testing.T) {
	h := svrTestNewHarness(t)

	if status, _, _ := h.get("/favicon.ico", nil); status != 404 {
		t.Fatalf("GET /favicon.ico → status %d, want 404", status)
	}

	status, body, _ := h.post("/v1/admin/accounts/reload", map[string]any{}, nil)
	if status != 200 {
		t.Fatalf("POST reload → status %d, want 200 (%s)", status, body)
	}
	if got := jint(svrTestDecode(t, "reload", body), "count"); got != 2 {
		t.Fatalf("reload count = %d, want 2", got)
	}
}

// TestChatIdFieldsAreMovedToHeaders 覆盖 _chat 里的字段搬家：chat_id / msg_id /
// turn_id / request_purpose 从请求体里摘掉、改走上游请求头。
func TestChatIdFieldsAreMovedToHeaders(t *testing.T) {
	h := svrTestNewHarness(t)

	payload := svrTestChatPayload()
	payload["chat_id"] = "c-1"
	payload["msg_id"] = "m-1"
	payload["turn_id"] = "t-1"
	payload["request_purpose"] = "chat.title"
	if status, body, _ := h.post("/v1/chat/completions", payload, nil); status != 200 {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	calls := h.upstream.CallsCopy()
	if len(calls) == 0 {
		t.Fatal("假上游没有收到请求")
	}
	last, _ := calls[len(calls)-1]["body"].(map[string]any)
	for _, key := range []string{"chat_id", "msg_id", "turn_id", "request_purpose"} {
		if _, ok := last[key]; ok {
			t.Fatalf("%s 应该被摘出请求体：%v", key, last)
		}
	}
}

// TestLogsEndpoint 对应 test_logs_endpoint。
func TestLogsEndpoint(t *testing.T) {
	h := svrTestNewHarness(t)

	h.gw.Log("panel test line")
	_, payload := h.getJSON("/api/panel/logs?lines=20", nil)
	if !jbool(payload, "ok") {
		t.Fatalf("ok = false：%v", payload)
	}
	found := false
	for _, raw := range jlist(payload, "lines") {
		if line, isStr := raw.(string); isStr && strings.Contains(line, "panel test line") {
			found = true
		}
	}
	if !found {
		t.Fatalf("日志尾巴里没有 panel test line：%v", payload["lines"])
	}
}

// --------------------------------------------------------- config 热重载

// svrTestWriteConfig 写一份覆盖 harness 关键配置的 config.json 到 dir，返回路径。
func svrTestWriteConfig(t *testing.T, dir, upstream, strategy string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	raw, err := json.Marshal(map[string]any{
		"upstream":              upstream,
		"account_base":          "http://127.0.0.1:1",
		"accounts_file":         filepath.Join(dir, "accounts.json"),
		"log_dir":               filepath.Join(dir, "logs"),
		"sessions_from_client":  false,
		"quota_refresh_minutes": 999,
		"max_retries":           2,
		"cooldown_seconds":      60,
		"log_console":           false,
		"strategy":              strategy,
	})
	if err != nil {
		t.Fatalf("marshal config.json: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	return path
}

// TestPanelConfigReload：POST /api/panel/config/reload 热重载 config.json，
// strategy 的变更对 /v1/admin/accounts 立即生效（Pool.Cfg 与 gw.cfg 是同一指针）。
func TestPanelConfigReload(t *testing.T) {
	h := svrTestNewHarness(t)

	path := svrTestWriteConfig(t, h.dir, h.upstream.URL(), "lru")
	h.cfg.SourcePath = path

	status, body, _ := h.post("/api/panel/config/reload", map[string]any{}, nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
	payload := svrTestDecode(t, "config reload", body)
	if !jbool(payload, "ok") {
		t.Fatalf("ok = false：%v", payload)
	}

	_, snap := h.getJSON("/v1/admin/accounts", nil)
	if got := jstr(snap, "strategy"); got != "lru" {
		t.Fatalf("reload 后 strategy = %q, want lru（热生效）", got)
	}
}

// TestPanelConfigReloadRequiresKey：api_keys 启用后，热重载端点同样在 auth 门之后。
func TestPanelConfigReloadRequiresKey(t *testing.T) {
	h := svrTestAuthHarness(t, "panel-key")

	status, body, _ := h.post("/api/panel/config/reload", map[string]any{}, nil)
	if status != 401 {
		t.Fatalf("status = %d, want 401 (%s)", status, body)
	}
}

// TestConfigHotReloadWatcher：config.json 被改后，watcher 自动热载，
// strategy 变更无需手动 POST reload 即对 /v1/admin/accounts 生效。
func TestConfigHotReloadWatcher(t *testing.T) {
	oldInterval := configWatchInterval
	configWatchInterval = 50 * time.Millisecond
	defer func() { configWatchInterval = oldInterval }()

	h := svrTestNewHarness(t)
	path := svrTestWriteConfig(t, h.dir, h.upstream.URL(), "balance")
	h.cfg.SourcePath = path

	gw := h.gw
	gw.Start()
	defer gw.Stop()

	// 改文件：strategy → round_robin。
	raw, err := json.Marshal(map[string]any{
		"upstream":              h.upstream.URL(),
		"account_base":          "http://127.0.0.1:1",
		"accounts_file":         filepath.Join(h.dir, "accounts.json"),
		"log_dir":               filepath.Join(h.dir, "logs"),
		"sessions_from_client":  false,
		"quota_refresh_minutes": 999,
		"max_retries":           2,
		"cooldown_seconds":      60,
		"log_console":           false,
		"strategy":              "round_robin",
	})
	if err != nil {
		t.Fatalf("marshal config.json: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, snap := h.getJSON("/v1/admin/accounts", nil)
		if got := jstr(snap, "strategy"); got == "round_robin" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, snap := h.getJSON("/v1/admin/accounts", nil)
	t.Fatalf("3s 内 strategy 没有变成 round_robin：%v", jstr(snap, "strategy"))
}
