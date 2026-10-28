package loomy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 移植自 tests/test_identity.py（测试配置对应 tests/support.py 的 make_config）。
//
// 所有用例都不碰真网络：账号服务/上游默认指向 127.0.0.1:1；需要看真实报文的用例
// 用本文件里的 httptest 假服务。与 Python 版只有断言写法上的差别：assertIs 对应
// 「两次返回同一个 map」，用改一个看另一个的方式验证；序列化用例额外补了 JSON 往返。

var (
	identHexRe    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	traceparentRe = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
)

// identityTestConfig 对应 tests/support.py 的 make_config。
func identityTestConfig() *Config {
	cfg := DefaultConfig()
	cfg.AccountBase = "http://127.0.0.1:1"
	cfg.Upstream = "http://127.0.0.1:1/api/v1"
	return cfg
}

// identTestStr 取字符串字段并断言它确实是字符串。
func identTestStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s 不是字符串: %v", key, m[key])
	}
	return s
}

// --------------------------------------------------------------- new_identity

func TestPerAccountModeIsDistinct(t *testing.T) {
	first, second := NewIdentity("per_account"), NewIdentity("per_account")

	if identTestStr(t, first, "devid") == identTestStr(t, second, "devid") {
		t.Fatalf("两个账号拿到同一个 devid: %q", first["devid"])
	}
	if identTestStr(t, first, "campus_device_id") == identTestStr(t, second, "campus_device_id") {
		t.Fatalf("两个账号拿到同一个 campus_device_id: %q", first["campus_device_id"])
	}
	if devid := identTestStr(t, first, "devid"); !strings.HasPrefix(devid, "web-") {
		t.Fatalf("devid = %q, 期望 web- 前缀", devid)
	}
	if len(identTestStr(t, first, "devid")) != len("web-")+16 {
		t.Fatalf("devid = %q, 期望 web- + 16 位十六进制", first["devid"])
	}
	if campus := identTestStr(t, first, "campus_device_id"); !strings.HasPrefix(campus, CampusDeviceIDPrefix) {
		t.Fatalf("campus_device_id = %q, 期望 %q 前缀", campus, CampusDeviceIDPrefix)
	}
}

func TestClientModeMirrorsTheClient(t *testing.T) {
	ident := NewIdentity("client")
	if got := identTestStr(t, ident, "devid"); got != "web" {
		t.Fatalf("devid = %q, want web", got)
	}
	if got := identTestStr(t, ident, "ua"); got != ClientUA {
		t.Fatalf("ua = %q, want %q", got, ClientUA)
	}
	if got := identTestStr(t, ident, "campus_device_id"); got != "" {
		t.Fatalf("campus_device_id = %q, want empty", got)
	}
}

func TestIdentityHasNoTraceid(t *testing.T) {
	// traceid 必须每个请求一个，和真客户端一致。
	for _, mode := range []string{"", "per_account", "client"} {
		if _, ok := NewIdentity(mode)["traceid"]; ok {
			t.Fatalf("mode=%q 的 identity 里出现了 traceid", mode)
		}
	}
}

// ------------------------------------------------------------ request envelope

func TestBaseCarriesIdentity(t *testing.T) {
	client := NewAccountClient(identityTestConfig())
	ident := NewIdentity("per_account")
	base := client.base(ident)

	if got, want := base["devid"], identTestStr(t, ident, "devid"); got != want {
		t.Fatalf("devid = %q, want %q", got, want)
	}
	if got := base["ua"]; got != ClientUA {
		t.Fatalf("ua = %q, want %q", got, ClientUA)
	}
	if got := base["modelid"]; got != WebModelID {
		t.Fatalf("modelid = %q, want %q", got, WebModelID)
	}
	if got := base["version"]; got != ClientVersion {
		t.Fatalf("version = %q, want %q", got, ClientVersion)
	}
	if got := base["appid"]; got != DefaultAccountAppID {
		t.Fatalf("appid = %q, want %q", got, DefaultAccountAppID)
	}
}

