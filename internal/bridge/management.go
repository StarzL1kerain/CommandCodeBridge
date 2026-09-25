package bridge

import (
	"embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed ui/*
var ui embed.FS

const apiBase = "/v0/management/commandcodebridge"

func (s *Service) registerManagement(raw json.RawMessage) (any, error) {
	routes := []map[string]string{}
	for _, p := range []string{"status", "logs", "models", "config", "credentials", "quota"} {
		routes = append(routes, map[string]string{"Method": "GET", "Path": apiBase + "/" + p})
	}
	for _, p := range []string{"models/refresh", "credentials"} {
		routes = append(routes, map[string]string{"Method": "POST", "Path": apiBase + "/" + p})
	}
	for _, p := range []string{"models", "config", "credentials"} {
		routes = append(routes, map[string]string{"Method": "PUT", "Path": apiBase + "/" + p})
	}
	routes = append(routes, map[string]string{"Method": "DELETE", "Path": apiBase + "/credentials"})
	return map[string]any{"routes": routes, "resources": []map[string]string{{"Path": "/console", "Menu": "CommandCodeBridge", "Description": "Command Code 模型、凭据与请求日志"}}}, nil
}
func managementJSON(code int, v any) (any, error) {
	return ManagementResponse{StatusCode: code, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: jsonBytes(v)}, nil
}
func (s *Service) management(raw json.RawMessage) (any, error) {
	var r ManagementRequest
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	if r.Method == "GET" && r.Path == "/v0/resource/plugins/"+PluginID+"/console" {
		b, e := ui.ReadFile("ui/index.html")
		if e != nil {
			return nil, e
		}
		b = []byte(strings.ReplaceAll(string(b), "__PASSBRIDGE_API_BASE__", apiBase))
		authJS, err := ui.ReadFile("ui/cpa-auth.js")
		if err != nil {
			return nil, err
		}
		b = []byte(strings.ReplaceAll(string(b), "/*__CPA_AUTH_COMPAT__*/", string(authJS)))
		return ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}, "X-Content-Type-Options": []string{"nosniff"}}, Body: b}, nil
	}
	if !strings.HasPrefix(r.Path, apiBase+"/") {
		return managementJSON(404, map[string]any{"error": "未找到"})
	}
	p := strings.TrimPrefix(r.Path, apiBase)
	switch r.Method + " " + p {
	case "GET /status":
		s.mu.RLock()
		logError := s.logWriteError
		s.mu.RUnlock()
		return managementJSON(200, map[string]any{"version": Version, "log_persistence_error": logError, "credential_count": len(s.credentials()), "model_count": len(s.config().Models)})
	case "GET /logs":
		return s.logsResponse(r)
	case "GET /config":
		return managementJSON(200, s.config())
	case "PUT /config":
		cfg := s.config()
		dataDir := cfg.DataDir
		base := cfg.BaseURL
		if e := json.Unmarshal(r.Body, &cfg); e != nil {
			return managementJSON(400, map[string]any{"error": "config JSON 无效"})
		}
		cfg.DataDir = dataDir
		cfg.BaseURL = base
		if e := s.saveConfig(cfg); e != nil {
			return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
		}
		return managementJSON(200, cfg)
	case "GET /models":
		return managementJSON(200, map[string]any{"models": s.config().Models})
	case "PUT /models":
		var in struct {
			Models *[]Model `json:"models"`
		}
		if e := json.Unmarshal(r.Body, &in); e != nil {
			return managementJSON(400, map[string]any{"error": "模型 JSON 无效"})
		}
		cfg := s.config()
		if in.Models == nil {
			return managementJSON(400, map[string]any{"error": "models 必须是数组"})
		}
		cfg.Models = *in.Models
		if e := s.saveConfig(cfg); e != nil {
			return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
		}
		return managementJSON(200, map[string]any{"models": cfg.Models, "message": "已保存，CPA 的凭据注册已刷新。"})
	case "POST /models/refresh":
		models, e := s.refreshModels(r.HostCallbackID)
		if e != nil {
			return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
		}
		return managementJSON(200, map[string]any{"models": models})
	case "GET /credentials":
		return managementJSON(200, map[string]any{"items": s.credentials()})
	case "GET /quota":
		return managementJSON(200, s.managementQuota(r.Query.Get("id")))
	case "POST /credentials":
		return s.importCredential(r)
	case "PUT /credentials":
		return s.updateCredential(r)
	case "DELETE /credentials":
		return s.deleteCredential(r.Query.Get("id"))
	default:
		return managementJSON(404, map[string]any{"error": "未找到"})
	}
}
func (s *Service) saveConfig(cfg Config) error {
	if e := cfg.validate(); e != nil {
		return e
	}
	if e := atomicJSON(filepath.Join(cfg.DataDir, "settings.json"), cfg); e != nil {
		return e
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return s.refreshRegistrations()
}
func (s *Service) logsResponse(r ManagementRequest) (any, error) {
	limit, _ := strconv.Atoi(r.Query.Get("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.Query.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	search := strings.ToLower(r.Query.Get("search"))
	status := r.Query.Get("status")
	provider := r.Query.Get("provider")
	s.mu.RLock()
	defer s.mu.RUnlock()
	filtered := []LogEntry{}
	for i := len(s.logs) - 1; i >= 0; i-- {
		v := s.logs[i]
		if search != "" && !strings.Contains(strings.ToLower(v.ID+" "+v.Model+" "+v.UpstreamModel+" "+v.Provider+" "+v.Error), search) {
			continue
		}
		if provider != "" && provider != "all" && !strings.EqualFold(provider, v.Provider) {
			continue
		}
		if status != "" && status != "all" {
			if status == "success" && v.Status >= 400 || status == "error" && v.Status < 400 {
				continue
			}
			if n, e := strconv.Atoi(status); e == nil && n != v.Status {
				continue
			}
		}
		filtered = append(filtered, v)
	}
	total := len(filtered)
	var prompt, completion, cached int64
	for _, entry := range filtered {
		prompt += entry.PromptTokens
		completion += entry.CompletionTokens
		cached += entry.CachedTokens
	}
	var cacheRate any
	if prompt > 0 {
		cacheRate = float64(cached) / float64(prompt)
	}
	summary := map[string]any{"requests": total, "prompt_tokens": prompt, "completion_tokens": completion, "cached_tokens": cached, "cache_rate": cacheRate}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return managementJSON(200, map[string]any{"items": filtered[offset:end], "total": total, "summary": summary})
}
func (s *Service) importCredential(r ManagementRequest) (any, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	var in struct {
		Label  string `json:"label"`
		APIKey string `json:"api_key"`
	}
	if e := json.Unmarshal(r.Body, &in); e != nil {
		return managementJSON(400, map[string]any{"error": "凭据 JSON 无效"})
	}
	in.APIKey = strings.TrimSpace(in.APIKey)
	if len(in.APIKey) < 8 || strings.ContainsAny(in.APIKey, "\r\n") {
		return managementJSON(400, map[string]any{"error": "API key 无效"})
	}
	if len(in.Label) > 100 {
		return managementJSON(400, map[string]any{"error": "备注长度不能超过 100 个字符"})
	}
	// 导入前先用 /alpha/whoami 校验 key，顺手取回用户名作为默认备注。
	who, e := s.verifyAPIKey(r.HostCallbackID, in.APIKey)
	if e != nil {
		return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
	}
	c := Credential{Type: Provider, ID: PluginID + "-" + id(), Label: strings.TrimSpace(in.Label), APIKey: in.APIKey, AccountID: strings.TrimSpace(who.User.ID), RequestScopedErrors: requestErrorRules()}
	if c.Label == "" {
		c.Label = who.displayName()
	}
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
	return managementJSON(201, map[string]any{"id": c.ID, "label": c.Label, "enabled": true})
}
func (s *Service) updateCredential(r ManagementRequest) (any, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	var in struct {
		Label  *string `json:"label"`
		APIKey *string `json:"api_key"`
	}
	if e := json.Unmarshal(r.Body, &in); e != nil {
		return managementJSON(400, map[string]any{"error": "凭据 JSON 无效"})
	}
	id := r.Query.Get("id")
	s.mu.RLock()
	c, ok := s.creds[id]
	filename := s.authFiles[id]
	s.mu.RUnlock()
	if !ok {
		return managementJSON(404, map[string]any{"error": "未找到凭据"})
	}
	if in.Label != nil {
		c.Label = strings.TrimSpace(*in.Label)
		if len(c.Label) > 100 {
			return managementJSON(400, map[string]any{"error": "备注长度不能超过 100 个字符"})
		}
		if c.Label == "" {
			c.Label = "Command Code"
		}
	}
	if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
		key := strings.TrimSpace(*in.APIKey)
		if len(key) < 8 || strings.ContainsAny(key, "\r\n") {
			return managementJSON(400, map[string]any{"error": "API key 无效"})
		}
		c.APIKey = key
	}
	if filename == "" {
		filename = c.ID + ".json"
	}
	if filepath.Base(filename) != filename || strings.ContainsAny(filename, "/\\") {
		return managementJSON(400, map[string]any{"error": "凭据文件名无效"})
	}
	c.RequestScopedErrors = requestErrorRules()
	if e := s.call("host.auth.save", map[string]any{"name": filename, "json": json.RawMessage(jsonBytes(c))}, nil); e != nil {
		return managementJSON(500, map[string]any{"error": "凭据保存失败"})
	}
	s.mu.Lock()
	s.creds[id] = c
	s.mu.Unlock()
	return managementJSON(200, map[string]any{"id": c.ID, "label": c.Label, "enabled": !c.Disabled})
}
func (s *Service) deleteCredential(credentialID string) (any, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[credentialID]
	if !ok {
		return managementJSON(404, map[string]any{"error": "未找到凭据"})
	}
	if s.authDir == "" {
		return managementJSON(409, map[string]any{"error": "尚未解析到凭据存储目录"})
	}
	if filepath.Base(c.ID) != c.ID || strings.ContainsAny(c.ID, "/\\") {
		return managementJSON(400, map[string]any{"error": "凭据 ID 无效"})
	}
	filename := s.authFiles[c.ID]
	if filename == "" {
		filename = c.ID + ".json"
	}
	if filepath.Base(filename) != filename || strings.ContainsAny(filename, "/\\") {
		return managementJSON(400, map[string]any{"error": "凭据文件名无效"})
	}
	path := filepath.Join(s.authDir, filename)
	b, e := os.ReadFile(path)
	if e != nil {
		return managementJSON(409, map[string]any{"error": "凭据文件不存在"})
	}
	var disk Credential
	if json.Unmarshal(b, &disk) != nil || disk.Type != Provider || disk.ID != c.ID {
		return managementJSON(409, map[string]any{"error": "凭据文件归属校验失败"})
	}
	if e = os.Remove(path); e != nil {
		return managementJSON(500, map[string]any{"error": "凭据删除失败"})
	}
	delete(s.creds, credentialID)
	delete(s.authFiles, credentialID)
	s.revoked[credentialID] = true
	return managementJSON(200, map[string]any{"deleted": true})
}
func (s *Service) refreshModels(callbackID string) ([]Model, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	// 该端点无需鉴权（已实测），返回 OpenAI 风格 {data:[{id, supported_endpoints}]}。
	// supported_endpoints 决定模型走 /chat/completions 还是 /messages。
	up, e := s.openUpstream(map[string]any{"host_callback_id": callbackID, "method": "GET", "url": s.config().BaseURL + "/models", "headers": http.Header{"Accept": []string{"application/json"}}}, time.Time{})
	if e != nil {
		return nil, e
	}
	b, e := s.readJSON(up)
	if e != nil {
		return nil, e
	}
	j, e := decodeObject(b)
	if e != nil {
		return nil, e
	}
	candidates := list(j["data"])
	if len(candidates) == 0 {
		return nil, fail(502, "Command Code 目录未返回任何模型")
	}
	models := []Model{}
	endpoints := map[string][]string{}
	for _, v := range candidates {
		m := object(v)
		upstreamID := str(m["id"])
		if upstreamID == "" || strings.ContainsAny(upstreamID, "\r\n\t") {
			continue
		}
		supported := []string{}
		for _, endpoint := range list(m["supported_endpoints"]) {
			if ep := str(endpoint); ep != "" {
				supported = append(supported, ep)
			}
		}
		endpoints[upstreamID] = supported
		models = append(models, Model{ID: upstreamID, UpstreamID: upstreamID})
	}
	if len(models) == 0 {
		return nil, fail(502, "Command Code 目录未返回有效模型")
	}
	s.mu.Lock()
	s.modelEndpoints = endpoints
	s.mu.Unlock()
	return models, nil
}
