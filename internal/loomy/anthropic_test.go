package loomy

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// 本文件移植 loomy2api/tests/test_anthropic.py，函数名与 Python 用例一一对应。
// 期望值逐字取自 Python 实现的实际输出（见每条用例的注释）。
// 另外补充的用例都标注了「补充」，用来钉住契约里的边界行为。
//
// 与 Python 的唯一可见差异：json.dumps 默认分隔符是 ", " / ": "，
// 这里用 Go 的紧凑编码（{"b":2}），因此涉及 arguments / partial_json
// 的断言一律先解析再比较（Python 用例本身也是这么写的）。

// ---------------------------------------------------------------- 测试辅助

// anthTestJSON 用关闭 HTML 转义的编码器序列化，键序确定，便于整串比较。
func anthTestJSON(t *testing.T, v any) string {
	t.Helper()
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("序列化 %#v 失败: %v", v, err)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// anthTestDynamicID 匹配实现生成的随机 id（Python 侧是 uuid4().hex 切片）。
var anthTestDynamicID = regexp.MustCompile(`(msg_|call_)[0-9a-f]{12,24}`)

// anthTestNorm 把随机 id 归一化成 <id>，便于整串对比。
func anthTestNorm(s string) string {
	return anthTestDynamicID.ReplaceAllString(s, "${1}<id>")
}

// anthTestWant 是流式用例里期望的一条事件：event 名 + data 的 JSON。
type anthTestWant struct {
	event string
	data  string
}

// anthTestMessageStart 是 message_start 的 data JSON（id 已归一化）。
const anthTestMessageStart = `{"message":{"content":[],"id":"msg_<id>","model":"m","role":"assistant",` +
	`"stop_reason":null,"stop_sequence":null,"type":"message","usage":{"input_tokens":0,"output_tokens":0}},` +
	`"type":"message_start"}`

// anthTestRunStream 对应 Python 用例里的 _run 辅助函数。
func anthTestRunStream(chunks ...map[string]any) (*StreamTranslator, []AnthropicEvent) {
	st := NewStreamTranslator("m")
	events := append([]AnthropicEvent{}, st.Start()...)
	for _, chunk := range chunks {
		events = append(events, st.Feed(chunk)...)
	}
	return st, append(events, st.Finish()...)
}

// anthTestCheckStream 把事件序列与期望序列逐条对比（event 名 + data JSON）。
func anthTestCheckStream(t *testing.T, events []AnthropicEvent, want []anthTestWant) {
	t.Helper()
	if len(events) != len(want) {
		got := make([]string, 0, len(events))
		for _, ev := range events {
			got = append(got, ev.Name+" "+anthTestNorm(anthTestJSON(t, ev.Data)))
		}
		t.Fatalf("事件数 %d，期望 %d:\n%s", len(events), len(want), strings.Join(got, "\n"))
	}
	for i, ev := range events {
		if ev.Name != want[i].event {
			t.Errorf("事件[%d] 名称 = %q，期望 %q", i, ev.Name, want[i].event)
		}
		if got, expect := anthTestNorm(anthTestJSON(t, ev.Data)), want[i].data; got != expect {
			t.Errorf("事件[%d] %s data =\n%s\n期望\n%s", i, ev.Name, got, expect)
		}
	}
}

// anthTestNames 取出事件名序列。
func anthTestNames(events []AnthropicEvent) []string {
	names := make([]string, 0, len(events))
	for _, ev := range events {
		names = append(names, ev.Name)
	}
	return names
}

// anthTestDataOf 取出指定名字的全部事件 data。
func anthTestDataOf(events []AnthropicEvent, name string) []map[string]any {
	out := []map[string]any{}
	for _, ev := range events {
		if ev.Name == name {
			out = append(out, ev.Data)
		}
	}
	return out
}

// anthTestMap 断言 v 是对象。
func anthTestMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("期望对象，得到 %T: %v", v, v)
	}
	return m
}

