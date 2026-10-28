package loomy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 对应 Python loomy2api/anthropic.py —— Anthropic Messages API 与
// OpenAI Chat Completions 的协议互转（含流式）。
// 上游的 reasoning_content 会被翻译成 Anthropic 的 thinking 块，避免静默丢弃推理内容。

// AnthropicEvent 是一条 Anthropic SSE 事件：event: <Name>\ndata: <json(Data)>。
type AnthropicEvent struct {
	Name string
	Data map[string]any
}

// ---------------------------------------------------------------- 取值辅助

// anthTruthy 对应 Python 的真值判断（None/False/0/""/空容器为假）。
func anthTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int32:
		return t != 0
	case int64:
		return t != 0
	case float32:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case []string:
		return len(t) > 0
	case []map[string]any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

// anthOr 对应 Python 的 `v or def`。
func anthOr(v any, def any) any {
	if anthTruthy(v) {
		return v
	}
	return def
}

// anthStr 对应 Python 的 str(v)。
func anthStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// anthJoin 对应 Python 的 "\n".join(p for p in parts if p)。
func anthJoin(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// anthBlockList 把 content 归一成 block 列表；字符串/对象等非列表返回 false。
func anthBlockList(content any) ([]any, bool) {
	switch v := content.(type) {
	case []any:
		return v, true
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out, true
	case []map[string]any:
		out := make([]any, len(v))
		for i, b := range v {
			out[i] = b
		}
		return out, true
	}
	return nil, false
}

// anthHex 生成 n 个十六进制字符（对应 Python uuid.uuid4().hex[:n]）。
func anthHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, 16)
	// crypto/rand.Read 在 Go 1.24+ 内部直接 panic，不会返回错误；
	// 这里保守回退成固定串，保证这个分支不可达时代码自身仍然合法。
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n)
	}
	out := hex.EncodeToString(buf)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// anthJSON 对应 Python 的 json.dumps(v, ensure_ascii=False)。
// Go 默认会把 <>& 转义成 \u003c 等，关掉以贴近 Python 的可读输出。
func anthJSON(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimRight(sb.String(), "\n")
}

// anthArgs 对应 Python 的 json.dumps(input or {}, ensure_ascii=False)。
func anthArgs(v any) string {
	return anthJSON(anthOr(v, map[string]any{}))
}

// anthParseArgs 对应 Python 的 json.loads(fn.get("arguments") or "{}")，
// 解析失败退回 {}。
func anthParseArgs(v any) any {
	raw := "{}"
	if s, ok := v.(string); ok && s != "" {
		raw = s
	}
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]any{}
	}
	return out
}

// anthBlockText 取单个 block 对应的纯文本；不认识的 block 返回 ""。
func anthBlockText(block any) string {
	switch b := block.(type) {
	case map[string]any:
		switch jstr(b, "type") {
		case "text":
			return anthTextOf(b)
		case "tool_result":
			return BlocksToText(b["content"])
		}
	case string:
		return b
	}
	return ""
}

// anthTextOf 对应 Python 的 str(block.get("text") or "")。
func anthTextOf(block map[string]any) string {
	v := block["text"]
	if !anthTruthy(v) {
		return ""
	}
	return anthStr(v)
}

// anthToolResultText 对应 Python 的 _tool_result_text。
func anthToolResultText(block map[string]any) string {
	if s, ok := block["content"].(string); ok {
		return s
	}
	return BlocksToText(block["content"])
}

// anthStopReason 把 OpenAI 的 finish_reason 映射成 Anthropic 的 stop_reason。
func anthStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// ---------------------------------------------------------------- 请求翻译

// BlocksToText 把 Anthropic 的 content（字符串或 block 列表）压平成纯文本。
func BlocksToText(content any) string {
	if s, isStr := content.(string); isStr {
		return s
	}
	if blocks, isList := anthBlockList(content); isList {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			parts = append(parts, anthBlockText(block))
		}
		return anthJoin(parts)
	}
	return anthStr(content)
}

