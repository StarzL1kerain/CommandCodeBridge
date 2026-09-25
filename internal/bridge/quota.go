package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Command Code 的用量信息来自四个内部只读接口（CLI 1.65.2 逆向，/usage 命令同款调用序列）：
//
//	GET {alpha}/alpha/whoami?limits=1            -> {user, org}            用户与组织
//	GET {alpha}/alpha/billing/credits?orgId=      -> {credits:{...}}        剩余积分与窗口上限
//	GET {alpha}/alpha/billing/subscriptions?orgId= -> {data:{planId, 周期}}  订阅与周期
//	GET {alpha}/alpha/usage/summary?orgId=&since= -> {totalCost}           周期内已花费
//
// 全部使用 Authorization: Bearer <API Key>。字段形状依据 CLI 的 projectUsageView /
// fetchUsageCredits / fetchUsageSubscription / fetchUsageSummary 消费路径逐字段核对：
//   - credits.credits.planId、windowLimits.fiveHour.{used,cap,resetAt}
//   - subscriptions.data.currentPeriodStart（多一层 data 包装，其余接口没有）
//   - summary.totalCost
// used/cap 是积分（1 积分 ≈ 1 美元用量），resetAt 是 epoch 毫秒（CLI 直接与 Date.now() 比较）。

// planDisplayNames 与 CLI 的 planId -> 展示名映射保持一致。
var planDisplayNames = map[string]string{
	"individual-go":       "Go",
	"individual-goat":     "GOAT",
	"individual-pro":      "Pro",
	"individual-pro-v1":   "Pro",
	"individual-provider": "Provider",
	"individual-max":      "Max",
	"individual-ultra":    "Ultra",
	"teams-pro":           "Teams Pro",
}

// quotaWindows 把 windowLimits 的两个滚动窗口映射为宿主管理面板能识别的 window token。
// 面板会把 token 翻译成中文标签（five_hour -> 5 小时限额、seven_day -> 7 天限额）；
// 认不出的 token 会退化成内部 id，所以这里的取值不能随意改。
var quotaWindows = []struct {
	apiKey string
	window string
	label  string
}{
	{"fiveHour", "five_hour", "5 小时"},
	{"weekly", "seven_day", "7 天"},
}

type windowLimit struct {
	Used    float64 `json:"used"`
	Cap     float64 `json:"cap"`
	ResetAt float64 `json:"resetAt"`
}

type creditsPayload struct {
	Credits struct {
		PlanID           string `json:"planId"`
		MonthlyCredits   any    `json:"monthlyCredits"`
		PurchasedCredits any    `json:"purchasedCredits"`
		FreeCredits      any    `json:"freeCredits"`
		WindowLimits     struct {
			Limited  bool         `json:"limited"`
			FiveHour *windowLimit `json:"fiveHour"`
			Weekly   *windowLimit `json:"weekly"`
		} `json:"windowLimits"`
	} `json:"credits"`
}

type subscriptionPayload struct {
	Data struct {
		PlanID             string `json:"planId"`
		Status             string `json:"status"`
		CurrentPeriodStart string `json:"currentPeriodStart"`
		CurrentPeriodEnd   string `json:"currentPeriodEnd"`
	} `json:"data"`
}

type usageSummaryPayload struct {
	TotalCost any `json:"totalCost"`
}

// quotaRequest 对应宿主的 QuotaFetchRequest / QuotaResetRequest。
type quotaRequest struct {
	AuthIndex      string  `json:"auth_index"`
	AuthID         string  `json:"auth_id"`
	Provider       string  `json:"provider"`
	StorageJSON    []byte  `json:"storage_json"`
	Metadata       any     `json:"metadata"`
	HostCallbackID string  `json:"host_callback_id"`
	AuthIndexCamel *string `json:"authIndex"`
	AuthIDPascal   *string `json:"AuthID"`
}

