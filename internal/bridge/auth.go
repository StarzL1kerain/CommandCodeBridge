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
//     插件环境做不到；且授权页只接受 localhost 回调，因此登录改为「回环回调 + 用户粘贴
//     回调 URL」（见 login.go）。
//   - GET /alpha/whoami 可校验 key 并返回用户与组织信息（401 信封：{success:false, error:{...}}）。
//   - key 无过期时间，auth.refresh 永远返回静态时间。
const (
	// alphaCallTimeout 是单次 /alpha 请求的时限。
	// 上游链路会偶发抖动（实测同账号同 key：credits 有时 1s，有时 47s 才出首字节，
	// 也会直接连接重置），所以单次预算保持短，靠重试而不是干等来覆盖抖动。
	alphaCallTimeout = 10 * time.Second
	// alphaCallAttempts 是单次逻辑请求的最大尝试次数（含首次）。
	alphaCallAttempts = 3
	// alphaRetryBackoff 是两次尝试之间的基础间隔，按次数线性增长。
	alphaRetryBackoff = 400 * time.Millisecond
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

// retryableStatus 判断上游状态码是否值得重试：链路错误、5xx 与限流/超时。
// 其余 4xx 是确定性结果（例如 key 无效），重试没有意义。
func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// waitBeforeRetry 等待退避间隔；插件开始关闭时立即返回 false。
func (s *Service) waitBeforeRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.stopCh:
		return false
	}
}

// hostRequest 通过宿主的流式 HTTP 回调完成一次普通请求，抖动时自动重试。
// 这里刻意复用 host.http.do_stream 而不是 host.http.do：本插件的目录请求
// 已经走通这条路径，避免依赖尚未实证的 host.http.do 响应结构。
func (s *Service) hostRequest(callbackID, method, rawURL string, headers http.Header, body []byte) (int, []byte, error) {
	var status int
	var payload []byte
	var err error
	for attempt := 1; attempt <= alphaCallAttempts; attempt++ {
		status, payload, err = s.hostRequestOnce(callbackID, method, rawURL, headers, body)
		if err == nil && !retryableStatus(status) {
			return status, payload, nil
		}
		if attempt < alphaCallAttempts && !s.waitBeforeRetry(time.Duration(attempt)*alphaRetryBackoff) {
			break
		}
	}
	return status, payload, err
}

// hostRequestOnce 发出一次不重试的请求。
func (s *Service) hostRequestOnce(callbackID, method, rawURL string, headers http.Header, body []byte) (int, []byte, error) {
	payload := map[string]any{"method": method, "url": rawURL, "headers": headers}
	if len(body) > 0 {
		payload["body"] = body
	}
	if callbackID != "" {
		payload["host_callback_id"] = callbackID
	}
	up, err := s.openUpstream(payload, time.Now().Add(alphaCallTimeout))
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
