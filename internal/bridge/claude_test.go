package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestModelNeedsAnthropicUsesCatalogThenFallback(t *testing.T) {
	s := registeredService(t, "")
	// 默认快照：Claude 系走 /messages。
	if !s.modelNeedsAnthropic("claude-sonnet-5") {
		t.Fatal("claude-sonnet-5 should route to /messages")
	}
	if s.modelNeedsAnthropic("deepseek/deepseek-v4-flash") {
		t.Fatal("deepseek model should route to /chat/completions")
	}
	// 未收录模型按命名兜底。
	if !s.modelNeedsAnthropic("claude-future-9") {
		t.Fatal("unknown claude model should fall back to /messages")
	}
	// 目录刷新后以精确结果为准。
	s.mu.Lock()
	s.modelEndpoints["claude-future-9"] = []string{"/chat/completions", "/responses"}
	s.mu.Unlock()
	if s.modelNeedsAnthropic("claude-future-9") {
		t.Fatal("catalog result should override name-based fallback")
	}
}

func TestOpenAIToAnthropicConvertsSystemToolsAndToolResults(t *testing.T) {
	source := map[string]any{
		"model":    "claude-sonnet-5",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call-1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": `{"q":"go"}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "result text"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "lookup", "description": "search", "parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}}},
		"tool_choice": "required",
		"max_completion_tokens": 512,
		"stream":     true,
		"provider":   "ignored",
	}
	out, err := openAIToAnthropic(source)
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "claude-sonnet-5" || out["system"] != "be brief" {
		t.Fatalf("model/system = %#v", out)
	}
	if number(out["max_tokens"]) != 512 {
		t.Fatalf("max_tokens = %#v", out["max_tokens"])
	}
	if _, present := out["provider"]; present {
		t.Fatal("provider must be stripped")
	}
	messages := list(out["messages"])
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	if str(object(messages[0])["role"]) != "user" || str(messages[0].(map[string]any)["content"]) != "hello" {
		t.Fatalf("first message = %#v", messages[0])
	}
	assistant := object(messages[1])
	if assistant["role"] != "assistant" {
		t.Fatalf("assistant role = %#v", assistant)
	}
	blocks := list(assistant["content"])
	if len(blocks) != 1 || str(object(blocks[0])["type"]) != "tool_use" || str(object(blocks[0])["name"]) != "lookup" {
		t.Fatalf("tool_use block = %#v", blocks)
	}
	if str(object(object(blocks[0])["input"])["q"]) != "go" {
		t.Fatalf("tool input = %#v", blocks[0])
	}
	toolResult := object(messages[2])
	if toolResult["role"] != "user" {
		t.Fatalf("tool result role = %#v", toolResult)
	}
	result := object(list(toolResult["content"])[0])
	if str(result["type"]) != "tool_result" || str(result["tool_use_id"]) != "call-1" || str(result["content"]) != "result text" {
		t.Fatalf("tool_result = %#v", result)
	}
	tools := list(out["tools"])
	if len(tools) != 1 || str(object(tools[0])["name"]) != "lookup" || object(tools[0])["input_schema"] == nil {
		t.Fatalf("tools = %#v", tools)
	}
	if choice := object(out["tool_choice"]); choice["type"] != "any" {
		t.Fatalf("tool_choice = %#v", out["tool_choice"])
	}
	if stream, _ := out["stream"].(bool); !stream {
		t.Fatalf("stream = %#v", out["stream"])
	}
}

func TestAnthropicToOpenAIResponseMapsBlocksStopReasonAndUsage(t *testing.T) {
	source := map[string]any{
		"id": "msg_1",
		"type": "message",
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "ponder"},
			map[string]any{"type": "text", "text": "answer"},
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"q": "go"}},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 11, "output_tokens": 4, "cache_read_input_tokens": 6},
	}
	out, err := anthropicToOpenAIResponse(source, "claude-alias")
	if err != nil {
		t.Fatal(err)
	}
	if out["object"] != "chat.completion" || out["model"] != "claude-alias" || out["id"] != "msg_1" {
		t.Fatalf("envelope = %#v", out)
	}
	choice := object(list(out["choices"])[0])
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
	message := object(choice["message"])
	if message["content"] != "answer" || message["reasoning_content"] != "ponder" {
		t.Fatalf("message = %#v", message)
	}
	toolCalls := list(message["tool_calls"])
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %#v", toolCalls)
	}
	fn := object(object(toolCalls[0])["function"])
	if fn["name"] != "lookup" || str(fn["arguments"]) != `{"q":"go"}` {
		t.Fatalf("tool call = %#v", toolCalls[0])
	}
	usage := object(out["usage"])
	if number(usage["prompt_tokens"]) != 11 || number(usage["completion_tokens"]) != 4 || number(object(usage["prompt_tokens_details"])["cached_tokens"]) != 6 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := map[string]any{
		"end_turn": "stop", "stop_sequence": "stop", "tool_use": "tool_calls",
		"max_tokens": "length", "refusal": "content_filter", "": nil,
	}
	for reason, want := range cases {
		if got := anthropicStopReason(reason); got != want {
			t.Fatalf("anthropicStopReason(%q) = %#v, want %#v", reason, got, want)
		}
	}
}