// anthTestList 断言 v 是数组。
func anthTestList(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("期望数组，得到 %T: %v", v, v)
	}
	return l
}

// ---------------------------------------------------------------- 请求翻译

// 对应 test_system_and_blocks。
func TestAnthropicToOpenAISystemAndBlocks(t *testing.T) {
	out := AnthropicToOpenAI(map[string]any{
		"model":       "m",
		"max_tokens":  16,
		"temperature": 0.2,
		"system":      []any{map[string]any{"type": "text", "text": "be brief"}},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "hi"},
		}}},
	})

	messages := anthTestList(t, out["messages"])
	if len(messages) != 2 {
		t.Fatalf("messages 长度 = %d，期望 2: %s", len(messages), anthTestJSON(t, messages))
	}
	if want := map[string]any{"role": "system", "content": "be brief"}; !reflect.DeepEqual(messages[0], want) {
		t.Errorf("messages[0] = %s，期望 %s", anthTestJSON(t, messages[0]), anthTestJSON(t, want))
	}
	if want := map[string]any{"role": "user", "content": "hi"}; !reflect.DeepEqual(messages[1], want) {
		t.Errorf("messages[1] = %s，期望 %s", anthTestJSON(t, messages[1]), anthTestJSON(t, want))
	}
	if got := out["max_tokens"]; got != 16 {
		t.Errorf("max_tokens = %#v，期望 16", got)
	}
	if got := out["temperature"]; got != 0.2 {
		t.Errorf("temperature = %#v，期望 0.2", got)
	}
	if got := out["stream"]; got != false {
		t.Errorf("stream = %#v，期望 false", got)
	}
}

// 对应 test_tools_and_tool_results。
func TestAnthropicToOpenAIToolsAndToolResults(t *testing.T) {
	out := AnthropicToOpenAI(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "calling"},
				map[string]any{"type": "tool_use", "id": "tu1", "name": "get_weather",
					"input": map[string]any{"city": "hz"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu1",
					"content": []any{map[string]any{"type": "text", "text": "sunny"}}},
			}},
		},
		"tools": []any{map[string]any{
			"name": "get_weather", "description": "w",
			"input_schema": map[string]any{"type": "object"},
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "get_weather"},
	})

	messages := anthTestList(t, out["messages"])
	// tool_result 只产生独立的 tool 消息，不产生空的 user 脚手架。
	if len(messages) != 2 {
		t.Fatalf("messages 长度 = %d，期望 2: %s", len(messages), anthTestJSON(t, messages))
	}

	assistant := anthTestMap(t, messages[0])
	call := anthTestMap(t, anthTestList(t, assistant["tool_calls"])[0])
	if call["id"] != "tu1" {
		t.Errorf("tool_calls[0].id = %#v，期望 \"tu1\"", call["id"])
	}
	if got := assistant["content"]; got != "calling" {
		t.Errorf("assistant.content = %#v，期望 \"calling\"", got)
	}
	arguments, ok := anthTestMap(t, call["function"])["arguments"].(string)
	if !ok {
		t.Fatalf("arguments 不是字符串: %v", call["function"])
	}
	var args any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON (%q): %v", arguments, err)
	}
	if want := map[string]any{"city": "hz"}; !reflect.DeepEqual(args, want) {
		t.Errorf("arguments 解析 = %s，期望 %s", anthTestJSON(t, args), anthTestJSON(t, want))
	}

	if want := map[string]any{"role": "tool", "tool_call_id": "tu1", "content": "sunny"}; !reflect.DeepEqual(messages[1], want) {
		t.Errorf("messages[1] = %s，期望 %s", anthTestJSON(t, messages[1]), anthTestJSON(t, want))
	}

	tool := anthTestMap(t, anthTestList(t, out["tools"])[0])
	if want := map[string]any{"type": "object"}; !reflect.DeepEqual(anthTestMap(t, tool["function"])["parameters"], want) {
		t.Errorf("tools[0].function.parameters = %s，期望 %s",
			anthTestJSON(t, anthTestMap(t, tool["function"])["parameters"]), anthTestJSON(t, want))
	}
	if want := map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}; !reflect.DeepEqual(out["tool_choice"], want) {
		t.Errorf("tool_choice = %s，期望 %s", anthTestJSON(t, out["tool_choice"]), anthTestJSON(t, want))
	}
}

