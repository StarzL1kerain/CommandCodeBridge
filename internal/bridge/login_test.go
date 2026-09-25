package bridge

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// decodeManagement 解开 managementJSON 的响应体。
func decodeManagement(t *testing.T, result any) map[string]any {
	t.Helper()
	response, ok := result.(ManagementResponse)
	if !ok {
		t.Fatalf("management response type = %T", result)
	}
	var body map[string]any
	if e := json.Unmarshal(response.Body, &body); e != nil {
		t.Fatalf("decode body: %v", e)
	}
	return body
}

func TestLoginParamsAcceptsEveryPasteShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"完整 URL", "http://127.0.0.1:41017/callback?apiKey=cmd-key&state=abc&userName=alice"},
		{"带前导问号的查询串", "?apiKey=cmd-key&state=abc&userName=alice"},
		{"裸表单数据", "apiKey=cmd-key&state=abc&userId=u1&userName=alice&keyName=cli"},
		{"路径形式的查询串", "callback?apiKey=cmd-key&state=abc"},
		{"参数在 fragment 里", "http://127.0.0.1:41017/callback#apiKey=cmd-key&state=abc"},
	} {
		values := loginParams(tc.raw)
		if values.Get("apiKey") != "cmd-key" || values.Get("state") != "abc" {
			t.Fatalf("%s: %q -> %#v", tc.name, tc.raw, values)
		}
	}
	for _, raw := range []string{"", "   ", "http://127.0.0.1:41017/callback"} {
		if values := loginParams(raw); values.Get("apiKey") != "" {
			t.Fatalf("%q should yield no apiKey, got %#v", raw, values)
		}
	}
}

func whoamiPlanForLogin() hostPlan {
	return jsonStatusPlan(200, map[string]any{
		"user": map[string]any{"id": "user-1", "userName": "alice", "name": "Alice", "email": "alice@example.com"},
		"org":  map[string]any{"id": "org-1", "login": "acme", "name": "Acme"},
	})
}

func TestBrowserLoginSessionAndComplete(t *testing.T) {
	s := registeredService(t, "")
	h := newRecordingHost(whoamiPlanForLogin())
	s.SetHost(h.call)

	result, err := s.startLoginSession(ManagementRequest{Body: jsonBytes(map[string]any{"label": "我的账号"})})
	if err != nil {
		t.Fatal(err)
	}
	body := decodeManagement(t, result)
	authURL, _ := body["url"].(string)
	state, _ := body["state"].(string)
	if state == "" || !strings.HasPrefix(authURL, "https://commandcode.ai/studio/auth/cli?") {
		t.Fatalf("session = %#v", body)
	}
	// 上游只接受 localhost 回调，插件永远把 callback 固定到本地回环地址。
	values, e := url.ParseQuery(strings.TrimPrefix(authURL, "https://commandcode.ai/studio/auth/cli?"))
	if e != nil {
		t.Fatal(e)
	}
	if values.Get("mode") != "redirect" || values.Get("state") != state {
		t.Fatalf("auth url params = %#v", values)
	}
	if callback := values.Get("callback"); callback != manualCallback {
		t.Fatalf("callback = %q", callback)
	}

	// 授权后浏览器停在打不开的本机地址，用户把地址栏里的完整 URL 粘贴回来。
	callbackURL := manualCallback + "?apiKey=cmd-test-key-123&state=" + url.QueryEscape(state) + "&userId=user-1&userName=alice&keyName=cli"
	result, err = s.completeLogin(ManagementRequest{Body: jsonBytes(map[string]any{"url": callbackURL})})
	if err != nil {
		t.Fatal(err)
	}
	body = decodeManagement(t, result)
	if body["label"] != "我的账号" {
		t.Fatalf("complete = %#v", body)
	}
	if len(h.saved) != 1 || h.saved[0].APIKey != "cmd-test-key-123" || h.saved[0].Type != Provider {
		t.Fatalf("saved credentials = %#v", h.saved)
	}
	if calls := h.snapshot(); len(calls) != 1 || calls[0].url != "https://api.commandcode.ai/alpha/whoami?limits=1" {
		t.Fatalf("whoami calls = %#v", calls)
	}

	// state 只能用一次：重放必须被拒。
	result, err = s.completeLogin(ManagementRequest{Body: jsonBytes(map[string]any{"url": callbackURL})})
	if err != nil {
		t.Fatal(err)
	}
	if body := decodeManagement(t, result); body["error"] == nil {
		t.Fatalf("replay should fail: %#v", body)
	}
}

