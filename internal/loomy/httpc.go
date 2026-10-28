package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"
)

// 对应 Python loomy2api/httpc.py —— 极薄的 HTTP 辅助层（仅标准库），显式代理控制。
//
// urllib 会隐式读取 http_proxy / https_proxy 环境变量，而这正是这里最不想要的行为：
// 上游都是国内端点，默认必须直连，代理只能由配置显式给出。Go 的
// http.ProxyFromEnvironment 有同样的隐式行为，所以 proxy=="" 时 Transport.Proxy
// 显式置 nil。
//
// 与 Python 的差异：http.client 的 timeout 是 socket 级（connect 与每次读写各自计时），
// Go 没有完全等价的开关。阻塞请求用 http.Client.Timeout 做整体超时；流式请求改用
// Transport.ResponseHeaderTimeout（只管到响应头为止）+ ctx 取消，否则 1200 秒的长 SSE
// 流会被总超时误杀。

// ---------------------------------------------------------------- transport 复用

// httpcTransportKey 区分 Transport：代理不同或响应头超时不同就不能共用。
type httpcTransportKey struct {
	proxy         string
	headerTimeout time.Duration
}

var (
	httpcTransportMu    sync.Mutex
	httpcTransportCache = map[httpcTransportKey]*http.Transport{}
)

// httpcTransport 返回（并缓存）一个可复用的 *http.Transport。
// 复用同一 Transport 才有 keep-alive：Python 每次新建连接，这里刻意比它更好。
// headerTimeout 只作用于「等到响应头」这一段，0 表示不设限。
func httpcTransport(proxy string, headerTimeout time.Duration) (*http.Transport, error) {
	key := httpcTransportKey{proxy: proxy, headerTimeout: headerTimeout}

	httpcTransportMu.Lock()
	defer httpcTransportMu.Unlock()
	if tr, ok := httpcTransportCache[key]; ok {
		return tr, nil
	}

	tr := &http.Transport{
		// 显式 nil：屏蔽 http_proxy / https_proxy / no_proxy 环境变量的隐式生效。
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: headerTimeout,
		// 不设 ForceAttemptHTTP2：保持 HTTP/1.1，与 Python 的 http.client 一致。
	}
	if proxy != "" {
		parsed, err := neturl.Parse(proxy)
		if err != nil || parsed.Host == "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("unsupported URL scheme: %s", proxy)
		}
		tr.Proxy = http.ProxyURL(parsed)
	}

	httpcTransportCache[key] = tr
	return tr, nil
}

// httpcHeader 把 map[string]string 转成 http.Header。
// 这里直接写 map 而不是 Set：Set 会把 "Content-MD5" 规范化成 "Content-Md5"，
// 而账号服务的签名头最好保持 build_headers 给出的原样写法。
// 另外补一个空的 User-Agent —— Go 默认发 "Go-http-client/1.1"，Python 的
// http.client 一个都不发；空值会让 Transport 彻底省略该头。
func httpcHeader(headers map[string]string) http.Header {
	out := make(http.Header, len(headers)+1)
	hasUA := false
	for k, v := range headers {
		out[k] = []string{v}
		if strings.EqualFold(k, "User-Agent") {
			hasUA = true
		}
	}
	if !hasUA {
		out["User-Agent"] = []string{""}
	}
	return out
}

// httpcText 对应 Python 的 data[:n].decode("utf-8", "replace")：
// 取前 n 字节，非法 UTF-8 字节序列换成 U+FFFD（截断到半个字符也会被换掉）。
func httpcText(data []byte, n int) string {
	if n > 0 && len(data) > n {
		data = data[:n]
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

// jsonDumps 对应 Python 的 json.dumps(v, ensure_ascii=False)。
// Go 默认把 < > & 转义成 \u003c 之类（Python 不会），关掉以保持报文一致。
func jsonDumps(v any) (string, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// ------------------------------------------------------------------- 请求

// httpcNewClient 造一个不跟随重定向的 client：Python 的 http.client 从不自动跟随 3xx，
// 而 Go 默认跟 10 次（还会把 301/302 的 POST 改成 GET）。这里保持「原样交出状态码」。
func httpcNewClient(tr *http.Transport) *http.Client {
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Request 是阻塞请求 → (status, 响应头, body, err)。
// proxy=="" 时直连（Transport.Proxy 为 nil，不看环境变量）；timeout<=0 表示不设
// 整体超时，否则用 http.Client.Timeout 覆盖到读完 body 为止。
func Request(url, method string, headers map[string]string, body []byte, timeout time.Duration, proxy string) (int, http.Header, []byte, error) {
	tr, err := httpcTransport(proxy, 0)
	if err != nil {
		return 0, nil, nil, err
	}
	if method == "" {
		method = "GET"
	}
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = httpcHeader(headers)

	client := httpcNewClient(tr)
	if timeout > 0 {
		client.Timeout = timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, resp.Header, nil, readErr
	}
	return resp.StatusCode, resp.Header, data, nil
}

// OpenStream 发起请求但只读到响应头为止：返回的 *http.Response 由调用方负责 Close。
// 语义对齐 Python open_stream —— timeout 只用于 ResponseHeaderTimeout，body 的流式读取
// 不设总超时（1200 秒的长 SSE 流不能被误杀）；ctx 用于调用方取消（例如 HTTP handler
// 发现客户端断开时中止上游读取）。
func OpenStream(ctx context.Context, url, method string, headers map[string]string, body []byte, timeout time.Duration, proxy string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tr, err := httpcTransport(proxy, timeout)
	if err != nil {
		return nil, err
	}
	if method == "" {
		method = "POST"
	}
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header = httpcHeader(headers)

	// 不设 client.Timeout：它是覆盖整个 body 读取的总超时。
	client := httpcNewClient(tr)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