// 对应 test_stop_sequences。
func TestAnthropicToOpenAIStopSequences(t *testing.T) {
	out := AnthropicToOpenAI(map[string]any{
		"model":          "m",
		"messages":       []any{},
		"stop_sequences": []any{"\n\n"},
	})
	if want := []any{"\n\n"}; !reflect.DeepEqual(out["stop"], want) {
		t.Errorf("stop = %s，期望 %s", anthTestJSON(t, out["stop"]), anthTestJSON(t, want))
	}
}

// 补充：可选字段只在非 None 时透传（0 / false 也要带上），空 messages 编码成 []。
func TestAnthropicToOpenAIOptionalFields(t *testing.T) {
	// Python 输出：{"max_tokens": 0, "messages": [], "model": "m", "stream": true, "temperature": 0}
	out := AnthropicToOpenAI(map[string]any{
		"model": "m", "max_tokens": 0, "temperature": 0,
		"top_p": nil, "stream": true, "messages": []any{},
	})
	want := `{"max_tokens":0,"messages":[],"model":"m","stream":true,"temperature":0}`
	if got := anthTestJSON(t, out); got != want {
		t.Errorf("整个请求体 =\n%s\n期望\n%s", got, want)
	}

	// system 是纯字符串，content 是纯字符串（空串也照发）。
	// Python 输出：{"messages": [{"content": "plain sys", "role": "system"},
	//   {"content": "hi", "role": "user"}, {"content": "", "role": "assistant"}],
	//   "model": "m", "stream": false}
	out = AnthropicToOpenAI(map[string]any{
		"model":  "m",
		"system": "plain sys",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": ""},
		},
	})
	want = `{"messages":[{"content":"plain sys","role":"system"},{"content":"hi","role":"user"},` +
		`{"content":"","role":"assistant"}],"model":"m","stream":false}`
	if got := anthTestJSON(t, out); got != want {
		t.Errorf("整个请求体 =\n%s\n期望\n%s", got, want)
	}
}

// 补充：tools 只收有 name 的项，缺 input_schema 用默认值；tool_choice 三种映射。
func TestAnthropicToOpenAIToolsAndToolChoice(t *testing.T) {
	// Python 输出：tools 里 "a" 带默认 parameters，"no name" 被丢弃；
	// tool_choice any -> "required"。
	out := AnthropicToOpenAI(map[string]any{
		"model": "m", "messages": []any{},
		"tools": []any{
			map[string]any{"name": "a"},
			map[string]any{"description": "no name"},
			map[string]any{"name": "b", "input_schema": map[string]any{"x": 1}},
		},
		"tool_choice": map[string]any{"type": "any"},
	})
	want := `{"messages":[],"model":"m","stream":false,"tool_choice":"required","tools":[` +
		`{"function":{"description":"","name":"a","parameters":{"properties":{},"type":"object"}},"type":"function"},` +
		`{"function":{"description":"","name":"b","parameters":{"x":1}},"type":"function"}]}`
	if got := anthTestJSON(t, out); got != want {
		t.Errorf("整个请求体 =\n%s\n期望\n%s", got, want)
	}

	out = AnthropicToOpenAI(map[string]any{
		"model": "m", "messages": []any{},
		"tool_choice": map[string]any{"type": "auto"},
	})
	if got := out["tool_choice"]; got != "auto" {
		t.Errorf("tool_choice = %#v，期望 \"auto\"", got)
	}
}