func TestTraceidIsRegeneratedPerCall(t *testing.T) {
	client := NewAccountClient(identityTestConfig())
	ident := NewIdentity("per_account")

	first, second := client.base(ident)["traceid"], client.base(ident)["traceid"]
	if first == second {
		t.Fatalf("两次 base() 的 traceid 相同: %q", first)
	}
	if !identHexRe.MatchString(first) {
		t.Fatalf("traceid = %q, 期望 32 位小写十六进制", first)
	}
}

func TestClientModeConfigIsHonoured(t *testing.T) {
	cfg := identityTestConfig()
	cfg.IdentityMode = "client"
	if got := NewAccountClient(cfg).base(nil)["devid"]; got != "web" {
		t.Fatalf("devid = %q, want web", got)
	}
}

// ----------------------------------------------------------- identity binding

func TestEnsureIdentityIsStable(t *testing.T) {
	client := NewAccountClient(identityTestConfig())
	acc := NewAccount("a")

	first := client.EnsureIdentity(acc)
	second := client.EnsureIdentity(acc)
	if first["devid"] != second["devid"] {
		t.Fatalf("两次 EnsureIdentity 的 devid 不同: %v vs %v", first["devid"], second["devid"])
	}
	// Python 断言的 assertIs(first, second)：两次返回的必须是同一个 map。
	first["probe"] = 1
	if _, ok := second["probe"]; !ok {
		t.Fatal("EnsureIdentity 每次都返回了新的 map")
	}
}

func TestRebindIdentityChangesDevid(t *testing.T) {
	client := NewAccountClient(identityTestConfig())
	acc := NewAccount("a")

	before := identTestStr(t, client.EnsureIdentity(acc), "devid")
	after := identTestStr(t, client.RebindIdentity(acc), "devid")
	if before == after {
		t.Fatalf("rebind 之后 devid 没变: %q", after)
	}
}

func TestIdentitySurvivesSerialisation(t *testing.T) {
	client := NewAccountClient(identityTestConfig())
	acc := NewAccount("a")
	acc.Loginid = "13800000000"
	acc.Password = "pw"
	client.EnsureIdentity(acc)
	wantDevid := identTestStr(t, acc.Identity, "devid")

	restored := NewAccountFromDict(acc.ToDict(), 0)
	if got := identTestStr(t, restored.Identity, "devid"); got != wantDevid {
		t.Fatalf("ToDict 往返后 devid = %q, want %q", got, wantDevid)
	}
	if got, _ := restored.IdentityView()["devid"].(string); got != wantDevid {
		t.Fatalf("往返后 identity_view().devid = %q, want %q", got, wantDevid)
	}

	// 再走一遍真正的 JSON 编解码（落盘 / 账号文件就是这个形状）。
	raw, err := json.Marshal(acc.ToDict())
	if err != nil {
		t.Fatalf("marshal to_dict: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal to_dict: %v", err)
	}
	again := NewAccountFromDict(decoded, 0)
	if got := identTestStr(t, again.Identity, "devid"); got != wantDevid {
		t.Fatalf("JSON 往返后 devid = %q, want %q", got, wantDevid)
	}
	if got := identTestStr(t, again.Identity, "ua"); got != ClientUA {
		t.Fatalf("JSON 往返后 ua = %q, want %q", got, ClientUA)
	}
}

func TestIdentityViewMasksLongCampusID(t *testing.T) {
	client := NewAccountClient(identityTestConfig())
	acc := NewAccount("a")
	client.EnsureIdentity(acc)

	view := acc.IdentityView()
	if bound, _ := view["bound"].(bool); !bound {
		t.Fatal("已绑定 identity，但 view.bound 为 false")
	}
	campus := identTestStr(t, view, "campus_device_id")
	if utf8.RuneCountInString(campus) > 33 {
		t.Fatalf("campus_device_id 太长: %q (%d 字符)", campus, utf8.RuneCountInString(campus))
	}
	if !strings.HasSuffix(campus, "…") {
		t.Fatalf("campus_device_id = %q, 期望省略号结尾", campus)
	}
}

