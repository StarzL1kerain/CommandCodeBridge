package bridge

import (
	"encoding/json"
	"strings"
	"time"
)

// Command Code 的 Claude 系模型只接受 Anthropic Messages 端点（/provider/v1/messages，
// supported_endpoints 只有 "/messages"），其余模型走 OpenAI Chat Completions。
// CPA 发给插件的是 OpenAI Chat Completions 格式，因此对 Claude 模型需要双向转换：
//
//	请求：  OpenAI messages/tools/tool_choice -> Anthropic messages/tools/tool_choice
//	响应：  Anthropic content blocks          -> OpenAI choices[].message
//	流式：  Anthropic SSE 事件                -> OpenAI chat.completion.chunk 帧
//
// 转换规则以 Anthropic Messages 官方 schema 为准；thinking 块映射到
// reasoning_content（DeepSeek 风格），宿主与客户端均已兼容该字段。

// anthropicDefaultMaxTokens 是 Anthropic 请求必填 max_tokens 的兜底值。
const anthropicDefaultMaxTokens = 16384

// textBlock 构造一个 Anthropic 文本块。
func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// openAIToAnthropic 把 OpenAI Chat Completions 请求体转为 Anthropic Messages 请求体。
func openAIToAnthropic(j map[string]any) (map[string]any, error) {
	out := map[string]any{}
	if v, ok := j["model"]; ok {
		out["model"] = v
	}
	// max_tokens 必填：优先 max_tokens，其次 max_completion_tokens，最后兜底。
	maxTokens := number(j["max_tokens"])
	if maxTokens <= 0 {
		maxTokens = number(j["max_completion_tokens"])
	}
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	out["max_tokens"] = maxTokens
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := j[key]; ok && v != nil {
			out[key] = v
		}
	}
	if stream, ok := j["stream"].(bool); ok {
		out["stream"] = stream
	}

	// system / developer 消息合并为顶层 system；tool 消息转 tool_result。
	systems := []string{}
	messages := []any{}
	for _, raw := range list(j["messages"]) {
		m := object(raw)
		if m == nil {
			continue
		}
		role := str(m["role"])
		switch role {
		case "system", "developer":
			if text := openAIContentText(m["content"]); text != "" {
				systems = append(systems, text)
			}
		case "tool":
			messages = append(messages, anthropicToolResult(m))
		default:
			if blocks := anthropicMessageBlocks(m); len(blocks) > 0 {
				content := any(blocks)
				// 单纯文本时用字符串形式，贴近 Anthropic 官方客户端的默认序列化。
				if len(blocks) == 1 && str(object(blocks[0])["type"]) == "text" {
					content = str(object(blocks[0])["text"])
				}
				messages = append(messages, map[string]any{"role": role, "content": content})
			}
		}
	}
	if len(messages) == 0 {
		return nil, fail(400, "messages 必须是非空数组")
	}
	out["messages"] = messages
	if len(systems) > 0 {
		out["system"] = strings.Join(systems, "\n\n")
	}

	// tools：OpenAI function 定义 -> Anthropic input_schema。
	if tools := list(j["tools"]); len(tools) > 0 {
		converted := []any{}
		for _, raw := range tools {
			t := object(raw)
			if t == nil {
				continue
			}
			fn := object(t["function"])
			name := str(t["function"])
			if fn != nil {
				name = str(fn["name"])
			}
			if name == "" {
				name = str(t["name"])
			}
			if name == "" {
				continue
			}
			schema := map[string]any{"type": "object", "properties": map[string]any{}}
			if fn != nil {
				if s := object(fn["parameters"]); s != nil {
					schema = s
				}
			}
			entry := map[string]any{"name": name, "input_schema": schema}
			if fn != nil {
				if d := str(fn["description"]); d != "" {
					entry["description"] = d
				}
			}
			converted = append(converted, entry)
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	// tool_choice："auto"/"none" -> auto；"required" -> any；指定函数 -> tool。
	if choice := j["tool_choice"]; choice != nil {
		if name, ok := choice.(string); ok {
			switch name {
			case "auto", "none":
				out["tool_choice"] = map[string]any{"type": "auto"}
			case "required":
				out["tool_choice"] = map[string]any{"type": "any"}
			}
		} else if c := object(choice); c != nil {
			switch str(c["type"]) {
			case "function":
				if fn := object(c["function"]); fn != nil && str(fn["name"]) != "" {
					out["tool_choice"] = map[string]any{"type": "tool", "name": str(fn["name"])}
				} else {
					out["tool_choice"] = map[string]any{"type": "auto"}
				}
			case "required":
				out["tool_choice"] = map[string]any{"type": "any"}
			}
		}
	}
	return out, nil
}

// openAIContentText 把 OpenAI 的字符串或分段 content 拼接为纯文本。
func openAIContentText(v any) string {
	if s := str(v); s != "" {
		return s
	}
	parts := []string{}
	for _, raw := range list(v) {
		if text := str(raw); text != "" {
			parts = append(parts, text)
			continue
		}
		if b := object(raw); b != nil && str(b["type"]) == "text" {
			parts = append(parts, str(b["text"]))
		}
	}
	return strings.Join(parts, "")
}

// anthropicMessageBlocks 转换 user/assistant 消息体。
// assistant 的 tool_calls 变成 tool_use 块；文本 content 变成 text 块。
func anthropicMessageBlocks(m map[string]any) []any {
	blocks := []any{}
	for _, raw := range list(m["tool_calls"]) {
		tc := object(raw)
		fn := object(tc["function"])
		if fn == nil || str(fn["name"]) == "" {
			continue
		}
		input := map[string]any{}
		if args := str(fn["arguments"]); args != "" {
			var parsed map[string]any
			if json.Unmarshal([]byte(args), &parsed) == nil && parsed != nil {
				input = parsed
			}
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    str(tc["id"]),
			"name":  str(fn["name"]),
			"input": input,
		})
	}
	if text := openAIContentText(m["content"]); text != "" {
		blocks = append(blocks, textBlock(text))
	}
	return blocks
}