// 补充：arguments 必须按 ensure_ascii=False 语义编码 —— 中文原样，<>& 不转义。
func TestAnthropicToOpenAIArgumentsEncoding(t *testing.T) {
	out := AnthropicToOpenAI(map[string]any{
		"model": "m",
		"messages": []any{map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "f",
				"input": map[string]any{"q": "中文<&>"}},
		}}},
	})
	assistant := anthTestMap(t, anthTestList(t, out["messages"])[0])
	call := anthTestMap(t, anthTestList(t, assistant["tool_calls"])[0])
	arguments, ok := anthTestMap(t, call["function"])["arguments"].(string)
	if !ok {
		t.Fatalf("arguments 不是字符串: %v", call["function"])
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`, `\u4e2d`} {
		if strings.Contains(arguments, escaped) {
			t.Errorf("arguments 不应包含转义 %s: %s", escaped, arguments)
		}
	}
	var args any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON (%q): %v", arguments, err)
	}
	if want := map[string]any{"q": "中文<&>"}; !reflect.DeepEqual(args, want) {
		t.Errorf("arguments 解析 = %s，期望 %s", anthTestJSON(t, args), anthTestJSON(t, want))
	}
}

// ---------------------------------------------------------------- 响应翻译

// 对应 test_text_reasoning_and_tool_calls。
func TestOpenAIToAnthropicTextReasoningAndToolCalls(t *testing.T) {
	got := OpenAIToAnthropic(map[string]any{
		"id": "x",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"reasoning_content": "because",
				"content":           "done",
				"tool_calls": []any{map[string]any{
					"id":       "c1",
					"function": map[string]any{"name": "f", "arguments": `{"a":1}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3},
	}, "m")

	if got["id"] != "x" {
		t.Errorf("id = %#v，期望 \"x\"", got["id"])
	}
	if got["type"] != "message" {
		t.Errorf("type = %#v，期望 \"message\"", got["type"])
	}
	if got["role"] != "assistant" {
		t.Errorf("role = %#v，期望 \"assistant\"", got["role"])
	}
	if got["model"] != "m" {
		t.Errorf("model = %#v，期望 \"m\"", got["model"])
	}

	content := anthTestList(t, got["content"])
	kinds := make([]any, 0, len(content))
	for _, block := range content {
		kinds = append(kinds, anthTestMap(t, block)["type"])
	}
	if want := []any{"thinking", "text", "tool_use"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("content 块类型 = %v，期望 %v", kinds, want)
	}
	if want := map[string]any{"type": "thinking", "thinking": "because", "signature": ""}; !reflect.DeepEqual(content[0], want) {
		t.Errorf("content[0] = %s，期望 %s", anthTestJSON(t, content[0]), anthTestJSON(t, want))
	}
	if want := map[string]any{"type": "text", "text": "done"}; !reflect.DeepEqual(content[1], want) {
		t.Errorf("content[1] = %s，期望 %s", anthTestJSON(t, content[1]), anthTestJSON(t, want))
	}
	// input 由 json.loads 得到，Go 侧数字是 float64，故用 JSON 对比。
	if want := `{"id":"c1","input":{"a":1},"name":"f","type":"tool_use"}`; anthTestJSON(t, content[2]) != want {
		t.Errorf("content[2] = %s，期望 %s", anthTestJSON(t, content[2]), want)
	}

	if got["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %#v，期望 \"tool_use\"", got["stop_reason"])
	}
	if got["stop_sequence"] != nil {
		t.Errorf("stop_sequence = %#v，期望 null", got["stop_sequence"])
	}
	if want := map[string]any{"input_tokens": int64(7), "output_tokens": int64(3)}; !reflect.DeepEqual(got["usage"], want) {
		t.Errorf("usage = %s，期望 %s", anthTestJSON(t, got["usage"]), anthTestJSON(t, want))
	}
}

// 对应 test_length_finish_reason。
func TestOpenAIToAnthropicLengthFinishReason(t *testing.T) {
	got := OpenAIToAnthropic(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"content": "x"},
			"finish_reason": "length",
		}},
	}, "m")
	if got["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %#v，期望 \"max_tokens\"", got["stop_reason"])
	}
}

