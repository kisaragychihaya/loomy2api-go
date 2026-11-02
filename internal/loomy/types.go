package loomy

import "net/http"

// LogFunc 是日志输出回调（对应 Python logger 的写出方法）。
type LogFunc func(string)

// Quota 是账号的额度快照，对应上游 points/查询返回的三个余额字段。
// 用指针区分「未知」与「零」——nil 表示上游没有给出该字段。
type Quota struct {
	Balance      *int64
	DailyBalance *int64
	Available    *int64
}

// GatewayAPI 是 ModelGateway 需要账号池提供的最小接口，
// 后续波次的测试 fake 实现它。
type GatewayAPI interface {
	Quota(session string) (Quota, error)
	// Call 对应 Python ModelGateway.call：原样透传一次请求，返回原始状态码与响应体
	// （onboarding 的 200-业务码判断要用到它，见 onboarding.go）。
	Call(session, suffix string, payload map[string]any, extra map[string]string, method string) (int, http.Header, []byte, error)
}
