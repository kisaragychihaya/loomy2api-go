package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 对应 Python loomy2api/upstream.py —— Loomy 模型网关（OpenAI 兼容上游）客户端。

// UpstreamError 是上游返回非 200 时的错误（对应 Python UpstreamError）。
type UpstreamError struct {
	Msg    string
	Status int
}

func (e *UpstreamError) Error() string {
	return e.Msg
}

// BuildModelHeaders 对应 Python build_model_headers：模型网关要求的请求头。
//
//   - Authorization 和 token 都发：客户端的 provider 用 useSessionAuth，网关两种都认。
//   - traceparent 必须有：客户端作者实测缺了它上游会一直挂到超时。
//   - loomy-version 只校验存在性。
//
// extra 里的非空值覆盖同名头（空值按 Python 的 `if value` 跳过）。
func BuildModelHeaders(session, version string, extra map[string]string) map[string]string {
	headers := map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + session,
		"token":         session,
		"traceparent":   "00-" + accountHex(32) + "-" + accountHex(16) + "-01",
	}
	if version != "" {
		headers["loomy-version"] = version
	}
	for key, value := range extra {
		if value != "" {
			headers[key] = value
		}
	}
	return headers
}

// ModelGateway 是上游 HTTP API 的薄封装。
type ModelGateway struct {
	cfg *Config
}

// 编译期确认 ModelGateway 满足 types.go 的 GatewayAPI。
var _ GatewayAPI = (*ModelGateway)(nil)

// NewModelGateway 对应 Python ModelGateway(cfg)。
func NewModelGateway(cfg *Config) *ModelGateway {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	return &ModelGateway{cfg: cfg}
}

// url 对应 Python _url：base 去尾斜杠、suffix 去前导斜杠后拼接。
func (g *ModelGateway) url(suffix string) string {
	return strings.TrimRight(g.cfg.Upstream, "/") + "/" + strings.TrimLeft(suffix, "/")
}

// headers 对应 Python _headers。
func (g *ModelGateway) headers(session string, extra map[string]string) map[string]string {
	return BuildModelHeaders(session, g.cfg.LoomyVersion, extra)
}

// gatewayTimeout 对应 Python 的 float(cfg.get("timeout") or def)：
// 配置为 0 / 负数时退回该方法自己的默认秒数。
func (g *ModelGateway) gatewayTimeout(defSeconds int) time.Duration {
	seconds := g.cfg.Timeout
	if seconds <= 0 {
		seconds = defSeconds
	}
	return time.Duration(seconds) * time.Second
}

// Models 对应 Python models：GET <upstream>/models。非 200 抛 UpstreamError。
func (g *ModelGateway) Models(session string) (map[string]any, error) {
	status, _, data, err := Request(g.url("models"), "GET", g.headers(session, nil), nil,
		g.gatewayTimeout(60), g.cfg.Proxy)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &UpstreamError{
			Msg:    fmt.Sprintf("/models HTTP %d: %s", status, httpcText(data, 200)),
			Status: status,
		}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("/models: %w", err)
	}
	return out, nil
}

// PointsRecords 对应 Python points_records：GET points/records（固定 30 秒超时）。
// Go 没有默认参数，pageSize<=0 时按 Python 的 page_size=20 处理。
func (g *ModelGateway) PointsRecords(session string, pageSize int) (map[string]any, error) {
	if pageSize <= 0 {
		pageSize = 20
	}
	suffix := fmt.Sprintf("points/records?record_type=all&page_no=1&page_size=%d", pageSize)
	status, _, data, err := Request(g.url(suffix), "GET", g.headers(session, nil), nil,
		30*time.Second, g.cfg.Proxy)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &UpstreamError{
			Msg:    fmt.Sprintf("/points/records HTTP %d", status),
			Status: status,
		}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("/points/records: %w", err)
	}
	return out, nil
}