// AnthropicToOpenAI 把 /v1/messages 请求翻译成 Chat Completions 请求体。
func AnthropicToOpenAI(req map[string]any) map[string]any {
	messages := []any{}

	if system := req["system"]; anthTruthy(system) {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": BlocksToText(system),
		})
	}

	for _, rawMessage := range jlist(req, "messages") {
		message, isMap := rawMessage.(map[string]any)
		if !isMap {
			continue
		}
		role := message["role"]
		content := message["content"]
		if text, isStr := content.(string); isStr {
			messages = append(messages, map[string]any{"role": role, "content": text})
			continue
		}

		texts := []string{}
		toolCalls := []any{}
		toolMessages := []any{}
		blocks, _ := anthBlockList(content)
		for _, rawBlock := range blocks {
			block, isBlockMap := rawBlock.(map[string]any)
			if !isBlockMap {
				continue
			}
			switch jstr(block, "type") {
			case "text":
				texts = append(texts, anthTextOf(block))
			case "tool_use":
				toolCalls = append(toolCalls, map[string]any{
					"id":   anthOr(block["id"], "call_"+anthHex(12)),
					"type": "function",
					"function": map[string]any{
						"name":      block["name"],
						"arguments": anthArgs(block["input"]),
					},
				})
			case "tool_result":
				toolMessages = append(toolMessages, map[string]any{
					"role":         "tool",
					"tool_call_id": anthOr(block["tool_use_id"], ""),
					"content":      anthToolResultText(block),
				})
			}
		}

		// tool_result 块来自 user 消息：它们变成独立的 tool 消息，
		// 且不产生一个空的 user 脚手架。
		if len(texts) > 0 || len(toolCalls) > 0 {
			var joined any // 对应 "\n".join(...) or None
			if s := anthJoin(texts); s != "" {
				joined = s
			}
			outMessage := map[string]any{"role": role, "content": joined}
			if len(toolCalls) > 0 {
				outMessage["tool_calls"] = toolCalls
			}
			messages = append(messages, outMessage)
		}
		messages = append(messages, toolMessages...)
	}

	out := map[string]any{
		"model":    req["model"],
		"messages": messages,
		"stream":   anthTruthy(req["stream"]),
	}
	for _, key := range []string{"max_tokens", "temperature", "top_p"} {
		if v, ok := req[key]; ok && v != nil {
			out[key] = v
		}
	}
	if stop := req["stop_sequences"]; anthTruthy(stop) {
		out["stop"] = stop
	}

	if tools := req["tools"]; anthTruthy(tools) {
		list := []any{}
		for _, rawTool := range jlist(req, "tools") {
			tool, isToolMap := rawTool.(map[string]any)
			if !isToolMap || !anthTruthy(tool["name"]) {
				continue
			}
			params := tool["input_schema"]
			if !anthTruthy(params) {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			list = append(list, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool["name"],
					"description": anthOr(tool["description"], ""),
					"parameters":  params,
				},
			})
		}
		out["tools"] = list
	}

	if choice, isChoiceMap := req["tool_choice"].(map[string]any); isChoiceMap {
		switch jstr(choice, "type") {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			if anthTruthy(choice["name"]) {
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": choice["name"]},
				}
			}
		}
	}
	return out
}

// OpenAIToAnthropic 把非流式的 Chat Completion 翻译成 Anthropic message。
func OpenAIToAnthropic(obj map[string]any, model string) map[string]any {
	choice := map[string]any{}
	if choices := jlist(obj, "choices"); len(choices) > 0 {
		if c, isChoiceMap := choices[0].(map[string]any); isChoiceMap {
			choice = c
		}
	}
	message := jmap(choice, "message")
	if message == nil {
		message = map[string]any{}
	}

	content := []any{}
	if reasoning := message["reasoning_content"]; anthTruthy(reasoning) {
		content = append(content, map[string]any{
			"type":      "thinking",
			"thinking":  reasoning,
			"signature": "",
		})
	}
	if text := message["content"]; anthTruthy(text) {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, rawCall := range jlist(message, "tool_calls") {
		call, isCallMap := rawCall.(map[string]any)
		if !isCallMap {
			continue
		}
		fn := jmap(call, "function")
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    anthOr(call["id"], "call_"+anthHex(12)),
			"name":  fn["name"],
			"input": anthParseArgs(fn["arguments"]),
		})
	}

	usage := jmap(obj, "usage")
	return map[string]any{
		"id":            anthOr(obj["id"], "msg_"+anthHex(24)),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthStopReason(jstr(choice, "finish_reason")),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  jint(usage, "prompt_tokens"),
			"output_tokens": jint(usage, "completion_tokens"),
		},
	}
}

// EstimateTokens 是流式 message_start 里 usage 的粗略估算：max(1, len(text)//4)。
// 长度按 Unicode 码点算，与 Python 的 len(str) 一致。
func EstimateTokens(text string) int {
	if n := utf8.RuneCountInString(text) / 4; n > 1 {
		return n
	}
	return 1
}

// ---------------------------------------------------------------- 流式翻译

// StreamTranslator 把 OpenAI SSE chunk 转换成 Anthropic SSE 事件。
//
//	st := NewStreamTranslator(model)
//	for _, ev := range st.Start() { emit(ev) }
//	for _, chunk := range openaiChunks {
//	    for _, ev := range st.Feed(chunk) { emit(ev) }
//	}
//	for _, ev := range st.Finish() { emit(ev) }
type StreamTranslator struct {
	model        string
	messageID    string
	usage        map[string]any
	finishReason string
	thinkOpen    bool
	textOpen     bool
	textIndex    int
}

