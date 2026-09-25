package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type hostPlan struct {
	status int
	header http.Header
	chunks []readChunk
}

type fakeHost struct {
	mu             sync.Mutex
	plans          []hostPlan
	opened         []bool
	callbackIDs    []string
	streams        map[string][]readChunk
	reads          map[string]int
	upstreamClosed []string
	emitted        [][]byte
	emitFailAt     int
	clientError    string
	clientClosed   chan struct{}
	closeOnce      sync.Once
}

func newFakeHost(plans ...hostPlan) *fakeHost {
	return &fakeHost{
		plans:        plans,
		streams:      make(map[string][]readChunk),
		reads:        make(map[string]int),
		clientClosed: make(chan struct{}),
	}
}

func (h *fakeHost) call(method string, payload, out any) error {
	request, _ := payload.(map[string]any)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case "host.http.do_stream":
		if len(h.opened) >= len(h.plans) {
			return fmt.Errorf("unexpected upstream request %d", len(h.opened)+1)
		}
		body, ok := request["body"].([]byte)
		if !ok {
			return errors.New("upstream body was not bytes")
		}
		var parsed struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return err
		}
		h.opened = append(h.opened, parsed.Stream)
		h.callbackIDs = append(h.callbackIDs, str(request["host_callback_id"]))
		plan := h.plans[len(h.opened)-1]
		streamID := fmt.Sprintf("upstream-%d", len(h.opened))
		h.streams[streamID] = plan.chunks
		*out.(*upstreamStream) = upstreamStream{StatusCode: plan.status, Headers: plan.header, StreamID: streamID}
		return nil
	case "host.http.stream_read":
		streamID := str(request["stream_id"])
		chunks, ok := h.streams[streamID]
		if !ok {
			return fmt.Errorf("unknown upstream stream %q", streamID)
		}
		index := h.reads[streamID]
		h.reads[streamID]++
		if index >= len(chunks) {
			*out.(*readChunk) = readChunk{Done: true}
		} else {
			*out.(*readChunk) = chunks[index]
		}
		return nil
	case "host.http.stream_close":
		h.upstreamClosed = append(h.upstreamClosed, str(request["stream_id"]))
		return nil
	case "host.stream.emit":
		chunk, ok := request["payload"].([]byte)
		if !ok {
			return errors.New("emitted payload was not bytes")
		}
		if h.emitFailAt > 0 && len(h.emitted)+1 >= h.emitFailAt {
			return errors.New("client connection closed")
		}
		h.emitted = append(h.emitted, bytes.Clone(chunk))
		return nil
	case "host.stream.close":
		h.clientError = str(request["error"])
		h.closeOnce.Do(func() { close(h.clientClosed) })
		return nil
	default:
		return fmt.Errorf("unexpected host method %q", method)
	}
}

func registeredService(t *testing.T, mode string) *Service {
	t.Helper()
	s := NewService()
	configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(t.TempDir()))
	if mode != "" {
		configYAML += fmt.Sprintf("nonstream_mode: %s\n", mode)
	}
	_, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)}))
	if err != nil {
		t.Fatalf("register plugin: %v", err)
	}
	return s
}

func executorRequest(streamID, model string) json.RawMessage {
	return jsonBytes(ExecutorRequest{
		Model:          model,
		Payload:        []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		StorageJSON:    jsonBytes(Credential{Type: Provider, ID: "credential-1", Label: "test credential", APIKey: "test-key"}),
		HostCallbackID: "callback-1",
		StreamID:       streamID,
	})
}

func jsonPlan(body any) hostPlan {
	return hostPlan{status: 200, header: http.Header{"Content-Type": []string{"application/json"}}, chunks: []readChunk{{Payload: jsonBytes(body), Done: true}}}
}

func sseFrame(v any) []byte {
	return append(append([]byte("data: "), jsonBytes(v)...), []byte("\r\n\r\n")...)
}

func ssePlan(parts ...[]byte) hostPlan {
	chunks := make([]readChunk, 0, len(parts))
	for _, part := range parts {
		chunks = append(chunks, readChunk{Payload: part})
	}
	return hostPlan{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, chunks: chunks}
}

func simpleSSE() []byte {
	var stream []byte
	stream = append(stream, sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}})...)
	stream = append(stream, sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 8, "completion_tokens": 2}})...)
	stream = append(stream, []byte("data: [DONE]\r\n\r\n")...)
	return stream
}

// anthropicEvent 构造一条 Anthropic SSE 事件帧。
func anthropicEvent(v any) []byte {
	return append(append([]byte("data: "), jsonBytes(v)...), []byte("\n\n")...)
}

