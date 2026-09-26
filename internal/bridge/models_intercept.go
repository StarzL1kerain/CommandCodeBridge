package bridge

import (
	"encoding/json"
	"strings"
)

// interceptModelsResponse 处理 response.intercept_after：给 CPA 的 /v1/models 响应补上
// `context_length`。
//
// 为什么需要：宿主的 OpenAIModels 处理器（sdk/api/handlers/openai/openai_handlers.go）写死了
// "只保留 4 个必需字段"（id/object/created/owned_by），规格在出口被丢掉 —— 客户端经 CPA 就只看得到
// 自己的默认值。宿主会把模型列表响应体交给插件拦截器（宿主自带的
// TestModelsEndpoint_ExposesResponseToPluginInterceptors_OpenAI 就是验证这条通路的）。
//
// 只处理 {"object":"list","data":[...]} 形状；其它响应（尤其是对话响应）原样返回。
func (s *Service) interceptModelsResponse(raw json.RawMessage) (any, error) {
	var in struct {
		Body []byte `json:"Body"`
	}
	if e := json.Unmarshal(raw, &in); e != nil || len(in.Body) == 0 {
		return modelsInterceptResult(nil), nil
	}
	var list struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if json.Unmarshal(in.Body, &list) != nil || list.Object != "list" || len(list.Data) == 0 {
		return modelsInterceptResult(nil), nil
	}
	// 本插件注册过的模型（客户端名与上游名都算）
	known := map[string]Model{}
	for _, m := range s.config().Models {
		if id := strings.TrimSpace(m.ID); id != "" {
			known[id] = m
		}
		if upstream := strings.TrimSpace(m.UpstreamID); upstream != "" {
			known[upstream] = m
		}
	}
	// 目录缓存：覆盖"名字对得上、但不是本插件注册"的模型（例如同一个模型走 CPA 内置
	// OpenAI 兼容路由接入，那条路不会带参数）。缓存由刷新模型时填充。
	s.mu.RLock()
	specs := s.modelSpecs
	s.mu.RUnlock()

	changed := false
	for _, entry := range list.Data {
		id, _ := entry["id"].(string)
		if m, ok := known[id]; ok {
			if m.ContextLength > 0 && entry["context_length"] == nil {
				entry["context_length"] = m.ContextLength
				changed = true
			}
			continue
		}
		if value, hit := specs[tailName(id)]; hit && entry["context_length"] == nil {
			entry["context_length"] = value
			changed = true
		}
	}
	if !changed {
		return modelsInterceptResult(nil), nil
	}
	body, e := json.Marshal(list)
	if e != nil {
		return modelsInterceptResult(nil), nil
	}
	return modelsInterceptResult(body), nil
}

// modelsInterceptResult 组装拦截响应：body 为 nil 表示"不改动原响应"。
func modelsInterceptResult(body []byte) map[string]any {
	if len(body) == 0 {
		return map[string]any{}
	}
	return map[string]any{"Body": body}
}

// tailName 取模型 id 的最后一段：some-vendor/deepseek-v4-pro → deepseek-v4-pro。
// 上游目录与 CPA 里的模型命名不一定一致，只能靠这一段对齐。
func tailName(id string) string {
	trimmed := strings.TrimSpace(id)
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}