func (r quotaRequest) credentialID() string {
	for _, candidate := range []string{r.AuthID, r.AuthIndex, valueOrEmpty(r.AuthIDPascal), valueOrEmpty(r.AuthIndexCamel)} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// quotaDescribe 声明本插件支持的 provider 与是否支持重置。
// 宿主侧 QuotaDescribeResponse 的字段标签是 snake_case 且没有自定义反序列化，
// 因此这里同时给出 camelCase 别名作为兜底：多余字段会被 encoding/json 忽略。
// 硬约束：这里的 provider key 必须与 Provider 常量、authData.Metadata.type 完全一致。
func quotaDescribe() any {
	return map[string]any{
		"supported_providers": []string{Provider},
		"SupportedProviders":  []string{Provider},
		"display_name":        "Command Code 额度",
		"displayName":         "Command Code 额度",
		"supports_reset":      false,
		"supportsReset":       false,
	}
}

func quotaUnsupportedReset() any {
	return map[string]any{"success": false, "message": "Command Code 未提供重置额度的接口"}
}

func (s *Service) quotaCredential(storage []byte, credentialID string) (Credential, error) {
	var c Credential
	if len(storage) > 0 {
		_ = json.Unmarshal(storage, &c)
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	} else if current, ok := s.creds[credentialID]; ok {
		c = current
	}
	s.mu.RUnlock()
	if c.bearerToken() == "" || c.Disabled {
		return c, fail(401, "Command Code 凭据缺失或已停用")
	}
	return c, nil
}

// alphaGet 以凭据 key 访问内部 /alpha 接口并解码 JSON。
// 注意各接口没有统一信封：credits 包了一层 credits、subscriptions 包了一层 data，
// 因此这里只做 HTTP 状态与错误信封检查，由调用方按各自形状解析。
func (s *Service) alphaGet(callbackID, path string, query url.Values, token string, out any) error {
	target := s.config().alphaBase() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	headers := http.Header{
		"Accept":        []string{"application/json"},
		"Authorization": []string{"Bearer " + token},
	}
	status, body, err := s.hostRequest(callbackID, http.MethodGet, target, headers, nil)
	if err != nil {
		return fail(502, "无法连接 Command Code 服务："+safeError(err))
	}
	if status < 200 || status >= 300 {
		j, _ := decodeObject(body)
		return fail(statusOr(status, 502), "读取额度失败："+errorMessage(j))
	}
	if out != nil {
		if e := json.Unmarshal(body, out); e != nil {
			return fail(502, "解析额度响应失败")
		}
	}
	return nil
}

// floatOf 宽容解析积分数值：上游可能给 number 或字符串。
func floatOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		var f float64
		_ = json.Unmarshal([]byte(n), &f)
		return f
	}
	return 0
}

// fetchQuota 汇总积分余额与两个滚动窗口的剩余比例。
// 用量数据缺失时不硬失败：按量付费账户没有窗口限制，仍应展示余额。
func (s *Service) fetchQuota(raw json.RawMessage) (any, error) {
	var r quotaRequest
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	c, e := s.quotaCredential(r.StorageJSON, r.credentialID())
	if e != nil {
		return nil, e
	}
	if e := s.begin(); e != nil {
		return nil, e
	}
	defer s.active.Done()
	token := c.bearerToken()

	who, err := s.verifyAPIKey(r.HostCallbackID, token)
	if err != nil {
		return nil, err
	}
	orgID := strings.TrimSpace(who.Org.ID)
	query := url.Values{}
	if orgID != "" {
		query.Set("orgId", orgID)
	}

	var credits creditsPayload
	creditsErr := s.alphaGet(r.HostCallbackID, "/alpha/billing/credits", query, token, &credits)
	var sub subscriptionPayload
	subErr := s.alphaGet(r.HostCallbackID, "/alpha/billing/subscriptions", query, token, &sub)
	if creditsErr != nil && subErr != nil {
		return nil, creditsErr
	}

	// 周期起点之后的已花费；解析失败不影响窗口展示。
	summaryQuery := url.Values{}
	for k, v := range query {
		summaryQuery[k] = v
	}
	if periodStart := strings.TrimSpace(sub.Data.CurrentPeriodStart); periodStart != "" {
		summaryQuery.Set("since", periodStart)
	}
	var summary usageSummaryPayload
	_ = s.alphaGet(r.HostCallbackID, "/alpha/usage/summary", summaryQuery, token, &summary)

	return buildQuotaResponse(credits, sub, summary, time.Now()), nil
}