func TestEmptyIdentityView(t *testing.T) {
	view := NewAccount("a").IdentityView()
	if bound, _ := view["bound"].(bool); bound {
		t.Fatal("空 identity 的 view.bound 应为 false")
	}
	if got := identTestStr(t, view, "devid"); got != "" {
		t.Fatalf("devid = %q, want empty", got)
	}
}

// --------------------------------------------------- account service（假服务）

func TestCallPostsSignedEnvelope(t *testing.T) {
	var (
		seenHeaders http.Header
		seenBody    map[string]any
		seenPath    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		seenBody = map[string]any{}
		_ = json.Unmarshal(raw, &seenBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"000000","data":{"pukey":"PUK","rcode":"RC"}}`)
	}))
	defer srv.Close()

	cfg := identityTestConfig()
	cfg.AccountBase = srv.URL + "/" // 尾斜杠必须被去掉
	client := NewAccountClient(cfg)
	ident := NewIdentity("per_account")

	pukey, rcode, err := client.GetPublicKey(ident)
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	if pukey != "PUK" || rcode != "RC" {
		t.Fatalf("pukey/rcode = %q/%q, want PUK/RC", pukey, rcode)
	}

	if seenPath != "/login/account/getPuKey" {
		t.Fatalf("path = %q", seenPath)
	}
	if auth := seenHeaders.Get("Authorization"); !strings.HasPrefix(auth, "account ") {
		t.Fatalf("Authorization = %q, 期望 account 前缀", auth)
	}
	if seenHeaders.Get("Date") == "" || seenHeaders.Get("Nonce") == "" {
		t.Fatalf("缺少 Date/Nonce 签名头: %v", seenHeaders)
	}
	if seenHeaders.Get("Content-MD5") == "" {
		t.Fatal("缺少 Content-MD5 头")
	}
	// Python 的 http.client 不带 User-Agent，这里也要藏掉 Go 的默认值。
	if ua := seenHeaders.Get("User-Agent"); ua != "" {
		t.Fatalf("User-Agent = %q, 期望省略", ua)
	}

	base, _ := seenBody["base"].(map[string]any)
	if base == nil {
		t.Fatalf("请求体缺少 base: %v", seenBody)
	}
	if got, want := identTestStr(t, base, "devid"), identTestStr(t, ident, "devid"); got != want {
		t.Fatalf("base.devid = %q, want %q", got, want)
	}
	if got := identTestStr(t, base, "appid"); got != DefaultAccountAppID {
		t.Fatalf("base.appid = %q", got)
	}
	if got := identTestStr(t, base, "traceid"); !identHexRe.MatchString(got) {
		t.Fatalf("base.traceid = %q, 期望 32 位小写十六进制", got)
	}
}

func TestCallMapsBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":"100001","desc":"busy"}`)
	}))
	defer srv.Close()

	cfg := identityTestConfig()
	cfg.AccountBase = srv.URL
	_, err := NewAccountClient(cfg).Call("/login/account/getPuKey",
		map[string]any{"base": map[string]any{}}, nil, time.Second)

	var aerr *AccountError
	if !errors.As(err, &aerr) {
		t.Fatalf("err = %v, want *AccountError", err)
	}
	if aerr.Code != "100001" || !aerr.Retryable {
		t.Fatalf("code/retryable = %q/%v, want 100001/true", aerr.Code, aerr.Retryable)
	}
	if want := "/login/account/getPuKey failed: busy"; aerr.Msg != want {
		t.Fatalf("msg = %q, want %q", aerr.Msg, want)
	}
}

