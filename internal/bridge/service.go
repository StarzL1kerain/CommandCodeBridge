package bridge

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var secretPattern = regexp.MustCompile(`(?i)(?:bearer\s+|sk[-_])[a-z0-9_.-]+`)

type Service struct {
	credentialMu  sync.Mutex
	authFiles     map[string]string
	mu            sync.RWMutex
	cfg           Config
	host          HostCall
	logs          []LogEntry
	creds         map[string]Credential
	authDir       string
	loaded        bool
	stopped       bool
	active        sync.WaitGroup
	streams       map[string]struct{}
	revoked       map[string]bool
	stopCh        chan struct{}
	logWriteError string
	// modelEndpoints 记录上游模型 -> Provider API 端点列表（/chat/completions、/messages）。
	modelEndpoints map[string][]string
	// modelSpecs 缓存上游目录里的上下文长度（按"名字最后一段"索引），
	// 供 /v1/models 拦截器给条目补规格用 —— 宿主那个出口只输出 4 个字段，规格会被丢掉。
	modelSpecs map[string]int
	loginMu    sync.Mutex
	// loginSessions 是浏览器登录的进行中会话（state -> session）。
	loginSessions map[string]*loginSession
}

func NewService() *Service {
	s := &Service{cfg: defaultConfig(), creds: map[string]Credential{}, authFiles: map[string]string{}, streams: map[string]struct{}{}, revoked: map[string]bool{}, stopCh: make(chan struct{}), modelEndpoints: map[string][]string{}, loginSessions: map[string]*loginSession{}}
	for _, model := range defaultAnthropicModels {
		s.modelEndpoints[model] = []string{"/messages"}
	}
	return s
}
func (s *Service) SetHost(h func(string, any, any) error) { s.mu.Lock(); s.host = h; s.mu.Unlock() }
func (s *Service) call(method string, in, out any) error {
	s.mu.RLock()
	h := s.host
	s.mu.RUnlock()
	if h == nil {
		return errors.New("宿主回调未初始化")
	}
	return h(method, in, out)
}
func (s *Service) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	c.Models = append([]Model{}, c.Models...)
	return c
}
func id() string { b := make([]byte, 12); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func atomicJSON(path string, v any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	tmp := path + ".tmp-" + id()
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

// defaultAnthropicModels 是 /provider/v1/models 中 supported_endpoints 仅有 /messages
// 的模型快照（2026-09-25 实测）。目录刷新会用最新结果替换；快照用于宿主离线启动时
// 仍能正确路由 Claude 系模型。
var defaultAnthropicModels = []string{
	"claude-sonnet-5",
	"claude-sonnet-4-6",
	"claude-fable-5-1",
	"claude-fable-5",
	"claude-opus-5-5",
	"claude-opus-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-haiku-4-5-20251001",
}

// modelNeedsAnthropic 判断上游模型是否只能走 Anthropic Messages 端点。
// 优先查目录刷新结果；没有目录信息时按 Claude 家族命名兜底。
func (s *Service) modelNeedsAnthropic(upstreamID string) bool {
	s.mu.RLock()
	endpoints := s.modelEndpoints[upstreamID]
	s.mu.RUnlock()
	return modelNeedsAnthropicBy(upstreamID, endpoints)
}

// modelNeedsAnthropicBy 是 modelNeedsAnthropic 的纯函数形式：
// 目录为空时按命名兜底，否则以 supported_endpoints 精确判定。
func modelNeedsAnthropicBy(upstreamID string, endpoints []string) bool {
	if len(endpoints) == 0 {
		lower := strings.ToLower(upstreamID)
		return strings.HasPrefix(lower, "claude") || strings.HasPrefix(lower, "anthropic/")
	}
	for _, endpoint := range endpoints {
		if strings.Contains(endpoint, "chat/completions") {
			return false
		}
	}
	for _, endpoint := range endpoints {
		if strings.Contains(endpoint, "/messages") {
			return true
		}
	}
	return false
}

func (s *Service) configure(raw json.RawMessage) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if e := json.Unmarshal(raw, &req); e != nil {
		return e
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if e := yaml.Unmarshal(req.ConfigYAML, &cfg); e != nil {
			return e
		}
	}
	// CPA passes plugin-owned config. State holds UI changes and bounded request metadata, never API keys.
	if b, e := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json")); e == nil {
		if e = json.Unmarshal(b, &cfg); e != nil {
			return e
		}
	}
	if e := cfg.validate(); e != nil {
		return e
	}
	if e := os.MkdirAll(cfg.DataDir, 0700); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	if !s.loaded {
		if b, e := os.ReadFile(filepath.Join(cfg.DataDir, "requests.json")); e == nil {
			_ = json.Unmarshal(b, &s.logs)
		}
		s.loaded = true
	}
	if len(s.logs) > cfg.LogRetention {
		s.logs = s.logs[len(s.logs)-cfg.LogRetention:]
	}
	return nil
}
func (s *Service) Handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if e := s.configure(raw); e != nil {
			return nil, e
		}
		return registration(), nil
	case "executor.identifier", "auth.identifier":
		return map[string]any{"identifier": Provider}, nil
	case "model.static", "model.for_auth", "model.register":
		return s.modelRegistration(), nil
	case "response.intercept_after":
		return s.interceptModelsResponse(raw)
	case "auth.parse":
		return s.parseAuth(raw)
	case "auth.login.start":
		return s.startHostLogin(raw)
	case "auth.login.poll":
		return s.pollHostLogin(raw)
	case "auth.refresh":
		return s.refreshAuth(raw)
	case "quota.identifier":
		return quotaIdentity(), nil
	case "quota.describe":
		return quotaDescribe(), nil
	case "quota.fetch":
		return s.fetchQuota(raw)
	case "quota.reset":
		return quotaUnsupportedReset(), nil
	case "executor.execute", "executor.execute_stream":
		var r ExecutorRequest
		if e := json.Unmarshal(raw, &r); e != nil {
			return nil, e
		}
		if method == "executor.execute_stream" {
			return s.executeStream(r)
		}
		return s.execute(r)
	case "executor.count_tokens":
		return nil, fail(501, "Command Code 未提供精确的 token 计数接口")
	case "executor.http_request":
		return nil, fail(400, "请改用 CommandCodeBridge 的模型执行器")
	case "management.register":
		return s.registerManagement(raw)
	case "management.handle":
		return s.management(raw)
	case "plugin.shutdown":
		s.mu.Lock()
		if !s.stopped {
			close(s.stopCh)
		}
		s.stopped = true
		streams := make([]string, 0, len(s.streams))
		for stream := range s.streams {
			streams = append(streams, stream)
		}
		s.mu.Unlock()
		for _, stream := range streams {
			s.closeUpstream(stream)
		}
		s.active.Wait()
		return map[string]any{}, nil
	default:
		return nil, fail(400, "不支持的插件方法："+method)
	}
}
func (s *Service) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return fail(503, "CommandCodeBridge 正在关闭")
	}
	s.active.Add(1)
	return nil
}

