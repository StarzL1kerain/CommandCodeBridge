package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Command Code 的鉴权只有一种形态：长期 API Key（Authorization: Bearer <key>）。
// 以下事实于 2026-09 通过官方文档、CLI 1.65.2 逆向与无鉴权实测确认：
//
//   - API Key 在 Studio（https://commandcode.ai/studio）创建，CLI 与 Provider API 共用同一把 key。
//   - CLI 的浏览器登录（/studio/auth/cli + 本地 loopback 回调）依赖在宿主进程内监听端口，
//     插件环境做不到，因此本插件只支持在管理面板手动粘贴 API Key。
//   - GET /alpha/whoami 可校验 key 并返回用户与组织信息（401 信封：{success:false, error:{...}}）。
//   - key 无过期时间，auth.refresh 永远返回静态时间。
const (
	// whoamiCallTimeout 是凭据校验请求的时限。
	whoamiCallTimeout = 20 * time.Second
)

// whoamiResponse 是 GET /alpha/whoami 的响应（CLI 的 projectUsageView 直接消费该形状）。
type whoamiResponse struct {
	User struct {
		ID       string `json:"id"`
		UserName string `json:"userName"`
		Name     string `json:"name"`
		Email    string `json:"email"`
	} `json:"user"`
	Org struct {
		ID    string `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	} `json:"org"`
}

// displayName 返回凭据备注的默认值：优先用户名，其次姓名/邮箱。
func (w whoamiResponse) displayName() string {
	for _, candidate := range []string{w.User.UserName, w.User.Name, w.User.Email, w.Org.Login} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return "Command Code"
}

func statusOr(status int, fallback int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return fallback
}

// hostRequest 通过宿主的流式 HTTP 回调完成一次普通请求。
// 这里刻意复用 host.http.do_stream 而不是 host.http.do：本插件的目录请求
// 已经走通这条路径，避免依赖尚未实证的 host.http.do 响应结构。
func (s *Service) hostRequest(callbackID, method, rawURL string, headers http.Header, body []byte) (int, []byte, error) {
	payload := map[string]any{"method": method, "url": rawURL, "headers": headers}
	if len(body) > 0 {
		payload["body"] = body
	}
	if callbackID != "" {
		payload["host_callback_id"] = callbackID
	}
	up, err := s.openUpstream(payload, time.Now().Add(whoamiCallTimeout))
	if err != nil {
		return 0, nil, err
	}
	var buf bytes.Buffer
	if err := s.read(up, func(b []byte) error { buf.Write(b); return nil }); err != nil {
		return up.StatusCode, buf.Bytes(), err
	}
	return up.StatusCode, buf.Bytes(), nil
}

// verifyAPIKey 用 /alpha/whoami 校验一把 key，成功时顺便返回用户信息。
func (s *Service) verifyAPIKey(callbackID, key string) (whoamiResponse, error) {
	if err := s.begin(); err != nil {
		return whoamiResponse{}, err
	}
	defer s.active.Done()
	headers := http.Header{
		"Accept":        []string{"application/json"},
		"Authorization": []string{"Bearer " + strings.TrimSpace(key)},
	}
	status, body, err := s.hostRequest(callbackID, http.MethodGet, s.config().alphaBase()+"/alpha/whoami?limits=1", headers, nil)
	if err != nil {
		return whoamiResponse{}, fail(502, "无法连接 Command Code 服务："+safeError(err))
	}
	var who whoamiResponse
	if e := json.Unmarshal(body, &who); e != nil {
		return whoamiResponse{}, fail(502, "Command Code 返回了无效 JSON")
	}
	if status < 200 || status >= 300 {
		j, _ := decodeObject(body)
		return whoamiResponse{}, fail(statusOr(status, 401), "API Key 校验失败："+errorMessage(j))
	}
	if strings.TrimSpace(who.User.ID) == "" && strings.TrimSpace(who.Org.ID) == "" {
		return whoamiResponse{}, fail(502, "Command Code 未返回用户信息")
	}
	return who, nil
}

// loginUnsupported 是对 auth.login.start / auth.login.poll 的统一回复：
// Command Code 的官方浏览器登录依赖本地回调端口，插件进程内无法提供，
// 引导用户改用管理面板粘贴 API Key。
func loginUnsupported() map[string]any {
	return map[string]any{
		"Status":  "error",
		"Message": "Command Code 暂不支持浏览器登录：请在插件管理控制台的「凭据」中粘贴 Studio 生成的 API Key（https://commandcode.ai/studio）。",
	}
}