// 补充：缺 id 时生成 msg_<24hex> / call_<12hex>；arguments 坏掉时 input 退化成 {}。
func TestOpenAIToAnthropicGeneratedIDs(t *testing.T) {
	msgIDPattern := regexp.MustCompile(`^msg_[0-9a-f]{24}$`)
	callIDPattern := regexp.MustCompile(`^call_[0-9a-f]{12}$`)

	// 空对象：Python 输出 {"content": [], "id": "msg_<24hex>", ..., "usage": {...0, 0}}
	got := OpenAIToAnthropic(map[string]any{}, "m")
	id, ok := got["id"].(string)
	if !ok || !msgIDPattern.MatchString(id) {
		t.Errorf("id = %#v，期望匹配 %s", got["id"], msgIDPattern)
	}
	if content := anthTestList(t, got["content"]); len(content) != 0 {
		t.Errorf("content = %s，期望 []", anthTestJSON(t, content))
	}
	if got["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %#v，期望 \"end_turn\"", got["stop_reason"])
	}
	if want := map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)}; !reflect.DeepEqual(got["usage"], want) {
		t.Errorf("usage = %s，期望 %s", anthTestJSON(t, got["usage"]), anthTestJSON(t, want))
	}

	// Python 输出：两个 tool_use，缺 id 的补 call_<12hex>，坏 arguments / 缺 arguments 都退化成 {}
	got = OpenAIToAnthropic(map[string]any{
		"id": "x",
		"choices": []any{map[string]any{"message": map[string]any{
			"tool_calls": []any{
				map[string]any{"id": "c1", "function": map[string]any{
					"name": "f", "arguments": "not json"}},
				map[string]any{"function": map[string]any{"name": "g"}},
			},
			"finish_reason": nil,
		}}},
	}, "m")
	content := anthTestList(t, got["content"])
	if len(content) != 2 {
		t.Fatalf("content 长度 = %d，期望 2: %s", len(content), anthTestJSON(t, content))
	}
	if want := `{"id":"c1","input":{},"name":"f","type":"tool_use"}`; anthTestJSON(t, content[0]) != want {
		t.Errorf("content[0] = %s，期望 %s", anthTestJSON(t, content[0]), want)
	}
	second := anthTestMap(t, content[1])
	if second["name"] != "g" {
		t.Errorf("content[1].name = %#v，期望 \"g\"", second["name"])
	}
	if want := map[string]any{}; !reflect.DeepEqual(second["input"], want) {
		t.Errorf("content[1].input = %s，期望 {}", anthTestJSON(t, second["input"]))
	}
	generated, ok := second["id"].(string)
	if !ok || !callIDPattern.MatchString(generated) {
		t.Errorf("content[1].id = %#v，期望匹配 %s", second["id"], callIDPattern)
	}
}

// 补充：usage 为 0 与缺失都归零。
func TestOpenAIToAnthropicUsageZero(t *testing.T) {
	got := OpenAIToAnthropic(map[string]any{
		"id":      "x",
		"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
		"usage":   map[string]any{"prompt_tokens": 0},
	}, "m")
	if want := map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)}; !reflect.DeepEqual(got["usage"], want) {
		t.Errorf("usage = %s，期望 %s", anthTestJSON(t, got["usage"]), anthTestJSON(t, want))
	}
}

// ---------------------------------------------------------------- 流式翻译

