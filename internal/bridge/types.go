package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const Version = "0.1.1"
const Provider = "command-code"
const PluginID = "commandcodebridge"

// emptyContentMessage 是插件自己判定“聚合后没有任何输出”时给出的提示。
const emptyContentMessage = "上游返回了空内容"

// upstreamEmptyContentHint 是上游可能原样透传的空内容提示；
// 空内容判定与交给 CPA 的 request_scoped_errors.match 必须同时识别它，
// 否则原生模式的回退会失效。
const upstreamEmptyContentHint = "empty response content"

// emptyContentSignals 返回所有代表“空内容”的信号串。
func emptyContentSignals() []string {
	return []string{emptyContentMessage, upstreamEmptyContentHint}
}

type APIError struct {
	Status  int
	Kind    string
	Message string
}

func (e *APIError) Error() string           { return e.Message }
func (e *APIError) StatusCode() int         { return e.Status }
func (e *APIError) Code() string            { return e.Kind }
func fail(status int, message string) error { return &APIError{status, "upstream_error", message} }

type HostCall func(string, any, any) error
type ExecutorRequest struct {
	deadline                                               time.Time
	AuthID, AuthProvider, Model, Format, SourceFormat, Alt string
	Stream                                                 bool
	Headers                                                http.Header
	Query                                                  url.Values
	OriginalRequest, Payload, StorageJSON                  []byte
	Metadata, AuthMetadata                                 map[string]any
	AuthAttributes                                         map[string]string
	StreamID                                               string `json:"stream_id"`
	HostCallbackID                                         string `json:"host_callback_id"`
}
type Response struct {
	Payload  []byte
	Headers  http.Header
	Metadata map[string]any `json:",omitempty"`
}
type ManagementRequest struct {
	Method, Path   string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id"`
}
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
// Credential 是一条 Command Code API Key 凭据。
// Command Code 只使用长期 API Key（Studio 或 `cmd login` 产生，CLI 与 Provider API 共用），
// 没有 OAuth 令牌与续期流程，因此令牌字段只有 api_key。
type Credential struct {
	Type                string             `json:"type"`
	ID                  string             `json:"id"`
	Label               string             `json:"label"`
	APIKey              string             `json:"api_key,omitempty"`
	AccountID           string             `json:"account_id,omitempty"`
	Disabled            bool               `json:"disabled"`
	ProxyURL            string             `json:"proxy_url,omitempty"`
	RequestScopedErrors []RequestErrorRule `json:"request_scoped_errors"`
	ModelRevision       string             `json:"model_revision,omitempty"`
}

// bearerToken 返回直接填入 Authorization: Bearer 的值。
func (c Credential) bearerToken() string { return strings.TrimSpace(c.APIKey) }

type RequestErrorRule struct {
	Status int      `json:"status"`
	Match  []string `json:"match"`
	Action string   `json:"action"`
}

func requestErrorRules() []RequestErrorRule {
	return []RequestErrorRule{{Status: 500, Match: emptyContentSignals(), Action: "stop"}}
}

type Model struct {
	ID         string   `json:"id" yaml:"id"`
	UpstreamID string   `json:"upstream_id" yaml:"upstream_id"`
	Providers  []string `json:"providers" yaml:"providers"`
}
type Config struct {
	DataDir          string  `json:"data_dir" yaml:"data_dir"`
	BaseURL          string  `json:"base_url" yaml:"base_url"`
	Models           []Model `json:"models" yaml:"models"`
	NonstreamMode    string  `json:"nonstream_mode" yaml:"nonstream_mode"`
	TimeoutSeconds   int     `json:"timeout_seconds" yaml:"timeout_seconds"`
	LogRetention     int     `json:"log_retention" yaml:"log_retention"`
	MaxResponseBytes int     `json:"max_response_bytes" yaml:"max_response_bytes"`
}

