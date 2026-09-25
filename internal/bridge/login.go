package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Command Code 的"终端登录"在 CLI 里是本地回环回调：CLI 在 127.0.0.1 起监听，
// 授权页把 apiKey 通过浏览器重定向送回该端口（cli.mjs 逆向实证：
//   /studio/auth/cli?callback=http://127.0.0.1:<port>/callback&state=...&mode=redirect
//   回调参数 apiKey/state/userId/userName/keyName）。
//
// 关键约束（2026-09-25 实测）：授权页只接受 localhost 回调，其他地址一律返回
// "Invalid Request ... Only localhost URLs are allowed for security"。
// 因此不能把 callback 指向插件控制台或 CPA 面板；插件进程也无法监听用户本机的端口，
// 只能让浏览器跳到本机端口、由用户把地址栏里的完整回调 URL 复制回控制台粘贴，
// 再由 completeLogin 解析出 apiKey。无论登录从面板还是控制台发起，都走这条链路。
const (
	studioAuthPath = "/studio/auth/cli"
	// manualCallback 是唯一的回调地址：上游授权页只接受 localhost 回调，
	// 浏览器会停在这个打不开的地址上，用户需要把地址栏里的 URL 复制回来粘贴。
	manualCallback = "http://127.0.0.1:41017/callback"
	// loginSessionTTL 与宿主插件 OAuth 会话的 30 分钟对齐：手动复制粘贴流程较慢，
	// TTL 短于宿主会导致会话在宿主还在轮询时就过期。
	loginSessionTTL  = 30 * time.Minute
	studioBaseSecure = "https://commandcode.ai"
)

// loginSession 是一次浏览器登录的进行中状态。
type loginSession struct {
	state   string
	label   string
	expires time.Time
	// hostDriven 表示会话由宿主的 auth.login.start 发起：凭据不在完成后立即落盘，
	// 而是挂在这里等宿主轮询 auth.login.poll 成功时由宿主自己保存。
	hostDriven bool
	// credential 是 hostDriven 会话完成后待交给宿主的凭据。
	credential *Credential
}

// newLoginState 生成会话 state（hex，天然落在宿主 OAuth 的字符白名单内）。
func newLoginState() (string, error) {
	buf := make([]byte, 16)
	if _, e := rand.Read(buf); e != nil {
		return "", e
	}
	return hex.EncodeToString(buf), nil
}

// pruneLoginSessions 清理过期会话，返回是否发生了修改。
func (s *Service) pruneLoginSessions() {
	now := time.Now()
	for k, v := range s.loginSessions {
		if now.After(v.expires) {
			delete(s.loginSessions, k)
		}
	}
}

// buildStudioAuthURL 按逆向出的 CLI 格式拼授权 URL。
func buildStudioAuthURL(callback, state string) string {
	q := url.Values{}
	q.Set("callback", callback)
	q.Set("state", state)
	q.Set("mode", "redirect")
	return studioBaseSecure + studioAuthPath + "?" + q.Encode()
}

