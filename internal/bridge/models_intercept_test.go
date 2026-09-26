package bridge

import (
	"encoding/json"
	"testing"
)

// CPA 的 /v1/models 只输出 id/object/created/owned_by（宿主写死的过滤），
// 插件通过 response.intercept_after 把 context_length 补回来 —— 客户端经 CPA 也能看到参数。
func TestModelsInterceptAddsContextLength(t *testing.T) {
	s := registeredService(t, "")
	body := jsonBytes(map[string]any{"models": []any{
		map[string]any{"id": "command-code/claude-sonnet-5", "upstream_id": "claude-sonnet-5", "context_length": 1000000},
	}})
	if _, err := s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/models", Body: body})); err != nil {
		t.Fatalf("准备映射失败：%v", err)
	}
	// 目录缓存：覆盖"名字对得上、但不是本插件注册"的模型
	s.mu.Lock()
	s.modelSpecs = map[string]int{"claude-sonnet-5": 1000000}
	s.mu.Unlock()

	list := jsonBytes(map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "command-code/claude-sonnet-5", "object": "model", "owned_by": "command-code"},
		map[string]any{"id": "some-vendor/claude-sonnet-5", "object": "model", "owned_by": "other"},
		map[string]any{"id": "unrelated", "object": "model", "owned_by": "other"},
	}})
	result, err := s.Handle("response.intercept_after", jsonBytes(map[string]any{"Body": list, "StatusCode": 200}))
	if err != nil {
		t.Fatalf("拦截失败：%v", err)
	}
	out, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("返回类型异常：%#v", result)
	}
	changed, ok := out["Body"].([]byte)
	if !ok || len(changed) == 0 {
		t.Fatalf("应当改写响应体：%#v", out)
	}
	var parsed struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(changed, &parsed); err != nil {
		t.Fatalf("改写后不是合法 JSON：%v", err)
	}
	if parsed.Object != "list" || len(parsed.Data) != 3 {
		t.Fatalf("响应结构被破坏：%#v", parsed)
	}
	if got := parsed.Data[0]["context_length"]; got != float64(1000000) {
		t.Fatalf("自有模型没补上：%#v", parsed.Data[0])
	}
	if got := parsed.Data[1]["context_length"]; got != float64(1000000) {
		t.Fatalf("目录里对得上的模型没补上：%#v", parsed.Data[1])
	}
	if _, has := parsed.Data[2]["context_length"]; has {
		t.Fatalf("目录里没有的模型不该乱补：%#v", parsed.Data[2])
	}

	// 对话响应（不是模型列表）必须原样返回
	chat := jsonBytes(map[string]any{"id": "x", "object": "chat.completion", "choices": []any{}})
	result, err = s.Handle("response.intercept_after", jsonBytes(map[string]any{"Body": chat, "StatusCode": 200}))
	if err != nil {
		t.Fatalf("拦截失败：%v", err)
	}
	if out, ok := result.(map[string]any); !ok || len(out) != 0 {
		t.Fatalf("非模型列表响应不该被改写：%#v", result)
	}
}