// Claude 模型的完整链路：请求打到 /messages 且已转 Anthropic 格式，
// 非流响应转回 OpenAI 后交给宿主。
func TestClaudeModelRoutesToMessagesAndTranslatesResponse(t *testing.T) {
	s := registeredService(t, "native")
	var capturedMethod, capturedURL, capturedBody string
	h := newCaptureHost(jsonPlan(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant",
		"content":      []any{map[string]any{"type": "text", "text": "bonjour"}},
		"stop_reason":  "end_turn",
		"usage":        map[string]any{"input_tokens": 5, "output_tokens": 1},
	}))
	h.capture = func(method, url, body string) {
		capturedMethod, capturedURL, capturedBody = method, url, body
	}
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest("", "claude-sonnet-5"))
	if err != nil {
		t.Fatalf("execute claude: %v", err)
	}
	if capturedMethod != "POST" || !strings.HasSuffix(capturedURL, "/provider/v1/messages") {
		t.Fatalf("claude call = %s %s", capturedMethod, capturedURL)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(capturedBody), &sent); err != nil {
		t.Fatal(err)
	}
	if _, has := sent["messages"]; !has {
		t.Fatalf("anthropic request = %#v", sent)
	}
	if number(sent["max_tokens"]) <= 0 {
		t.Fatalf("anthropic max_tokens = %#v", sent["max_tokens"])
	}
	var body map[string]any
	if err := json.Unmarshal(result.(Response).Payload, &body); err != nil {
		t.Fatal(err)
	}
	message := object(object(list(body["choices"])[0])["message"])
	if message["content"] != "bonjour" || body["model"] != "claude-sonnet-5" {
		t.Fatalf("translated response = %#v", body)
	}
}

// Claude 模型流式链路：Anthropic 事件流聚合为 OpenAI completion。
func TestClaudeModelAggregatesAnthropicStream(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	h := newCaptureHost(ssePlan(simpleAnthropicSSE()))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest("", "claude-sonnet-5"))
	if err != nil {
		t.Fatalf("execute claude stream: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.(Response).Payload, &body); err != nil {
		t.Fatal(err)
	}
	choice := object(list(body["choices"])[0])
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
	message := object(choice["message"])
	if message["content"] != "hi" {
		t.Fatalf("message = %#v", message)
	}
	usage := object(body["usage"])
	if number(usage["prompt_tokens"]) != 9 || number(usage["completion_tokens"]) != 2 {
		t.Fatalf("usage = %#v", usage)
	}
	if len(s.logs) != 1 || s.logs[0].CompletionTokens != 2 {
		t.Fatalf("stream log = %#v", s.logs)
	}
}

// Anthropic 流式直发：宿主拿到的是 OpenAI chunk，且 Claude 端点的请求体里
// 不应出现 stream_options。
func TestClaudeStreamEmitsOpenAIChunks(t *testing.T) {
	s := registeredService(t, "native")
	var capturedBody string
	h := newCaptureHost(ssePlan(simpleAnthropicSSE()))
	h.capture = func(method, url, body string) { capturedBody = body }
	s.SetHost(h.call)
	if _, err := s.Handle("executor.execute_stream", executorRequest("client-1", "claude-sonnet-5")); err != nil {
		t.Fatalf("start claude stream: %v", err)
	}
	select {
	case <-h.clientClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("client stream was never closed")
	}
	if strings.Contains(capturedBody, "stream_options") {
		t.Fatalf("anthropic request must not carry stream_options: %s", capturedBody)
	}
	if len(h.emitted) == 0 {
		t.Fatal("no chunks emitted")
	}
	var first map[string]any
	if err := json.Unmarshal(h.emitted[0], &first); err != nil {
		t.Fatal(err)
	}
	if first["object"] != "chat.completion.chunk" {
		t.Fatalf("emitted chunk = %#v", first)
	}
	var last map[string]any
	if err := json.Unmarshal(h.emitted[len(h.emitted)-1], &last); err != nil {
		t.Fatal(err)
	}
	choice := object(list(last["choices"])[0])
	if choice["finish_reason"] != "stop" || last["usage"] == nil {
		t.Fatalf("final chunk = %#v", last)
	}
}

// captureHost 是记录出站请求的宿主桩，用于断言路由与请求体。
type captureHost struct {
	mu           sync.Mutex
	plan         hostPlan
	capture      func(method, targetURL, body string)
	streams      map[string][]readChunk
	reads        map[string]int
	emitted      [][]byte
	clientClosed chan struct{}
	closeOnce    sync.Once
}

func newCaptureHost(plan hostPlan) *captureHost {
	return &captureHost{plan: plan, streams: map[string][]readChunk{}, reads: map[string]int{}, clientClosed: make(chan struct{})}
}

func (h *captureHost) call(method string, payload, out any) error {
	request, _ := payload.(map[string]any)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case "host.http.do_stream":
		body, _ := request["body"].([]byte)
		if h.capture != nil {
			h.capture(str(request["method"]), str(request["url"]), string(body))
		}
		streamID := "capture-1"
		h.streams[streamID] = h.plan.chunks
		*out.(*upstreamStream) = upstreamStream{StatusCode: h.plan.status, Headers: h.plan.header, StreamID: streamID}
		return nil
	case "host.http.stream_read":
		chunks := h.streams["capture-1"]
		index := h.reads["capture-1"]
		h.reads["capture-1"]++
		if index >= len(chunks) {
			*out.(*readChunk) = readChunk{Done: true}
		} else {
			*out.(*readChunk) = chunks[index]
		}
		return nil
	case "host.http.stream_close":
		return nil
	case "host.stream.emit":
		chunk, _ := request["payload"].([]byte)
		h.emitted = append(h.emitted, chunk)
		return nil
	case "host.stream.close":
		h.closeOnce.Do(func() { close(h.clientClosed) })
		return nil
	default:
		return fmt.Errorf("unexpected host method %q", method)
	}
}
