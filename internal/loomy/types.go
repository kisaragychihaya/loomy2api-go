package loomy

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
// 后续波次的测试 fake 实现它；目前只有一个方法。
type GatewayAPI interface {
	Quota(session string) (Quota, error)
}