// 对应 test_stream_text_only。
func TestStreamTextOnly(t *testing.T) {
	_, events := anthTestRunStream(map[string]any{
		"choices": []any{map[string]any{
			"delta":         map[string]any{"content": "a"},
			"finish_reason": "length",
		}},
	})

	names := anthTestNames(events)
	if names[0] != "message_start" {
		t.Errorf("首个事件 = %q，期望 \"message_start\"", names[0])
	}
	if names[len(names)-1] != "message_stop" {
		t.Errorf("末个事件 = %q，期望 \"message_stop\"", names[len(names)-1])
	}

	starts := []any{}
	for _, data := range anthTestDataOf(events, "content_block_start") {
		starts = append(starts, anthTestMap(t, data["content_block"])["type"])
	}
	if want := []any{"text"}; !reflect.DeepEqual(starts, want) {
		t.Errorf("content_block_start 类型 = %v，期望 %v", starts, want)
	}

	messageDelta := anthTestDataOf(events, "message_delta")
	if len(messageDelta) != 1 {
		t.Fatalf("message_delta 数量 = %d，期望 1", len(messageDelta))
	}
	if got := anthTestMap(t, messageDelta[0]["delta"])["stop_reason"]; got != "max_tokens" {
		t.Errorf("stop_reason = %#v，期望 \"max_tokens\"", got)
	}

	anthTestCheckStream(t, events, []anthTestWant{
		{"message_start", anthTestMessageStart},
		{"content_block_start", `{"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}`},
		{"content_block_delta", `{"delta":{"text":"a","type":"text_delta"},"index":0,"type":"content_block_delta"}`},
		{"content_block_stop", `{"index":0,"type":"content_block_stop"}`},
		{"message_delta", `{"delta":{"stop_reason":"max_tokens","stop_sequence":null},"type":"message_delta","usage":{"output_tokens":0}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})
}

// 对应 test_stream_with_reasoning。
func TestStreamWithReasoning(t *testing.T) {
	st, events := anthTestRunStream(
		map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"reasoning_content": "hmm"},
		}}},
		map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": "hi"},
		}}},
		map[string]any{"choices": []any{}, "usage": map[string]any{
			"completion_tokens": 9, "points_consumed": 2,
		}},
	)

	names := anthTestNames(events)
	if names[0] != "message_start" {
		t.Errorf("首个事件 = %q，期望 \"message_start\"", names[0])
	}
	if names[len(names)-1] != "message_stop" {
		t.Errorf("末个事件 = %q，期望 \"message_stop\"", names[len(names)-1])
	}

	// 每个 delta 都落在 index 0 上：thinking 与 text 共用 0 号块。
	indexes := map[int]bool{}
	for _, data := range anthTestDataOf(events, "content_block_delta") {
		index, ok := data["index"].(int)
		if !ok {
			t.Fatalf("index 不是 int: %#v", data["index"])
		}
		indexes[index] = true
	}
	if len(indexes) != 1 || !indexes[0] {
		t.Errorf("content_block_delta 的 index 集合 = %v，期望 {0}", indexes)
	}

	kinds := []any{}
	for _, data := range anthTestDataOf(events, "content_block_start") {
		kinds = append(kinds, anthTestMap(t, data["content_block"])["type"])
	}
	if want := []any{"thinking", "text"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("content_block_start 类型 = %v，期望 %v", kinds, want)
	}

	messageDelta := anthTestDataOf(events, "message_delta")
	if len(messageDelta) != 1 {
		t.Fatalf("message_delta 数量 = %d，期望 1", len(messageDelta))
	}
	if got := anthTestMap(t, messageDelta[0]["usage"])["output_tokens"]; got != int64(9) {
		t.Errorf("message_delta.usage.output_tokens = %#v，期望 9", got)
	}
	if got := anthTestMap(t, messageDelta[0]["delta"])["stop_reason"]; got != "end_turn" {
		t.Errorf("message_delta.delta.stop_reason = %#v，期望 \"end_turn\"", got)
	}

	// usage 聚合的是上游 usage 原样合并（choices 为空的那一帧也要吞下 usage）。
	if want := map[string]any{"completion_tokens": 9, "points_consumed": 2}; !reflect.DeepEqual(st.Usage(), want) {
		t.Errorf("Usage() = %s，期望 %s", anthTestJSON(t, st.Usage()), anthTestJSON(t, want))
	}

	anthTestCheckStream(t, events, []anthTestWant{
		{"message_start", anthTestMessageStart},
		{"content_block_start", `{"content_block":{"thinking":"","type":"thinking"},"index":0,"type":"content_block_start"}`},
		{"content_block_delta", `{"delta":{"thinking":"hmm","type":"thinking_delta"},"index":0,"type":"content_block_delta"}`},
		{"content_block_stop", `{"index":0,"type":"content_block_stop"}`},
		{"content_block_start", `{"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}`},
		{"content_block_delta", `{"delta":{"text":"hi","type":"text_delta"},"index":0,"type":"content_block_delta"}`},
		{"content_block_stop", `{"index":0,"type":"content_block_stop"}`},
		{"message_delta", `{"delta":{"stop_reason":"end_turn","stop_sequence":null},"type":"message_delta","usage":{"output_tokens":9}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})
}

// 对应 test_stream_tool_call。
func TestStreamToolCall(t *testing.T) {
	_, events := anthTestRunStream(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"id":       "c1",
				"function": map[string]any{"name": "f", "arguments": `{"b":2}`},
			}}},
			"finish_reason": "tool_calls",
		}},
	})

	starts := []any{}
	for _, data := range anthTestDataOf(events, "content_block_start") {
		starts = append(starts, anthTestMap(t, data["content_block"])["type"])
	}
	if want := []any{"tool_use"}; !reflect.DeepEqual(starts, want) {
		t.Errorf("content_block_start 类型 = %v，期望 %v", starts, want)
	}

	var inputDelta map[string]any
	for _, data := range anthTestDataOf(events, "content_block_delta") {
		delta := anthTestMap(t, data["delta"])
		if delta["type"] == "input_json_delta" {
			inputDelta = delta
			break
		}
	}
	if inputDelta == nil {
		t.Fatalf("没有 input_json_delta 事件: %s", anthTestJSON(t, events))
	}
	var partial any
	raw, _ := inputDelta["partial_json"].(string)
	if err := json.Unmarshal([]byte(raw), &partial); err != nil {
		t.Fatalf("partial_json 不是合法 JSON (%q): %v", raw, err)
	}
	if want := map[string]any{"b": float64(2)}; !reflect.DeepEqual(partial, want) {
		t.Errorf("partial_json 解析 = %s，期望 %s", anthTestJSON(t, partial), anthTestJSON(t, want))
	}

	anthTestCheckStream(t, events, []anthTestWant{
		{"message_start", anthTestMessageStart},
		{"content_block_start", `{"content_block":{"id":"c1","input":{},"name":"f","type":"tool_use"},"index":0,"type":"content_block_start"}`},
		{"content_block_delta", `{"delta":{"partial_json":"{\"b\":2}","type":"input_json_delta"},"index":0,"type":"content_block_delta"}`},
		{"content_block_stop", `{"index":0,"type":"content_block_stop"}`},
		{"message_delta", `{"delta":{"stop_reason":"tool_use","stop_sequence":null},"type":"message_delta","usage":{"output_tokens":0}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})
}

// 补充：text 块开着时 tool_use 用 index = text_index + 1，
// 收尾先关 tool_use 再关 text（Python 实测序列）。
func TestStreamToolCallAfterText(t *testing.T) {
	_, events := anthTestRunStream(
		map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": "hi"},
		}}},
		map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"id":       "c1",
				"function": map[string]any{"name": "f", "arguments": `{"b":2}`},
			}}},
			"finish_reason": "tool_calls",
		}}},
	)

	anthTestCheckStream(t, events, []anthTestWant{
		{"message_start", anthTestMessageStart},
		{"content_block_start", `{"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}`},
		{"content_block_delta", `{"delta":{"text":"hi","type":"text_delta"},"index":0,"type":"content_block_delta"}`},
		{"content_block_start", `{"content_block":{"id":"c1","input":{},"name":"f","type":"tool_use"},"index":1,"type":"content_block_start"}`},
		{"content_block_delta", `{"delta":{"partial_json":"{\"b\":2}","type":"input_json_delta"},"index":1,"type":"content_block_delta"}`},
		{"content_block_stop", `{"index":1,"type":"content_block_stop"}`},
		{"content_block_stop", `{"index":0,"type":"content_block_stop"}`},
		{"message_delta", `{"delta":{"stop_reason":"tool_use","stop_sequence":null},"type":"message_delta","usage":{"output_tokens":0}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})
}

// 补充：choices 为空只积累 usage（不产事件），后续帧覆盖旧值（last write wins）。
func TestStreamUsageAggregation(t *testing.T) {
	st := NewStreamTranslator("m")
	if len(st.Start()) != 1 {
		t.Fatalf("Start() 应只产生 1 个事件")
	}
	if events := st.Feed(map[string]any{"usage": map[string]any{
		"completion_tokens": 9, "prompt_tokens": 5,
	}}); len(events) != 0 {
		t.Errorf("choices 缺失时不应产生事件: %s", anthTestJSON(t, events))
	}
	if events := st.Feed(map[string]any{
		"choices": []any{},
		"usage":   map[string]any{"completion_tokens": 11},
	}); len(events) != 0 {
		t.Errorf("choices 为空时不应产生事件: %s", anthTestJSON(t, events))
	}
	if want := map[string]any{"completion_tokens": 11, "prompt_tokens": 5}; !reflect.DeepEqual(st.Usage(), want) {
		t.Errorf("Usage() = %s，期望 %s", anthTestJSON(t, st.Usage()), anthTestJSON(t, want))
	}
	anthTestCheckStream(t, st.Finish(), []anthTestWant{
		{"message_delta", `{"delta":{"stop_reason":"end_turn","stop_sequence":null},"type":"message_delta","usage":{"output_tokens":11}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})
}

// 补充：message_start 的 id 形如 msg_<24hex>。
func TestStreamMessageStartID(t *testing.T) {
	st := NewStreamTranslator("m")
	events := st.Start()
	if len(events) != 1 || events[0].Name != "message_start" {
		t.Fatalf("Start() = %s，期望单个 message_start", anthTestJSON(t, events))
	}
	message := anthTestMap(t, events[0].Data["message"])
	id, ok := message["id"].(string)
	if !ok || !regexp.MustCompile(`^msg_[0-9a-f]{24}$`).MatchString(id) {
		t.Errorf("message.id = %#v，期望 msg_<24hex>", message["id"])
	}
	if got := message["model"]; got != "m" {
		t.Errorf("message.model = %#v，期望 \"m\"", got)
	}

	// 两个实例的 id 不应相同。
	if other := anthTestMap(t, NewStreamTranslator("m").Start()[0].Data["message"])["id"]; other == id {
		t.Errorf("两次生成的 message.id 相同: %v", id)
	}
}

// ---------------------------------------------------------------- 纯函数

// 对应 Python 的 _blocks_to_text。
func TestBlocksToText(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"字符串", "abc", "abc"},
		{"nil", nil, ""},
		{"标量", 5, "5"},
		{"单 text 块", []any{map[string]any{"type": "text", "text": "a"}}, "a"},
		{"混合块", []any{
			map[string]any{"type": "text", "text": "a"},
			map[string]any{"type": "tool_result", "content": []any{
				map[string]any{"type": "text", "text": "b"},
			}},
			"c",
			map[string]any{"type": "text", "text": ""},
			map[string]any{},
			nil,
			[]any{"x"},
		}, "a\nb\nc"},
	}
	for _, tc := range cases {
		if got := BlocksToText(tc.in); got != tc.want {
			t.Errorf("%s: BlocksToText(%#v) = %q，期望 %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// EstimateTokens：max(1, len(text)//4)，长度按码点算。
func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 1},
		{"abc", 1},
		{"abcd", 1},
		{"abcde", 1},
		{"abcdefgh", 2},
		{strings.Repeat("a", 100), 25},
		{"你好世界", 1},     // 4 个码点（不是 12 字节）
		{"你好世界你好世界", 2}, // 8 个码点
	}
	for _, tc := range cases {
		if got := EstimateTokens(tc.in); got != tc.want {
			t.Errorf("EstimateTokens(%q) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}