// Re-save this provider's auth records so CPA's watcher refreshes model registrations.
func (s *Service) refreshRegistrations() error {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	revision := sha256.Sum256(jsonBytes(s.config().Models))
	s.mu.RLock()
	credentials := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		credentials = append(credentials, c)
	}
	s.mu.RUnlock()
	for _, c := range credentials {
		// CPA skips unchanged files. A model revision makes watcher refreshes reliable.
		c.ModelRevision = hex.EncodeToString(revision[:])
		c.RequestScopedErrors = requestErrorRules()
		s.mu.RLock()
		filename := s.authFiles[c.ID]
		s.mu.RUnlock()
		if filename == "" {
			filename = c.ID + ".json"
		}
		if e := s.call("host.auth.save", map[string]any{"name": filename, "json": json.RawMessage(jsonBytes(c))}, nil); e != nil {
			return e
		}
	}
	return nil
}

func registration() any {
	return map[string]any{"schema_version": 6, "metadata": map[string]any{"Name": "CommandCodeBridge", "Version": Version, "Author": "StarzL1kerain", "GitHubRepository": "https://github.com/StarzL1kerain/CommandCodeBridge", "Logo": "https://raw.githubusercontent.com/StarzL1kerain/CommandCodeBridge/main/logo.png", "Description": "Command Code 订阅接入插件：支持 API key 凭据、OpenAI 兼容转发与 Claude 模型协议转换，并记录用量", "ConfigFields": []map[string]any{{"Name": "data_dir", "Type": "string", "Description": "插件状态持久化目录"}}}, "capabilities": map[string]any{"auth_provider": true, "model_provider": true, "executor": true, "executor_model_scope": "both", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions"}, "management_api": true, "quota_provider": true, "model_registrar": true, "response_interceptor": true}}
}
func (s *Service) modelRegistration() any {
	cfg := s.config()
	models := []map[string]any{}
	for _, m := range cfg.Models {
		// 有上游给的名字就用它，别让面板只显示原始 id。
		display := m.ID
		if name := strings.TrimSpace(m.Name); name != "" {
			display = name
		}
		// 规格字段：CPA 自己的模型目录只收录内置厂商，插件模型不会被自动补全，
		// 所以上下文长度必须由插件申报，否则宿主与客户端显示不出模型规格。
		specs := map[string]any{}
		if m.ContextLength > 0 {
			specs["ContextLength"] = m.ContextLength
		}
		entry := func(id, name, shown string) map[string]any {
			out := map[string]any{"ID": id, "Name": name, "Object": "model", "OwnedBy": Provider, "DisplayName": shown, "SupportedGenerationMethods": []string{"chat"}, "UserDefined": true}
			for key, value := range specs {
				out[key] = value
			}
			return out
		}
		models = append(models, entry(m.ID, m.UpstreamID, display))
		// 上游原名也注册一份：宿主按"客户端请求的模型名"找凭据，只注册一个名字时，
		// 客户端直接用上游名会被判成 no auth available（503）。
		if upstream := strings.TrimSpace(m.UpstreamID); upstream != "" && upstream != m.ID {
			models = append(models, entry(upstream, upstream, upstream))
		}
	}
	return map[string]any{"Provider": Provider, "Models": models}
}
func (s *Service) resolveModel(model string) (string, error) {
	cfg := s.config()
	for _, m := range cfg.Models {
		if model == m.ID || model == m.UpstreamID {
			return m.UpstreamID, nil
		}
	}
	return "", fail(400, "CommandCodeBridge 未启用该模型："+model)
}
func authData(c Credential, filename string) any {
	// 匹配规则必须与空内容时抛出的文案严格一致，否则 CPA 不会按预期停止重试。
	scoped := []any{}
	for _, rule := range requestErrorRules() {
		scoped = append(scoped, map[string]any{"status": rule.Status, "match": rule.Match, "action": rule.Action})
	}
	// 硬约束：authData.Metadata.type、Provider 常量与 quota.describe 的 provider key
	// 三者必须完全一致，否则宿主不会把该凭据与额度视图关联起来。
	return map[string]any{"Provider": Provider, "ID": c.ID, "FileName": filename, "Label": c.Label, "Disabled": c.Disabled, "ProxyURL": c.ProxyURL, "StorageJSON": jsonBytes(c), "Metadata": map[string]any{"type": Provider, "request_scoped_errors": scoped}, "Attributes": map[string]string{"auth_kind": "api_key"}}
}
func (s *Service) parseAuth(raw json.RawMessage) (any, error) {
	var r struct {
		Provider, Path, FileName string
		RawJSON                  []byte
		Host                     struct{ AuthDir string }
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	var c Credential
	if e := json.Unmarshal(r.RawJSON, &c); e != nil || c.Type != Provider {
		return map[string]any{"Handled": false}, nil
	}
	if c.bearerToken() == "" {
		return nil, fail(401, "Command Code 凭据缺少可用的 API Key")
	}
	if c.ID == "" {
		c.ID = strings.TrimSuffix(r.FileName, ".json")
	}
	if c.Label == "" {
		c.Label = c.ID
	}
	c.RequestScopedErrors = requestErrorRules()
	s.mu.Lock()
	s.creds[c.ID] = c
	if r.FileName != "" {
		s.authFiles[c.ID] = filepath.Base(r.FileName)
	}
	if r.Host.AuthDir != "" {
		s.authDir = r.Host.AuthDir
	}
	s.mu.Unlock()
	return map[string]any{"Handled": true, "Auth": authData(c, r.FileName)}, nil
}
func (s *Service) refreshAuth(raw json.RawMessage) (any, error) {
	var r struct {
		StorageJSON    []byte
		AuthID         string
		HostCallbackID string `json:"host_callback_id"`
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	var c Credential
	if e := json.Unmarshal(r.StorageJSON, &c); e != nil {
		return nil, e
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	} else if current, ok := s.creds[r.AuthID]; ok {
		c = current
	}
	filename := s.authFiles[c.ID]
	revoked := s.revoked[c.ID] || s.revoked[r.AuthID]
	s.mu.RUnlock()
	if revoked {
		return nil, fail(401, "Command Code 凭据已被删除")
	}
	if filename == "" {
		filename = c.ID + ".json"
	}
	// Command Code 的 API key 是长期静态凭据，无需续期。
	return map[string]any{"Auth": authData(c, filename), "NextRefreshAfter": time.Now().Add(365 * 24 * time.Hour)}, nil
}
func (s *Service) selectedCredential(r ExecutorRequest) (Credential, error) {
	var c Credential
	if len(r.StorageJSON) > 0 {
		_ = json.Unmarshal(r.StorageJSON, &c)
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	} else if current, ok := s.creds[r.AuthID]; ok {
		c = current
	}
	revoked := s.revoked[c.ID] || s.revoked[r.AuthID]
	s.mu.RUnlock()
	if c.bearerToken() == "" || c.Disabled || revoked {
		return c, fail(401, "Command Code 凭据缺失或已停用")
	}
	return c, nil
}
func (s *Service) appendLog(entry LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.Error = safeError(errors.New(entry.Error))
	s.logs = append(s.logs, entry)
	if len(s.logs) > s.cfg.LogRetention {
		s.logs = s.logs[len(s.logs)-s.cfg.LogRetention:]
	}
	s.logWriteError = safeError(atomicJSON(filepath.Join(s.cfg.DataDir, "requests.json"), s.logs))
}

// credentials 列出插件托管的凭据。列出前会核对 auth-dir 里的文件是否还在：
// 凭据文件被外部删除（例如在宿主的认证文件页里删掉）时，内存里的记录会变成一条
// "列表里还显示、删除又因读不到文件而失败"的幽灵记录，这里顺手把它清掉。
func (s *Service) credentials() []map[string]any {
	type entry struct {
		c    Credential
		file string
	}
	s.mu.RLock()
	dir := s.authDir
	items := make([]entry, 0, len(s.creds))
	for _, c := range s.creds {
		file := s.authFiles[c.ID]
		if file == "" {
			file = c.ID + ".json"
		}
		items = append(items, entry{c: c, file: file})
	}
	s.mu.RUnlock()
	out := []map[string]any{}
	stale := []entry{}
	for _, it := range items {
		if !authFileExists(dir, it.file) {
			stale = append(stale, it)
			continue
		}
		out = append(out, map[string]any{"id": it.c.ID, "label": it.c.Label, "enabled": !it.c.Disabled})
	}
	if len(stale) > 0 {
		s.mu.Lock()
		for _, it := range stale {
			// 加锁期间文件可能又回来了（或刚被重新导入），不能误删。
			if authFileExists(dir, it.file) {
				continue
			}
			if current, ok := s.authFiles[it.c.ID]; ok && current != it.file {
				continue
			}
			delete(s.creds, it.c.ID)
			delete(s.authFiles, it.c.ID)
		}
		s.mu.Unlock()
		for _, it := range stale {
			s.appendLog(LogEntry{
				ID:         id(),
				Time:       time.Now(),
				Model:      "(" + PluginID + " 凭据)",
				Status:     410,
				Credential: it.c.ID,
				Error:      "凭据文件已不在 auth-dir，已从列表移除",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return str(out[i]["id"]) < str(out[j]["id"]) })
	return out
}

// authFileExists 判断凭据文件是否还在 auth-dir 里。目录未知时一律返回 true：
// 宁可不清理，也不能在拿不到 auth-dir 时把全部凭据误判成幽灵记录。
func authFileExists(dir, file string) bool {
	if dir == "" || file == "" {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, file))
	return err == nil || !os.IsNotExist(err)
}