// 凭据 ID 由账号派生：与内置供应商一样，凭据名能直接看出是哪个账号。
func TestCredentialIDIsAccountDerivedAndFileSafe(t *testing.T) {
	if got := credentialID("user-1", "goat-user"); got != PluginID+"-goat-user" {
		t.Fatalf("credential id = %q", got)
	}
	for _, tc := range []struct{ userID, userName string }{
		{"7c9e6679-7425-40de-944b-e07fc1f90ae7", ""},
		{"user-1", "a/b\\c:d*e?f"},
		{"user-1", "   "},
		{"user-1", "../../etc/passwd"},
	} {
		id := credentialID(tc.userID, tc.userName)
		if id == "" || strings.ContainsAny(id, `/\:*?"<>|`) || strings.Contains(id, "..") || filepath.Base(id) != id {
			t.Fatalf("unsafe credential id %q (from %q / %q)", id, tc.userID, tc.userName)
		}
	}
}

// 同一账号重复导入必须覆盖同一份凭据，而不是每操作一次就多出一条。
func TestRepeatedImportOverwritesSameCredential(t *testing.T) {
	s := registeredService(t, "")
	h := newRecordingHost(whoamiPlan(), whoamiPlan())
	s.SetHost(h.call)
	for i := 0; i < 2; i++ {
		response, err := s.management(jsonBytes(ManagementRequest{Method: "POST", Path: apiBase + "/credentials", Body: []byte(`{"api_key":"cmd-live-key"}`)}))
		if err != nil {
			t.Fatal(err)
		}
		if status := response.(ManagementResponse).StatusCode; status != 201 {
			t.Fatalf("import %d status = %d", i, status)
		}
	}
	if len(h.saved) != 2 {
		t.Fatalf("saved = %#v", h.saved)
	}
	if h.saved[0].ID != PluginID+"-goat-user" || h.saved[1].ID != h.saved[0].ID {
		t.Fatalf("ids = %q / %q", h.saved[0].ID, h.saved[1].ID)
	}
	if h.saved[0].AccountID != "user-1" || h.saved[0].Label != "goat-user" {
		t.Fatalf("credential = %#v", h.saved[0])
	}
	if got := len(s.credentials()); got != 1 {
		t.Fatalf("credential count = %d, want 1", got)
	}
}

// TestHostDrivenLoginFlow 覆盖宿主 auth.login.start → 粘贴回调 URL → 宿主 auth.login.poll 的完整链路。
func TestHostDrivenLoginFlow(t *testing.T) {
	s := registeredService(t, "")
	h := newRecordingHost(whoamiPlanForLogin())
	s.SetHost(h.call)

	started, err := s.Handle("auth.login.start", jsonBytes(map[string]any{"Provider": Provider}))
	if err != nil {
		t.Fatal(err)
	}
	state := str(object(started)["State"])
	values, e := url.ParseQuery(strings.TrimPrefix(str(object(started)["URL"]), "https://commandcode.ai/studio/auth/cli?"))
	if e != nil {
		t.Fatal(e)
	}
	if callback := values.Get("callback"); callback != manualCallback {
		t.Fatalf("callback = %q", callback)
	}

	polled, err := s.Handle("auth.login.poll", jsonBytes(map[string]any{"Provider": Provider, "State": state}))
	if err != nil {
		t.Fatal(err)
	}
	if object(polled)["Status"] != "pending" {
		t.Fatalf("poll before callback = %#v", polled)
	}

	// 粘贴的是表单数据（授权页以 POST 表单提交时用户能拿到的就是这段）。
	callbackURL := "apiKey=cmd-test-key-789&state=" + url.QueryEscape(state) + "&userId=7c9e6679&userName=alice&keyName=cli-2026-09-25"
	completed, err := s.Handle("management.handle", jsonBytes(map[string]any{
		"Method": "POST", "Path": apiBase + "/login/complete",
		"Body": jsonBytes(map[string]any{"url": callbackURL}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if body := decodeManagement(t, completed); body["error"] != nil {
		t.Fatalf("complete = %#v", body)
	}
	// 宿主发起的登录由宿主落盘，插件此时不能自己写凭据。
	if len(h.saved) != 0 {
		t.Fatalf("host-driven login must not save a credential itself: %#v", h.saved)
	}

	polled, err = s.Handle("auth.login.poll", jsonBytes(map[string]any{"Provider": Provider, "State": state}))
	if err != nil {
		t.Fatal(err)
	}
	body := object(polled)
	if body["Status"] != "success" {
		t.Fatalf("poll after callback = %#v", body)
	}
	auths := list(body["Auths"])
	if len(auths) != 1 {
		t.Fatalf("auths = %#v", body["Auths"])
	}
	auth := object(auths[0])
	if str(auth["Provider"]) != Provider || str(auth["Label"]) != "alice" {
		t.Fatalf("auth = %#v", auth)
	}
	if str(object(auth["Metadata"])["type"]) != Provider {
		t.Fatalf("auth metadata = %#v", auth["Metadata"])
	}

	// 凭据只能交付一次。
	polled, err = s.Handle("auth.login.poll", jsonBytes(map[string]any{"Provider": Provider, "State": state}))
	if err != nil {
		t.Fatal(err)
	}
	if object(polled)["Status"] != "error" {
		t.Fatalf("replayed poll = %#v", polled)
	}
}
