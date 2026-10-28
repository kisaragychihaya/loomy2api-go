package loomy

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// 解析 JSON 得到的 map[string]any 的安全取值辅助函数。
// 对应 Python 侧对 dict 的 .get(...) 访问；nil map 与缺失 key 都返回零值。

// jstr 取值并转成字符串：string 原样，float64 用 %v 风格（整数不带小数点），
// nil 或缺 key 返回 ""，其它类型用 fmt.Sprint。
func jstr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// 与 Python str(17890.0) 不同，这里按 %v 风格去掉整数的 ".0"。
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	default:
		return fmt.Sprint(v)
	}
}

// jstrOK 取字符串值，第二个返回值表示该 key 存在且值确实是字符串。
func jstrOK(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, isStr := v.(string)
	if !isStr {
		return "", false
	}
	return s, true
}

// jint 取值并转成 int64：float64/int/int64/json.Number 支持，nil 或类型不符返回 0。
func jint(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case int32:
		return int64(t)
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			f, ferr := t.Float64()
			if ferr != nil {
				return 0
			}
			return int64(f)
		}
		return n
	default:
		return 0
	}
}

// jfloat 取值并转成 float64：float64/int/int64/json.Number 支持，其余返回 0。
func jfloat(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case int32:
		return float64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// jbool 取值并转成 bool：bool 原样，nil 或其它类型返回 false。
func jbool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	b, isBool := v.(bool)
	if !isBool {
		return false
	}
	return b
}

// jmap 取嵌套对象，缺失或不是对象时返回 nil。
func jmap(m map[string]any, key string) map[string]any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	out, isMap := v.(map[string]any)
	if !isMap {
		return nil
	}
	return out
}

// jlist 取嵌套数组，缺失或不是数组时返回 nil。
func jlist(m map[string]any, key string) []any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	out, isList := v.([]any)
	if !isList {
		return nil
	}
	return out
}

// jset 写入 m[key]=v；m 为 nil 时静默不做事。
func jset(m map[string]any, key string, v any) {
	if m == nil {
		return
	}
	m[key] = v
}