// Quota 对应 Python quota：从 points/records 的 data 里取三个余额（实现 types.go 的 GatewayAPI）。
// 缺失或 null 的字段保持 nil，这样调用方能区分「未知」与「零」。
func (g *ModelGateway) Quota(session string) (Quota, error) {
	payload, err := g.PointsRecords(session, 1)
	if err != nil {
		return Quota{}, err
	}
	data := jmap(payload, "data")
	return Quota{
		Balance:      jintPtr(data, "balance"),
		DailyBalance: jintPtr(data, "dailyBalance"),
		Available:    jintPtr(data, "availableBalance"),
	}, nil
}

// jintPtr 取整数并返回指针：缺失 / null → nil。
func jintPtr(m map[string]any, key string) *int64 {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	n := pyInt64(v)
	return &n
}

// Call 对应 Python call：原样透传一次请求，body 为空时不带请求体。
func (g *ModelGateway) Call(session, suffix string, payload map[string]any, extra map[string]string, method string) (int, http.Header, []byte, error) {
	var body []byte
	if len(payload) > 0 {
		text, err := jsonDumps(payload)
		if err != nil {
			return 0, nil, nil, err
		}
		body = []byte(text)
	}
	if method == "" {
		method = "POST"
	}
	return Request(g.url(suffix), method, g.headers(session, extra), body,
		g.gatewayTimeout(1200), g.cfg.Proxy)
}

// Stream 对应 Python stream：POST 上游并只读到响应头，body 由调用方流式读取并 Close。
// ctx 让持流的一方（例如 HTTP handler）在客户端断开时能中止上游读取。
func (g *ModelGateway) Stream(ctx context.Context, session, suffix string, payload map[string]any, extra map[string]string) (*http.Response, error) {
	text, err := jsonDumps(payload)
	if err != nil {
		return nil, err
	}
	return OpenStream(ctx, g.url(suffix), "POST", g.headers(session, extra), []byte(text),
		g.gatewayTimeout(1200), g.cfg.Proxy)
}

// ---------------------------------------------------------------- usage 统计

// ExtractUsage 对应 Python extract_usage：容错地取出 obj["usage"]，取不到返回空 map。
func ExtractUsage(obj map[string]any) map[string]any {
	usage, isMap := obj["usage"].(map[string]any)
	if !isMap || usage == nil {
		return map[string]any{}
	}
	return usage
}

// SSEUsage 对应 Python sse_usage：把一行 SSE 里的 usage 折进 sink（后写覆盖先写）。
// 只认 "data:" 前缀，跳过空行与 [DONE]，JSON 解析失败静默忽略。
func SSEUsage(line string, sink map[string]any) {
	const prefix = "data:"
	if !strings.HasPrefix(line, prefix) {
		return
	}
	chunk := strings.TrimSpace(line[len(prefix):])
	if chunk == "" || chunk == "[DONE]" {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(chunk), &obj); err != nil {
		return
	}
	usage := ExtractUsage(obj)
	if len(usage) == 0 || sink == nil {
		return
	}
	for k, v := range usage {
		sink[k] = v
	}
}

// HumanUsage 对应 Python human_usage：拼出 "tok=a/b think=c points=d"。
// Python 用 f-string 打印值，缺失的 token 数会写成 None，这里保持一致。
func HumanUsage(usage map[string]any) string {
	parts := []string{}

	prompt, hasPrompt := usage["prompt_tokens"]
	completion, hasCompletion := usage["completion_tokens"]
	if (hasPrompt && prompt != nil) || (hasCompletion && completion != nil) {
		parts = append(parts, "tok="+upStr(usage["prompt_tokens"])+"/"+upStr(usage["completion_tokens"]))
	}

	if details, isMap := usage["completion_tokens_details"].(map[string]any); isMap {
		if reasoning := details["reasoning_tokens"]; anthTruthy(reasoning) {
			parts = append(parts, "think="+upStr(reasoning))
		}
	}

	if points, ok := usage["points_consumed"]; ok && points != nil {
		parts = append(parts, "points="+upStr(points))
	}

	return strings.Join(parts, " ")
}

// upStr 对应 Python 的 str(v)：None 打成 "None"，数字不带多余小数位。
// Python 会区分 int 与 float（str(2.0) == "2.0"），Go 的 JSON 解码只有 float64，
// 所以这里统一按「无多余小数」打印。
func upStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}