func defaultConfig() Config {
	// 裸名 claude-* 会与 CPA 内置 Claude 模型撞名（插件注册被忽略、请求报 unknown provider），
	// Claude 系模型一律以 command-code/ 前缀暴露。
	return Config{DataDir: "plugins/commandcodebridge-data", BaseURL: "https://api.commandcode.ai/provider/v1", Models: []Model{{ID: "deepseek-v4-flash", UpstreamID: "deepseek/deepseek-v4-flash"}, {ID: "deepseek/deepseek-v4-flash", UpstreamID: "deepseek/deepseek-v4-flash"}, {ID: "command-code/claude-sonnet-5", UpstreamID: "claude-sonnet-5"}}, NonstreamMode: "stream-aggregate", TimeoutSeconds: 180, LogRetention: 1000, MaxResponseBytes: 16 << 20}
}
func (c *Config) validate() error {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Scheme != "https" || u.Host != "api.commandcode.ai" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/provider/v1" {
		return fail(400, "base_url 必须为 https://api.commandcode.ai/provider/v1")
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 1800 {
		return fail(400, "timeout_seconds 必须在 10 到 1800 之间")
	}
	if c.LogRetention < 50 || c.LogRetention > 10000 {
		return fail(400, "log_retention 必须在 50 到 10000 之间")
	}
	if c.MaxResponseBytes < 65536 || c.MaxResponseBytes > 64<<20 {
		return fail(400, "max_response_bytes 必须在 64 KiB 到 64 MiB 之间")
	}
	if c.NonstreamMode != "native" && c.NonstreamMode != "native-fallback" && c.NonstreamMode != "stream-aggregate" {
		return fail(400, "nonstream_mode 无效")
	}
	seen := map[string]bool{}
	for i := range c.Models {
		m := &c.Models[i]
		if strings.TrimSpace(m.ID) == "" || seen[m.ID] {
			return fail(400, "模型别名不能为空且不能重复")
		}
		seen[m.ID] = true
		if strings.TrimSpace(m.UpstreamID) == "" || strings.ContainsAny(m.ID+m.UpstreamID, "\r\n\t") {
			return fail(400, "模型标识不能为空且不能包含控制类空白字符")
		}
	}
	return nil
}

// alphaBase 返回内部 /alpha/* 接口的基地址（https://api.commandcode.ai）。
// CLI 逆向（Vt.prod）与 Provider API 同域；从已校验的 base_url 推导，避免引入多余配置。
func (c Config) alphaBase() string {
	return strings.TrimSuffix(strings.TrimSuffix(c.BaseURL, "/"), "/provider/v1")
}

type Attempt struct {
	Status         int    `json:"status"`
	Mode           string `json:"mode"`
	Provider       string `json:"provider"`
	ProviderSource string `json:"provider_source"`
	DurationMS     int64  `json:"duration_ms"`
	Error          string `json:"error,omitempty"`
}
type LogEntry struct {
	ID               string    `json:"id"`
	Time             time.Time `json:"time"`
	Model            string    `json:"model"`
	UpstreamModel    string    `json:"upstream_model"`
	Stream           bool      `json:"stream"`
	Status           int       `json:"status"`
	Provider         string    `json:"provider"`
	ProviderSource   string    `json:"provider_source"`
	DurationMS       int64     `json:"duration_ms"`
	TTFTMS           int64     `json:"ttft_ms"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens"`
	Credential       string    `json:"credential"`
	Attempts         []Attempt `json:"attempts"`
	Error            string    `json:"error,omitempty"`
}

func jsonBytes(v any) []byte      { b, _ := json.Marshal(v); return b }
func str(v any) string            { s, _ := v.(string); return s }
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }

// credentialID 用账号派生凭据 ID。内置供应商的凭据名能直接看出是哪个账号
// （如 claude-<邮箱>.json），这里保持一致；更重要的是：同一账号重复登录/重新粘贴
// 会落到同一个 ID，从而覆盖同一份凭据，而不是每操作一次就多出一条。
func credentialID(userID, userName string) string {
	if slug := slugifyAccount(userName); slug != "" {
		return PluginID + "-" + slug
	}
	if slug := slugifyAccount(userID); slug != "" {
		return PluginID + "-" + slug
	}
	return PluginID + "-" + id()
}

// slugifyAccount 保留可安全用作文件名的字符（凭据 ID 会拼进 auth-dir 的文件名），
// 其余字符折叠成 '-'；必须与 deleteCredential 的路径校验保持一致。
func slugifyAccount(value string) string {
	var b strings.Builder
	dashPending := false
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
			dashPending = false
		default:
			if !dashPending && b.Len() > 0 {
				b.WriteByte('-')
				dashPending = true
			}
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "-.")
}
func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int:
		return int64(n)
	case int64:
		return n
	case int32:
		return int64(n)
	}
	return 0
}
func decodeObject(b []byte) (map[string]any, error) {
	var j map[string]any
	if err := json.Unmarshal(b, &j); err != nil || j == nil {
		return nil, fail(502, "上游返回了无效 JSON")
	}
	return j, nil
}
func errorMessage(j map[string]any) string {
	v := j["error"]
	if m := object(v); m != nil {
		return str(m["message"])
	}
	if s := str(v); s != "" {
		return s
	}
	return "上游请求失败"
}
func statusOf(err error) int {
	if err == nil {
		return 200
	}
	if e, ok := err.(interface{ StatusCode() int }); ok && e.StatusCode() > 0 {
		return e.StatusCode()
	}
	return 502
}
func safeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// 按字符截断，避免把中文截成半个字符产生非法 UTF-8。
	if runes := []rune(s); len(runes) > 500 {
		s = string(runes[:500])
	}
	return fmt.Sprintf("%s", secretPattern.ReplaceAllString(s, "[已脱敏]"))
}