func buildQuotaResponse(credits creditsPayload, sub subscriptionPayload, summary usageSummaryPayload, now time.Time) any {
	c := credits.Credits
	buckets := []any{}
	if c.WindowLimits.Limited {
		for _, window := range quotaWindows {
			var limit *windowLimit
			if window.apiKey == "fiveHour" {
				limit = c.WindowLimits.FiveHour
			} else {
				limit = c.WindowLimits.Weekly
			}
			if limit == nil || limit.Cap <= 0 {
				continue
			}
			// 上游给出已用积分与上限，宿主只认 remainingFraction（0~1）。
			remaining := 1 - limit.Used/limit.Cap
			if remaining < 0 {
				remaining = 0
			}
			if remaining > 1 {
				remaining = 1
			}
			bucket := map[string]any{
				"window":            window.window,
				"remainingFraction": remaining,
				"description":       fmt.Sprintf("%s窗口已用 %.1f%%（%.2f / %.2f 积分）", window.label, limit.Used/limit.Cap*100, limit.Used, limit.Cap),
			}
			if limit.ResetAt > 0 {
				bucket["resetTime"] = time.UnixMilli(int64(limit.ResetAt)).UTC().Format(time.RFC3339Nano)
			}
			buckets = append(buckets, bucket)
		}
	}

	total := floatOf(c.MonthlyCredits) + floatOf(c.PurchasedCredits) + floatOf(c.FreeCredits)
	extra := floatOf(c.PurchasedCredits) + floatOf(c.FreeCredits)
	summary2 := []any{
		quotaCurrencyMetric("command_code_balance", "剩余积分", total),
	}
	if extra > 0 {
		summary2 = append(summary2, quotaCurrencyMetric("command_code_extra_credits", "额外积分", extra))
	}
	if spent := floatOf(summary.TotalCost); spent > 0 {
		summary2 = append(summary2, quotaCurrencyMetric("command_code_period_spent", "本周期已花费", spent))
	}
	if end, err := time.Parse(time.RFC3339, strings.TrimSpace(sub.Data.CurrentPeriodEnd)); err == nil {
		days := end.Sub(now).Hours() / 24
		if days < 0 {
			days = 0
		}
		summary2 = append(summary2, map[string]any{
			"key":    "command_code_period_days_left",
			"label":  "当前周期剩余天数",
			"value":  float64(int(days*10)) / 10,
			"unit":   "天",
			"format": "number",
		})
	}

	response := map[string]any{
		"groups": []any{map[string]any{"displayName": "Command Code 额度", "buckets": buckets}},
	}
	if len(summary2) > 0 {
		response["summary"] = summary2
	}
	planName := planDisplayName(strings.TrimSpace(sub.Data.PlanID))
	if planName == "" {
		planName = planDisplayName(strings.TrimSpace(c.PlanID))
	}
	if planName != "" {
		response["subscription"] = map[string]any{
			"plan":     planName,
			"tierName": planName,
		}
	}
	return response
}

// planDisplayName 把上游 planId 翻译为 CLI 同款展示名，未收录的原样返回。
func planDisplayName(planID string) string {
	if planID == "" {
		return ""
	}
	if name, ok := planDisplayNames[planID]; ok {
		return name
	}
	return planID
}

// quotaCurrencyMetric 构造一条货币指标。宿主只接受 ISO 4217 大写代码。
func quotaCurrencyMetric(key, label string, value float64) map[string]any {
	return map[string]any{
		"key":      key,
		"label":    label,
		"value":    value,
		"format":   "currency",
		"currency": "USD",
	}
}

// managementQuota 供插件管理页直接查看，便于在不依赖宿主 UI 的情况下核对额度。
func (s *Service) managementQuota(credentialID string) any {
	s.mu.RLock()
	c, ok := s.creds[credentialID]
	if !ok && credentialID == "" {
		for _, candidate := range s.creds {
			c, ok = candidate, true
			break
		}
	}
	s.mu.RUnlock()
	if !ok {
		return map[string]any{"error": "未找到凭据"}
	}
	result, err := s.fetchQuota(jsonBytes(quotaRequest{StorageJSON: jsonBytes(c), AuthID: c.ID}))
	if err != nil {
		return map[string]any{"error": safeError(err)}
	}
	out := result.(map[string]any)
	out["credential"] = map[string]any{"id": c.ID, "label": c.Label, "auth_kind": "api_key"}
	return out
}

func quotaIdentity() any {
	return map[string]any{"identifier": Provider}
}