func TestCallMapsHTTPErrors(t *testing.T) {
	t.Run("non json body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "upstream down")
		}))
		defer srv.Close()

		cfg := identityTestConfig()
		cfg.AccountBase = srv.URL
		_, err := NewAccountClient(cfg).Call("/x", map[string]any{"a": 1}, nil, time.Second)

		var aerr *AccountError
		if !errors.As(err, &aerr) {
			t.Fatalf("err = %v, want *AccountError", err)
		}
		if aerr.Code != "HTTP_503" || !aerr.Retryable {
			t.Fatalf("code/retryable = %q/%v, want HTTP_503/true", aerr.Code, aerr.Retryable)
		}
		if want := "account service returned HTTP 503: upstream down"; aerr.Msg != want {
			t.Fatalf("msg = %q, want %q", aerr.Msg, want)
		}
	})

	t.Run("message field", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"code":"000000","message":"too many"}`)
		}))
		defer srv.Close()

		cfg := identityTestConfig()
		cfg.AccountBase = srv.URL
		_, err := NewAccountClient(cfg).Call("/x", map[string]any{"a": 1}, nil, time.Second)

		var aerr *AccountError
		if !errors.As(err, &aerr) {
			t.Fatalf("err = %v, want *AccountError", err)
		}
		if aerr.Code != "HTTP_429" || aerr.Retryable {
			t.Fatalf("code/retryable = %q/%v, want HTTP_429/false", aerr.Code, aerr.Retryable)
		}
		if want := "/x HTTP 429: too many"; aerr.Msg != want {
			t.Fatalf("msg = %q, want %q", aerr.Msg, want)
		}
	})
}

func TestCallMapsNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // 端口立刻拒绝连接

	cfg := identityTestConfig()
	cfg.AccountBase = deadURL
	_, err := NewAccountClient(cfg).Call("/x", map[string]any{"a": 1}, nil, time.Second)

	var aerr *AccountError
	if !errors.As(err, &aerr) {
		t.Fatalf("err = %v, want *AccountError", err)
	}
	if aerr.Code != "NETWORK" || !aerr.Retryable {
		t.Fatalf("code/retryable = %q/%v, want NETWORK/true", aerr.Code, aerr.Retryable)
	}
	if !strings.HasPrefix(aerr.Msg, "account service unreachable: ") {
		t.Fatalf("msg = %q", aerr.Msg)
	}
}

func TestFillRequiresCredentials(t *testing.T) {
	acc := NewAccount("a")
	err := NewAccountClient(identityTestConfig()).Fill(acc)

	var aerr *AccountError
	if !errors.As(err, &aerr) {
		t.Fatalf("err = %v, want *AccountError", err)
	}
	if aerr.Code != "NO_CREDENTIALS" {
		t.Fatalf("code = %q, want NO_CREDENTIALS", aerr.Code)
	}
	if want := "account a: loginid/password missing"; aerr.Msg != want {
		t.Fatalf("msg = %q, want %q", aerr.Msg, want)
	}
}

func TestFillLogsInAndBindsIdentity(t *testing.T) {
	n, _ := testRSAKey(t)
	pukey := testSPKIDERB64(n, testRSAE)

	var byPwdBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/account/getPuKey":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":"000000","data":{"pukey":"`+pukey+`","rcode":"rc-1"}}`)
		case "/login/account/byPwd":
			raw, _ := io.ReadAll(r.Body)
			byPwdBody = map[string]any{}
			_ = json.Unmarshal(raw, &byPwdBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":"000000","data":{"session":"sess-1","userid":"uid-1","phone":"13800000000"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := identityTestConfig()
	cfg.AccountBase = srv.URL
	client := NewAccountClient(cfg)

	acc := NewAccount("a")
	acc.Loginid = "13800000000"
	acc.Password = "pw"

	before := time.Now().Unix()
	if err := client.Fill(acc); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	if acc.Session != "sess-1" || acc.Userid != "uid-1" {
		t.Fatalf("session/userid = %q/%q, want sess-1/uid-1", acc.Session, acc.Userid)
	}
	if !acc.SessionValid() {
		t.Fatal("Fill 之后 session 应该有效")
	}
	if acc.ObtainedAt < before || acc.ObtainedAt > time.Now().Unix() {
		t.Fatalf("obtained_at = %d, 不在本次调用区间内", acc.ObtainedAt)
	}
	if want := acc.ObtainedAt + int64(SessionExpireSeconds); acc.ExpireAt != want {
		t.Fatalf("expire_at = %d, want %d", acc.ExpireAt, want)
	}
	if days, ok := acc.DaysLeft(); !ok || days < 13.99 || days > 14.01 {
		t.Fatalf("days_left = %v/%v, want ~14 天", days, ok)
	}
	if devid := identTestStr(t, acc.Identity, "devid"); devid == "" {
		t.Fatal("Fill 之后应该绑定 identity")
	}

	param, _ := byPwdBody["param"].(map[string]any)
	if param == nil {
		t.Fatalf("byPwd 请求体缺少 param: %v", byPwdBody)
	}
	if got := identTestStr(t, param, "loginid"); got != "13800000000" {
		t.Fatalf("loginid = %q", got)
	}
	if got := identTestStr(t, param, "rcode"); got != "rc-1" {
		t.Fatalf("rcode = %q, want rc-1", got)
	}
	if got, _ := param["type"].(float64); got != 1 {
		t.Fatalf("type = %v, want 1", param["type"])
	}
	if got, _ := param["expire"].(float64); int64(got) != int64(SessionExpireSeconds) {
		t.Fatalf("expire = %v, want %d", param["expire"], SessionExpireSeconds)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(identTestStr(t, param, "password"))
	if err != nil {
		t.Fatalf("password 不是 base64: %v", err)
	}
	if len(ciphertext) != (n.BitLen()+7)/8 {
		t.Fatalf("密文长度 = %d, want %d", len(ciphertext), (n.BitLen()+7)/8)
	}
	base, _ := byPwdBody["base"].(map[string]any)
	if got, want := identTestStr(t, base, "devid"), identTestStr(t, acc.Identity, "devid"); got != want {
		t.Fatalf("byPwd base.devid = %q, want %q（identity 应复用）", got, want)
	}
}

// ---------------------------------------------------------------- 传输层

func TestTransportIgnoresEnvProxy(t *testing.T) {
	// Python 的 httpc 特意不走 urllib 的隐式代理：proxy=="" 必须直连。
	direct, err := httpcTransport("", 0)
	if err != nil {
		t.Fatalf("httpcTransport(\"\"): %v", err)
	}
	if direct.Proxy != nil {
		t.Fatal("proxy==\"\" 时 Transport.Proxy 必须为 nil（否则会读 http_proxy 环境变量）")
	}

	proxied, err := httpcTransport("http://127.0.0.1:3128", 0)
	if err != nil {
		t.Fatalf("httpcTransport(proxy): %v", err)
	}
	if proxied.Proxy == nil {
		t.Fatal("有代理时 Transport.Proxy 不能为 nil")
	}
	req, _ := http.NewRequest("GET", "http://example.com/x", nil)
	u, err := proxied.Proxy(req)
	if err != nil || u == nil || u.Host != "127.0.0.1:3128" {
		t.Fatalf("proxy = %v/%v, want 127.0.0.1:3128", u, err)
	}

	// 与 Python _split 一致：只认 http / https 代理。
	if _, err := httpcTransport("socks5://127.0.0.1:1080", 0); err == nil {
		t.Fatal("不支持的代理 scheme 应该报错")
	}
}

func TestRequestDoesNotFollowRedirects(t *testing.T) {
	// Python 的 http.client 不跟随 3xx；Go 默认跟随，这里必须原样返回状态码。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "landed")
	}))
	defer srv.Close()

	status, _, body, err := Request(srv.URL+"/redirect", "POST", nil, []byte("{}"),
		5*time.Second, "")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if status != http.StatusFound {
		t.Fatalf("status = %d, want 302（不能跟随重定向）", status)
	}
	if string(body) != "" {
		t.Fatalf("body = %q, want empty", body)
	}
}

