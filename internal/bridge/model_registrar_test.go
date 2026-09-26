package bridge

import "testing"

// 模型规格必须由插件申报（CPA 自己的模型目录只收录内置厂商，插件模型不会被自动补全）。
// 上游目录 /provider/v1/models 实测带 name 与 context_length，刷新时原样接住并申报。
func TestModelRegistrationDeclaresSpecsAndUpstreamAlias(t *testing.T) {
	s := registeredService(t, "")
	body := jsonBytes(map[string]any{"models": []any{
		map[string]any{
			"id": "command-code/claude-sonnet-5", "upstream_id": "claude-sonnet-5",
			"name": "Claude Sonnet 5", "context_length": 1000000,
		},
	}})
	if _, err := s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/models", Body: body})); err != nil {
		t.Fatalf("保存模型映射失败：%v", err)
	}

	reg, ok := s.modelRegistration().(map[string]any)
	if !ok {
		t.Fatalf("modelRegistration 返回类型异常：%#v", s.modelRegistration())
	}
	models, ok := reg["Models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("只应注册客户端名一个条目，实际 %#v", reg["Models"])
	}
	for _, m := range models {
		if m["ContextLength"] != 1000000 {
			t.Fatalf("规格字段未申报：%#v", m)
		}
	}
	if models[0]["ID"] != "command-code/claude-sonnet-5" {
		t.Fatalf("应当是客户端模型名，实际 %#v", models[0]["ID"])
	}

	// 宿主通过 model.register 拿模型，形状必须与 model.static 一致。
	if _, err := s.Handle("model.register", nil); err != nil {
		t.Fatalf("model.register 未实现：%v", err)
	}
}