// anthropicToolResult 把 OpenAI 的 tool 消息转为带 tool_result 块的 user 消息。
func anthropicToolResult(m map[string]any) map[string]any {
	return map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type":        "tool_result",
			"tool_use_id": str(m["tool_call_id"]),
			"content":     openAIContentText(m["content"]),
		}},
	}
}

// anthropicStopReason 把 Anthropic stop_reason 映射为 OpenAI finish_reason。
func anthropicStopReason(reason string) any {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	case "":
		return nil
	default:
		return "stop"
	}
}

// openAIUsage 把 Anthropic usage 映射为 OpenAI usage。
func openAIUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	out := map[string]any{
		"prompt_tokens":     number(u["input_tokens"]),
		"completion_tokens": number(u["output_tokens"]),
	}
	if cached := number(u["cache_read_input_tokens"]); cached > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return out
}

// anthropicToOpenAIResponse 把 Anthropic 非流式响应转为 OpenAI Chat Completions 响应。
func anthropicToOpenAIResponse(j map[string]any, model string) (map[string]any, error) {
	if j["type"] == "error" || j["error"] != nil {
		return nil, fail(502, errorMessage(j))
	}
	var text, reasoning string
	toolCalls := []any{}
	for _, raw := range list(j["content"]) {
		block := object(raw)
		if block == nil {
			continue
		}
		switch str(block["type"]) {
		case "text":
			text += str(block["text"])
		case "thinking":
			reasoning += str(block["thinking"])
		case "tool_use":
			args := jsonBytes(block["input"])
			if len(args) == 0 {
				args = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   str(block["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      str(block["name"]),
					"arguments": string(args),
				},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":      anthropicMessageID(j),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": anthropicStopReason(str(j["stop_reason"])),
		}},
	}
	if usage := openAIUsage(object(j["usage"])); usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

func anthropicMessageID(j map[string]any) string {
	if v := str(j["id"]); v != "" {
		return v
	}
	return "chatcmpl-" + id()
}

// ---- 流式转换：Anthropic SSE 事件 -> OpenAI chunk ----

// anthropicStream 是有状态的流式转换器。CPA 拥有下游 SSE 信封与终止标记，
// 这里只产出与 OpenAI 兼容的 chunk 对象。
type anthropicStream struct {
	id       string
	created  int64
	usage    map[string]any
	toolSeq  map[int64]int64 // Anthropic content block 序号 -> OpenAI tool_calls 序号
	nextTool int64
	stop     string
	stopped  bool
}

func newAnthropicStream() *anthropicStream {
	return &anthropicStream{id: "chatcmpl-" + id(), created: time.Now().Unix(), toolSeq: map[int64]int64{}}
}

func (a *anthropicStream) chunk(delta map[string]any, finish any, usage map[string]any) map[string]any {
	out := map[string]any{
		"id":      a.id,
		"object":  "chat.completion.chunk",
		"created": a.created,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

// feed 处理一个 Anthropic SSE 事件，返回要下发的 OpenAI chunk 列表。
// 第三个返回值为 true 表示流已正常结束（等价于 OpenAI 的 [DONE]）。
func (a *anthropicStream) feed(event string, payload []byte) ([]map[string]any, bool, error) {
	j, err := decodeObject(payload)
	if err != nil {
		return nil, false, err
	}
	kind := str(j["type"])
	if kind == "" {
		kind = event
	}
	switch kind {
	case "message_start":
		if message := object(j["message"]); message != nil {
			if v := str(message["id"]); v != "" {
				a.id = v
			}
			if u := object(message["usage"]); u != nil {
				a.usage = map[string]any{"input_tokens": u["input_tokens"], "output_tokens": u["output_tokens"]}
			}
		}
		return []map[string]any{a.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)}, false, nil
	case "content_block_start":
		block := object(j["content_block"])
		if block == nil || str(block["type"]) != "tool_use" {
			return nil, false, nil
		}
		index := number(j["index"])
		seq := a.nextTool
		a.nextTool++
		a.toolSeq[index] = seq
		return []map[string]any{a.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": seq,
			"id":    str(block["id"]),
			"type":  "function",
			"function": map[string]any{
				"name":      str(block["name"]),
				"arguments": "",
			},
		}}}, nil, nil)}, false, nil
	case "content_block_delta":
		delta := object(j["delta"])
		if delta == nil {
			return nil, false, nil
		}
		index := number(j["index"])
		switch str(delta["type"]) {
		case "text_delta":
			return []map[string]any{a.chunk(map[string]any{"content": str(delta["text"])}, nil, nil)}, false, nil
		case "input_json_delta":
			seq, ok := a.toolSeq[index]
			if !ok {
				seq = a.nextTool
				a.toolSeq[index] = seq
				a.nextTool++
			}
			return []map[string]any{a.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index":    seq,
				"function": map[string]any{"arguments": str(delta["partial_json"])},
			}}}, nil, nil)}, false, nil
		case "thinking_delta":
			return []map[string]any{a.chunk(map[string]any{"reasoning_content": str(delta["thinking"])}, nil, nil)}, false, nil
		}
		return nil, false, nil
	case "message_delta":
		if delta := object(j["delta"]); delta != nil {
			a.stop = str(delta["stop_reason"])
		}
		if u := object(j["usage"]); u != nil {
			if a.usage == nil {
				a.usage = map[string]any{}
			}
			if v, ok := u["output_tokens"]; ok {
				a.usage["output_tokens"] = v
			}
		}
		return nil, false, nil
	case "message_stop":
		a.stopped = true
		return []map[string]any{a.chunk(map[string]any{}, anthropicStopReason(a.stop), openAIUsage(a.usage))}, true, nil
	case "ping", "content_block_stop":
		return nil, false, nil
	case "error":
		return nil, false, fail(502, errorMessage(j))
	default:
		return nil, false, nil
	}
}