// ---------------------------------------------------------------- 上游网关

func TestUpstreamQuotaReadsBalanceFields(t *testing.T) {
	var (
		seenPath  string
		seenQuery string
		seenAuth  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenQuery = r.URL.RawQuery
		seenAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"code":"000000","data":{"balance":100,"dailyBalance":50}}`)
	}))
	defer srv.Close()

	cfg := identityTestConfig()
	cfg.Upstream = srv.URL + "/api/v1/"
	gw := NewModelGateway(cfg)

	quota, err := gw.Quota("sess-1")
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if quota.Balance == nil || *quota.Balance != 100 {
		t.Fatalf("balance = %v, want 100", quota.Balance)
	}
	if quota.DailyBalance == nil || *quota.DailyBalance != 50 {
		t.Fatalf("daily_balance = %v, want 50", quota.DailyBalance)
	}
	if quota.Available != nil {
		t.Fatalf("available = %v, 缺失时应为 nil", *quota.Available)
	}
	if seenPath != "/api/v1/points/records" {
		t.Fatalf("path = %q", seenPath)
	}
	if !strings.Contains(seenQuery, "page_size=1") || !strings.Contains(seenQuery, "record_type=all") {
		t.Fatalf("query = %q", seenQuery)
	}
	if seenAuth != "Bearer sess-1" {
		t.Fatalf("Authorization = %q", seenAuth)
	}
}

func TestUpstreamModelsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid session"}}`)
	}))
	defer srv.Close()

	cfg := identityTestConfig()
	cfg.Upstream = srv.URL
	_, err := NewModelGateway(cfg).Models("bad")

	var uerr *UpstreamError
	if !errors.As(err, &uerr) {
		t.Fatalf("err = %v, want *UpstreamError", err)
	}
	if uerr.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", uerr.Status)
	}
	if want := "/models HTTP 401: "; !strings.HasPrefix(uerr.Msg, want) || !strings.Contains(uerr.Msg, "invalid session") {
		t.Fatalf("msg = %q", uerr.Msg)
	}
}