// simpleAnthropicSSE 复刻 /messages 流式响应：文本增量 + tool_use + 结束。
func simpleAnthropicSSE() []byte {
	var stream []byte
	stream = append(stream, anthropicEvent(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "usage": map[string]any{"input_tokens": 9, "output_tokens": 1}}})...)
	stream = append(stream, anthropicEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})...)
	stream = append(stream, anthropicEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "hi"}})...)
	stream = append(stream, anthropicEvent(map[string]any{"type": "content_block_stop", "index": 0})...)
	stream = append(stream, anthropicEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 2}})...)
	stream = append(stream, anthropicEvent(map[string]any{"type": "message_stop"})...)
	return stream
}

func TestModelNamesArePreservedAndRegistered(t *testing.T) {
	for _, upstream := range []string{"deepseek/deepseek-v4-flash", "claude-sonnet-5", " custom-name "} {
		cfg := defaultConfig()
		cfg.Models = []Model{{ID: " my-alias ", UpstreamID: upstream}}
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Models[0].ID != " my-alias " || cfg.Models[0].UpstreamID != upstream {
			t.Fatalf("validation rewrote user model: %#v", cfg.Models[0])
		}
	}
	s := registeredService(t, "native-fallback")
	got, err := s.resolveModel("deepseek-v4-flash")
	if err != nil || got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("configured alias resolved to %q, %v", got, err)
	}
	models, err := s.Handle("model.static", nil)
	if err != nil || len(models.(map[string]any)["Models"].([]map[string]any)) == 0 {
		t.Fatalf("registered models missing: %v, %v", models, err)
	}
}