// NewStreamTranslator 对应 Python 的 StreamTranslator(model)。
func NewStreamTranslator(model string) *StreamTranslator {
	return &StreamTranslator{
		model:     model,
		messageID: "msg_" + anthHex(24),
		usage:     map[string]any{},
	}
}

// Start 产生 message_start 事件。
func (st *StreamTranslator) Start() []AnthropicEvent {
	return []AnthropicEvent{{
		Name: "message_start",
		Data: map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            st.messageID,
				"type":          "message",
				"role":          "assistant",
				"model":         st.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		},
	}}
}

// Feed 消费一个上游 chunk，返回需要下发的 Anthropic 事件。
func (st *StreamTranslator) Feed(obj map[string]any) []AnthropicEvent {
	if usage, isUsageMap := obj["usage"].(map[string]any); isUsageMap {
		for k, v := range usage {
			st.usage[k] = v
		}
	}

	choices := jlist(obj, "choices")
	if len(choices) == 0 {
		return nil
	}
	choice, isChoiceMap := choices[0].(map[string]any)
	if !isChoiceMap {
		return nil
	}
	delta := jmap(choice, "delta")
	if delta == nil {
		delta = map[string]any{}
	}

	var events []AnthropicEvent

	if thinking := delta["reasoning_content"]; anthTruthy(thinking) {
		if !st.thinkOpen {
			st.thinkOpen = true
			events = append(events, AnthropicEvent{
				Name: "content_block_start",
				Data: map[string]any{
					"type":          "content_block_start",
					"index":         0,
					"content_block": map[string]any{"type": "thinking", "thinking": ""},
				},
			})
		}
		events = append(events, AnthropicEvent{
			Name: "content_block_delta",
			Data: map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
			},
		})
	}

	if text := delta["content"]; anthTruthy(text) {
		if st.thinkOpen && !st.textOpen {
			st.thinkOpen = false
			events = append(events, AnthropicEvent{
				Name: "content_block_stop",
				Data: map[string]any{"type": "content_block_stop", "index": 0},
			})
		}
		if !st.textOpen {
			st.textOpen = true
			st.textIndex = 0
			events = append(events, AnthropicEvent{
				Name: "content_block_start",
				Data: map[string]any{
					"type":          "content_block_start",
					"index":         st.textIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				},
			})
		}
		events = append(events, AnthropicEvent{
			Name: "content_block_delta",
			Data: map[string]any{
				"type":  "content_block_delta",
				"index": st.textIndex,
				"delta": map[string]any{"type": "text_delta", "text": text},
			},
		})
	}

	// 工具调用整块下发（上游一次给全）。
	for _, rawCall := range jlist(delta, "tool_calls") {
		call, isCallMap := rawCall.(map[string]any)
		if !isCallMap {
			continue
		}
		fn := jmap(call, "function")
		if !anthTruthy(fn["name"]) {
			continue
		}
		index := 0
		if st.textOpen {
			index = st.textIndex + 1
		}
		events = append(events,
			AnthropicEvent{
				Name: "content_block_start",
				Data: map[string]any{
					"type":  "content_block_start",
					"index": index,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    anthOr(call["id"], "call_"+anthHex(12)),
						"name":  fn["name"],
						"input": map[string]any{},
					},
				},
			},
			AnthropicEvent{
				Name: "content_block_delta",
				Data: map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": anthJSON(anthParseArgs(fn["arguments"])),
					},
				},
			},
			AnthropicEvent{
				Name: "content_block_stop",
				Data: map[string]any{"type": "content_block_stop", "index": index},
			},
		)
	}

	if finish := jstr(choice, "finish_reason"); finish != "" {
		st.finishReason = finish
	}
	return events
}

// Finish 收尾：关闭还开着的块，然后发 message_delta + message_stop。
func (st *StreamTranslator) Finish() []AnthropicEvent {
	var events []AnthropicEvent
	if st.thinkOpen {
		st.thinkOpen = false
		events = append(events, AnthropicEvent{
			Name: "content_block_stop",
			Data: map[string]any{"type": "content_block_stop", "index": 0},
		})
	}
	if st.textOpen {
		st.textOpen = false
		events = append(events, AnthropicEvent{
			Name: "content_block_stop",
			Data: map[string]any{"type": "content_block_stop", "index": st.textIndex},
		})
	}
	return append(events,
		AnthropicEvent{
			Name: "message_delta",
			Data: map[string]any{
				"type": "message_delta",
				"delta": map[string]any{
					"stop_reason":   anthStopReason(st.finishReason),
					"stop_sequence": nil,
				},
				"usage": map[string]any{"output_tokens": jint(st.usage, "completion_tokens")},
			},
		},
		AnthropicEvent{
			Name: "message_stop",
			Data: map[string]any{"type": "message_stop"},
		},
	)
}

// Usage 对应 Python 的 translator.usage 属性：聚合后的上游 usage（last write wins）。
func (st *StreamTranslator) Usage() map[string]any {
	return st.usage
}