func TestUpstreamStreamForwardsHeadersAndUsage(t *testing.T) {
	var (
		seenHeaders http.Header
		seenPath    string
		seenBody    map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		seenBody = map[string]any{}
		_ = json.Unmarshal(raw, &seenBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":4,\"points_consumed\":1}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := identityTestConfig()
	cfg.Upstream = srv.URL + "/api/v1"
	gw := NewModelGateway(cfg)

	resp, err := gw.Stream(context.Background(), "sess-1", "chat/completions",
		map[string]any{"model": "m", "stream": true}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("读取流: %v", readErr)
	}

	if seenPath != "/api/v1/chat/completions" {
		t.Fatalf("path = %q", seenPath)
	}
	if got := seenHeaders.Get("Authorization"); got != "Bearer sess-1" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := seenHeaders.Get("token"); got != "sess-1" {
		t.Fatalf("token = %q", got)
	}
	if got := seenHeaders.Get("traceparent"); !traceparentRe.MatchString(got) {
		t.Fatalf("traceparent = %q", got)
	}
	if got := seenHeaders.Get("loomy-version"); got != DefaultLoomyVersion {
		t.Fatalf("loomy-version = %q, want %q", got, DefaultLoomyVersion)
	}
	if seenBody["model"] != "m" || seenBody["stream"] != true {
		t.Fatalf("请求体 = %v", seenBody)
	}

	usage := map[string]any{}
	for _, line := range strings.Split(string(body), "\n") {
		SSEUsage(strings.TrimSpace(line), usage)
	}
	if got, want := HumanUsage(usage), "tok=2/4 points=1"; got != want {
		t.Fatalf("human_usage = %q, want %q", got, want)
	}
	if got := usage["completion_tokens"]; got != float64(4) {
		t.Fatalf("sink.completion_tokens = %v, want 4", got)
	}
}