// startHostLogin 响应宿主的 auth.login.start。
// 宿主只把成功响应按 {URL, State} 解析并把 URL 交给面板打开，所以这里必须返回成功形状；
// 回调固定用本地回环地址（上游只认 localhost），用户在浏览器里拿到 apiKey 后仍需到
// 插件控制台粘贴回调 URL，凭据再由宿主在 auth.login.poll 成功时自行落盘。
func (s *Service) startHostLogin(raw json.RawMessage) (any, error) {
	var r struct {
		Provider string
		BaseURL  string
		Host     struct{ AuthDir string }
		Metadata map[string]any `json:"Metadata"`
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	if r.Host.AuthDir != "" {
		s.mu.Lock()
		s.authDir = r.Host.AuthDir
		s.mu.Unlock()
	}
	state, e := newLoginState()
	if e != nil {
		return nil, fail(500, "无法生成登录会话")
	}
	s.loginMu.Lock()
	s.pruneLoginSessions()
	s.loginSessions[state] = &loginSession{state: state, hostDriven: true, expires: time.Now().Add(loginSessionTTL)}
	s.loginMu.Unlock()
	return map[string]any{"Provider": Provider, "URL": buildStudioAuthURL(manualCallback, state), "State": state}, nil
}

// pollHostLogin 响应宿主的 auth.login.poll。
// 完成前返回 pending；一旦控制台回调把凭据挂上会话，就返回 success 让宿主自己落盘。
func (s *Service) pollHostLogin(raw json.RawMessage) (any, error) {
	var r struct {
		Provider string
		State    string
		Metadata map[string]any `json:"Metadata"`
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	state := strings.TrimSpace(r.State)
	if state == "" {
		return loginPollError("登录会话标识为空，请重新发起登录"), nil
	}
	s.loginMu.Lock()
	s.pruneLoginSessions()
	session := s.loginSessions[state]
	if session != nil && session.credential != nil {
		delete(s.loginSessions, state)
	}
	s.loginMu.Unlock()
	if session == nil {
		return loginPollError("登录会话不存在或已过期，请重新发起浏览器登录"), nil
	}
	if session.credential == nil {
		return map[string]any{"Status": "pending", "Message": "等待浏览器完成授权"}, nil
	}
	c := *session.credential
	return map[string]any{"Status": "success", "Auths": []any{authData(c, c.ID+".json")}}, nil
}

func loginPollError(message string) map[string]any {
	return map[string]any{"Status": "error", "Message": message}
}

// startLoginSession 处理 POST /login/session：创建会话并返回授权 URL。
// 回调固定是本地回环地址：上游只接受 localhost 回调，浏览器授权后会跳到本机端口
// （那里没有服务，页面打不开是正常的），用户要把地址栏里的完整 URL 复制回来粘贴。
func (s *Service) startLoginSession(r ManagementRequest) (any, error) {
	var in struct {
		Label string `json:"label"`
	}
	if e := json.Unmarshal(r.Body, &in); e != nil {
		return managementJSON(400, map[string]any{"error": "请求 JSON 无效"})
	}
	if len(in.Label) > 100 {
		return managementJSON(400, map[string]any{"error": "备注长度不能超过 100 个字符"})
	}
	state, e := newLoginState()
	if e != nil {
		return managementJSON(500, map[string]any{"error": "无法生成登录会话"})
	}
	s.loginMu.Lock()
	s.pruneLoginSessions()
	s.loginSessions[state] = &loginSession{state: state, label: strings.TrimSpace(in.Label), expires: time.Now().Add(loginSessionTTL)}
	s.loginMu.Unlock()
	return managementJSON(200, map[string]any{"url": buildStudioAuthURL(manualCallback, state), "state": state, "expires_in_seconds": int(loginSessionTTL.Seconds())})
}

// loginParams 解析用户粘贴回来的授权结果。授权页既可能把参数拼在回调 URL 的查询串上，
// 也可能以表单（POST body）形式提交，用户复制到的内容因此不固定，这里全部兼容：
//  1. 完整回调 URL：http://127.0.0.1:41017/callback?apiKey=…&state=…
//  2. 只有查询串：?apiKey=…&state=… 或 callback?apiKey=…&state=…
//  3. 纯表单数据：apiKey=…&state=…&userId=…&userName=…&keyName=…
//  4. 参数放在 fragment（#）里的 URL
func loginParams(raw string) url.Values {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return url.Values{}
	}
	if u, e := url.Parse(raw); e == nil && (u.Scheme != "" || u.Host != "") {
		if values := u.Query(); len(values) > 0 {
			return values
		}
		if values, e := url.ParseQuery(strings.TrimPrefix(u.Fragment, "?")); e == nil {
			return values
		}
		return url.Values{}
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[i+1:]
	}
	values, e := url.ParseQuery(strings.TrimPrefix(raw, "?"))
	if e != nil {
		return url.Values{}
	}
	return values
}

// completeLogin 处理 POST /login/complete：校验 state、解析 apiKey 并落盘凭据。
// 用户粘贴的内容可以是完整回调 URL，也可以只是表单数据里的 apiKey=…&state=…。
func (s *Service) completeLogin(r ManagementRequest) (any, error) {
	var in struct {
		URL      string `json:"url"`
		Data     string `json:"data"`
		APIKey   string `json:"api_key"`
		State    string `json:"state"`
		Label    string `json:"label"`
		UserName string `json:"user_name"`
	}
	if e := json.Unmarshal(r.Body, &in); e != nil {
		return managementJSON(400, map[string]any{"error": "请求 JSON 无效"})
	}
	for _, raw := range []string{in.URL, in.Data} {
		q := loginParams(raw)
		if in.APIKey == "" {
			in.APIKey = q.Get("apiKey")
		}
		if in.State == "" {
			in.State = q.Get("state")
		}
		if in.UserName == "" {
			in.UserName = q.Get("userName")
		}
	}
	in.APIKey = strings.TrimSpace(in.APIKey)
	if len(in.APIKey) < 8 || strings.ContainsAny(in.APIKey, "\r\n") {
		return managementJSON(400, map[string]any{"error": "回调里没有可用的 API Key（参数以表单 POST 提交，地址栏看不到：请按 F12 → Network → Payload 复制 apiKey=…&state=…，或粘贴完整回调 URL）"})
	}
	s.loginMu.Lock()
	s.pruneLoginSessions()
	session := s.loginSessions[in.State]
	// 控制台发起的登录在完成时一次性消费；宿主发起的会话要留到宿主轮询成功。
	if session != nil && !session.hostDriven {
		delete(s.loginSessions, in.State)
	}
	s.loginMu.Unlock()
	if session == nil {
		return managementJSON(400, map[string]any{"error": "登录会话不存在或已过期，请重新发起浏览器登录"})
	}
	if session.credential != nil {
		return managementJSON(409, map[string]any{"error": "该登录会话已完成，请重新发起浏览器登录"})
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		label = strings.TrimSpace(session.label)
	}
	if label == "" {
		label = strings.TrimSpace(in.UserName)
	}
	// 会话 state 只换一次机会；key 无效时必须重新发起登录，防止暴力试探。
	who, e := s.verifyAPIKey(r.HostCallbackID, in.APIKey)
	if e != nil {
		if session.hostDriven {
			s.loginMu.Lock()
			delete(s.loginSessions, in.State)
			s.loginMu.Unlock()
		}
		return managementJSON(statusOf(e), map[string]any{"error": "回调的 API Key 校验失败：" + safeError(e)})
	}
	if label == "" {
		label = who.displayName()
	}
	// ID 用账号派生：同一账号重复登录会覆盖同一份凭据，凭据名也能看出是哪个账号。
	c := Credential{Type: Provider, ID: credentialID(who.User.ID, who.displayName()), Label: label, APIKey: in.APIKey, AccountID: who.User.ID, RequestScopedErrors: requestErrorRules()}
	if c.Label == "" {
		c.Label = "Command Code"
	}
	if session.hostDriven {
		s.loginMu.Lock()
		current := s.loginSessions[in.State]
		if current == nil {
			s.loginMu.Unlock()
			return managementJSON(409, map[string]any{"error": "该登录会话已失效，请重新发起浏览器登录"})
		}
		current.credential = &c
		s.loginMu.Unlock()
		return managementJSON(201, map[string]any{"id": c.ID, "label": c.Label, "enabled": true, "message": "浏览器登录成功，请回到 CPA 面板完成登录。"})
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	var saved struct {
		Path string `json:"path"`
	}
	if e := s.call("host.auth.save", map[string]any{"name": c.ID + ".json", "json": json.RawMessage(jsonBytes(c))}, &saved); e != nil {
		return managementJSON(500, map[string]any{"error": "凭据保存失败"})
	}
	s.mu.Lock()
	s.creds[c.ID] = c
	s.authFiles[c.ID] = c.ID + ".json"
	if saved.Path != "" {
		s.authDir = filepath.Dir(saved.Path)
	}
	s.mu.Unlock()
	return managementJSON(201, map[string]any{"id": c.ID, "label": c.Label, "enabled": true, "message": "浏览器登录成功，凭据已保存。"})
}