func TestLogSummaryUsesFilteredRecordsBeforePagination(t *testing.T) {
	s := registeredService(t, "native-fallback")
	s.logs = []LogEntry{
		{Model: "keep", Status: 200, PromptTokens: 100, CompletionTokens: 20, CachedTokens: 60},
		{Model: "keep", Status: 200, PromptTokens: 200, CompletionTokens: 30, CachedTokens: 90},
		{Model: "other", Status: 200, PromptTokens: 1000},
	}
	result, err := s.logsResponse(ManagementRequest{Query: url.Values{"search": {"keep"}, "limit": {"1"}}})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Items   []LogEntry `json:"items"`
		Summary struct {
			Requests   int      `json:"requests"`
			Prompt     int64    `json:"prompt_tokens"`
			Completion int64    `json:"completion_tokens"`
			Cached     int64    `json:"cached_tokens"`
			Rate       *float64 `json:"cache_rate"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(result.(ManagementResponse).Body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Summary.Requests != 2 || response.Summary.Prompt != 300 || response.Summary.Completion != 50 || response.Summary.Cached != 150 || response.Summary.Rate == nil || *response.Summary.Rate != .5 {
		t.Fatalf("incorrect filtered summary: %s", result.(ManagementResponse).Body)
	}
}

// Command Code 的成功响应是标准 OpenAI 形状（无 {success,data} 信封）。
func TestDirectNonstreamResponsePreservesToolsReasoningAndUsage(t *testing.T) {
	s := registeredService(t, "native")
	data := map[string]any{
		"choices":  []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "done", "reasoning_content": "thinking", "tool_calls": []any{map[string]any{"id": "call-1", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}}},
		"usage":    map[string]any{"prompt_tokens": 20, "completion_tokens": 4, "prompt_tokens_details": map[string]any{"cached_tokens": 12}, "completion_tokens_details": map[string]any{"reasoning_tokens": 3}},
		"provider": "deepseek",
	}
	h := newFakeHost(jsonPlan(data))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest("", "deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("execute direct response: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.(Response).Payload, &body); err != nil {
		t.Fatal(err)
	}
	message := object(object(list(body["choices"])[0])["message"])
	usage := object(body["usage"])
	if message["content"] != "done" || message["reasoning_content"] != "thinking" || len(list(message["tool_calls"])) != 1 {
		t.Fatalf("response body lost output fields: %#v", message)
	}
	if number(object(usage["prompt_tokens_details"])["cached_tokens"]) != 12 || number(object(usage["completion_tokens_details"])["reasoning_tokens"]) != 3 {
		t.Fatalf("response body lost usage details: %#v", usage)
	}
	if body["model"] != "deepseek-v4-flash" {
		t.Fatalf("response model = %#v", body["model"])
	}
	if len(s.logs) != 1 || s.logs[0].CachedTokens != 12 || s.logs[0].ReasoningTokens != 3 || s.logs[0].Provider != "deepseek" {
		t.Fatalf("request log lost actual usage/provider: %#v", s.logs)
	}
}

// 上游错误信封 {success:false, error:{...}} 必须翻译为失败。
func TestUpstreamErrorEnvelopeFails(t *testing.T) {
	s := registeredService(t, "native")
	h := newFakeHost(hostPlan{status: 401, header: http.Header{"Content-Type": []string{"application/json"}}, chunks: []readChunk{{Payload: []byte(`{"success":false,"error":{"code":"UNAUTHORIZED","status":401,"message":"Invalid API key"}}`), Done: true}}})
	s.SetHost(h.call)
	_, err := s.Handle("executor.execute", executorRequest("", "deepseek-v4-flash"))
	if err == nil || !strings.Contains(err.Error(), "Invalid API key") || statusOf(err) != 401 {
		t.Fatalf("error envelope = %v (status %d)", err, statusOf(err))
	}
}

func TestSSEDecoderByteSplitsCRLFAndIncompleteFrame(t *testing.T) {
	input := []byte("event: message\r\ndata: first\r\ndata: second\r\n\r\ndata: [DONE]\r\n\r\n")
	decoder := SSEDecoder{max: 1024}
	var events []string
	for _, b := range input {
		if err := decoder.Feed([]byte{b}, func(data []byte, event string) error {
			events = append(events, event+":"+string(data))
			return nil
		}); err != nil {
			t.Fatalf("feed split SSE: %v", err)
		}
	}
	if err := decoder.End(); err != nil {
		t.Fatalf("complete stream rejected: %v", err)
	}
	if len(events) != 2 || events[0] != "message:first\nsecond" || events[1] != ":[DONE]" {
		t.Fatalf("decoded SSE events = %#v", events)
	}
	broken := SSEDecoder{max: 1024}
	_ = broken.Feed([]byte("data: unfinished"), func([]byte, string) error { return nil })
	if err := broken.End(); err == nil {
		t.Error("incomplete SSE frame was accepted")
	}
}

func TestStreamAggregationMergesToolFragmentsAndUsage(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	first := sseFrame(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call-1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": "{\"x\":"},
			}}},
		}},
	})
	second := sseFrame(map[string]any{
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "tool_calls",
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": "1}"},
			}}},
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "prompt_tokens_details": map[string]any{"cached_tokens": 7}, "completion_tokens_details": map[string]any{"reasoning_tokens": 2}},
	})
	h := newFakeHost(ssePlan(first[:len(first)/2], append(append(bytes.Clone(first[len(first)/2:]), second...), []byte("data: [DONE]\r\n\r\n")...)))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest("", "deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("aggregate stream: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.(Response).Payload, &body); err != nil {
		t.Fatal(err)
	}
	tools := list(object(object(list(body["choices"])[0])["message"])["tool_calls"])
	function := object(object(tools[0])["function"])
	if function["name"] != "lookup" || function["arguments"] != `{"x":1}` {
		t.Fatalf("tool fragments were not merged: %#v", tools)
	}
	usage := object(body["usage"])
	if number(object(usage["prompt_tokens_details"])["cached_tokens"]) != 7 || number(object(usage["completion_tokens_details"])["reasoning_tokens"]) != 2 {
		t.Fatalf("stream usage details lost: %#v", usage)
	}
	if len(h.opened) != 1 || !h.opened[0] {
		t.Fatalf("aggregate mode sent upstream stream flags %#v", h.opened)
	}
}

func TestStreamErrorsAreReported(t *testing.T) {
	for _, test := range []struct {
		name   string
		plan   hostPlan
		needle string
	}{
		{"missing DONE", ssePlan(sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}, "finish_reason": "stop"}}})), "收到 [DONE] 之前"},
		{"event error", ssePlan([]byte("event: error\r\ndata: {\"error\":{\"message\":\"quota exhausted\"}}\r\n\r\n")), "quota exhausted"},
		{"transport interruption", hostPlan{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, chunks: []readChunk{{Payload: sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}})}, {Error: "connection reset", Done: true}}}, "connection reset"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			h := newFakeHost(test.plan)
			s.SetHost(h.call)
			_, err := s.Handle("executor.execute", executorRequest("", "deepseek-v4-flash"))
			if err == nil || !strings.Contains(err.Error(), test.needle) || statusOf(err) != 502 {
				t.Fatalf("stream error = %v (status %d), want %q/502", err, statusOf(err), test.needle)
			}
			if len(s.logs) != 1 || s.logs[0].Status != 502 {
				t.Fatalf("failed stream log = %#v", s.logs)
			}
		})
	}
}

func TestActualProviderRequiresResponseEvidence(t *testing.T) {
	entry := LogEntry{Provider: "unknown", ProviderSource: "not_reported"}
	attempt := Attempt{Provider: "unknown", ProviderSource: "not_reported"}
	observeMetadata(map[string]any{"provider": "deepseek"}, &entry, &attempt)
	if entry.Provider != "deepseek" || attempt.Provider != "deepseek" {
		t.Fatalf("actual provider = %q / %q", entry.Provider, attempt.Provider)
	}
	entry = LogEntry{Provider: "unknown", ProviderSource: "not_reported"}
	attempt = Attempt{Provider: "unknown", ProviderSource: "not_reported"}
	observeMetadata(map[string]any{"candidates": []any{"openai"}}, &entry, &attempt)
	if entry.Provider != "unknown" {
		t.Fatalf("candidate list leaked into provider: %#v", entry)
	}
}

func TestExecuteStreamEmitsChunks(t *testing.T) {
	s := registeredService(t, "native")
	h := newFakeHost(ssePlan(simpleSSE()))
	s.SetHost(h.call)
	if _, err := s.Handle("executor.execute_stream", executorRequest("client-1", "deepseek-v4-flash")); err != nil {
		t.Fatalf("start stream: %v", err)
	}
	select {
	case <-h.clientClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("client stream was never closed")
	}
	if len(h.emitted) == 0 {
		t.Fatal("no chunks were emitted")
	}
	var first map[string]any
	if err := json.Unmarshal(h.emitted[0], &first); err != nil {
		t.Fatal(err)
	}
	if first["model"] != "deepseek-v4-flash" {
		t.Fatalf("emitted chunk model = %#v", first["model"])
	}
	if len(s.logs) != 1 || s.logs[0].Status != 200 || !s.logs[0].Stream {
		t.Fatalf("stream log = %#v", s.logs)
	}
}

func TestShutdownWaitsForActiveStreams(t *testing.T) {
	s := registeredService(t, "native")
	slow := make(chan struct{})
	h := newFakeHost(hostPlan{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, chunks: []readChunk{
		{Payload: sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "part"}}}})},
	}})
	s.SetHost(func(method string, payload, out any) error {
		if method == "host.http.stream_read" {
			<-slow
		}
		return h.call(method, payload, out)
	})
	if _, err := s.Handle("executor.execute_stream", executorRequest("client-1", "deepseek-v4-flash")); err != nil {
		t.Fatalf("start stream: %v", err)
	}
	done := make(chan struct{})
	go func() { s.Handle("plugin.shutdown", nil); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown did not wait for active stream")
	case <-time.After(100 * time.Millisecond):
	}
	close(slow)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown never finished")
	}
}

func TestLoginIsUnsupportedWithGuidance(t *testing.T) {
	s := registeredService(t, "")
	response, err := s.Handle("auth.login.start", jsonBytes(map[string]any{}))
	if err != nil {
		t.Fatalf("login.start: %v", err)
	}
	message := str(response.(map[string]any)["Message"])
	if !strings.Contains(message, "API Key") {
		t.Fatalf("login message = %q", message)
	}
	if _, err := s.Handle("auth.login.poll", jsonBytes(map[string]any{})); err != nil {
		t.Fatalf("login.poll: %v", err)
	}
}

func TestRefreshAuthReturnsStaticTimeForAPIKey(t *testing.T) {
	s := registeredService(t, "")
	credential := Credential{Type: Provider, ID: "cred-1", Label: "test", APIKey: "test-key"}
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{"Provider": Provider, "FileName": "cred-1.json", "RawJSON": jsonBytes(credential)})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	response, err := s.Handle("auth.refresh", jsonBytes(map[string]any{"StorageJSON": jsonBytes(credential), "AuthID": "cred-1"}))
	if err != nil {
		t.Fatalf("refresh auth: %v", err)
	}
	auth := response.(map[string]any)["Auth"].(map[string]any)
	if str(object(auth["Metadata"])["type"]) != Provider {
		t.Fatalf("auth metadata type = %#v", auth["Metadata"])
	}
	next, ok := response.(map[string]any)["NextRefreshAfter"].(time.Time)
	if !ok || time.Until(next) < 364*24*time.Hour {
		t.Fatalf("next refresh = %#v", response.(map[string]any)["NextRefreshAfter"])
	}
}

func TestParseAuthRejectsForeignCredentials(t *testing.T) {
	s := registeredService(t, "")
	response, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": "other-provider", "FileName": "x.json", "RawJSON": []byte(`{"type":"other-provider","api_key":"k"}`),
	}))
	if err != nil {
		t.Fatalf("parse foreign credential: %v", err)
	}
	if handled, _ := response.(map[string]any)["Handled"].(bool); handled {
		t.Fatalf("foreign credential was handled: %#v", response)
	}
}
