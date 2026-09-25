package bridge

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedCall 记录一次出站请求，用于断言调用序列与鉴权头。
type recordedCall struct {
	method string
	url    string
	header http.Header
}

// recordingHost 按顺序消费 hostPlan 并记录请求。
type recordingHost struct {
	mu      sync.Mutex
	plans   []hostPlan
	streams map[string][]readChunk
	reads   map[string]int
	calls   []recordedCall
	saved   []Credential
}

func newRecordingHost(plans ...hostPlan) *recordingHost {
	return &recordingHost{plans: plans, streams: map[string][]readChunk{}, reads: map[string]int{}}
}

func (h *recordingHost) call(method string, payload, out any) error {
	request, _ := payload.(map[string]any)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case "host.http.do_stream":
		header, _ := request["headers"].(http.Header)
		h.calls = append(h.calls, recordedCall{method: str(request["method"]), url: str(request["url"]), header: header})
		if len(h.plans) == 0 {
			return fmt.Errorf("unexpected upstream request %q", str(request["url"]))
		}
		plan := h.plans[0]
		h.plans = h.plans[1:]
		streamID := fmt.Sprintf("recording-%d", len(h.calls))
		h.streams[streamID] = plan.chunks
		*out.(*upstreamStream) = upstreamStream{StatusCode: plan.status, Headers: plan.header, StreamID: streamID}
		return nil
	case "host.http.stream_read":
		streamID := str(request["stream_id"])
		chunks, ok := h.streams[streamID]
		if !ok {
			return fmt.Errorf("unknown upstream stream %q", streamID)
		}
		index := h.reads[streamID]
		h.reads[streamID]++
		if index >= len(chunks) {
			*out.(*readChunk) = readChunk{Done: true}
		} else {
			*out.(*readChunk) = chunks[index]
		}
		return nil
	case "host.http.stream_close":
		return nil
	case "host.auth.save":
		raw, ok := request["json"].(json.RawMessage)
		if !ok {
			return fmt.Errorf("saved credential was not raw JSON")
		}
		var credential Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return err
		}
		h.saved = append(h.saved, credential)
		return nil
	default:
		return fmt.Errorf("unexpected host callback %q", method)
	}
}

func (h *recordingHost) snapshot() []recordedCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedCall(nil), h.calls...)
}

func jsonStatusPlan(status int, body any) hostPlan {
	return hostPlan{status: status, header: http.Header{"Content-Type": []string{"application/json"}}, chunks: []readChunk{{Payload: jsonBytes(body), Done: true}}}
}

func apiKeyCredential() Credential {
	return Credential{Type: Provider, ID: "commandcodebridge-quota", Label: "goat-user", APIKey: "cmd-test-key"}
}

func whoamiPlan() hostPlan {
	return jsonStatusPlan(200, map[string]any{
		"user": map[string]any{"id": "user-1", "userName": "goat-user", "email": "goat@example.com"},
		"org":  map[string]any{"id": "org-42", "login": "goat-org", "name": "Goat Org"},
	})
}

// creditsPlan 复刻 /alpha/billing/credits 的真实形状（2026-09-25 实测）：
// windowLimits 与 credits 平级，credits 里没有 planId。
func creditsPlan() hostPlan {
	return jsonStatusPlan(200, map[string]any{
		"credits": map[string]any{
			"belowThreshold":   false,
			"creditThreshold":  0,
			"monthlyCredits":   400,
			"purchasedCredits": 50.5,
			"freeCredits":      9.5,
		},
		"windowLimits": map[string]any{
			"limited":  true,
			"exceeded": "weekly",
			"fiveHour": map[string]any{"used": 25, "cap": 100, "exceeded": false, "resetAt": float64(time.Now().Add(3 * time.Hour).UnixMilli())},
			"weekly":   map[string]any{"used": 600, "cap": 2000, "exceeded": false, "resetAt": float64(time.Now().Add(60 * time.Hour).UnixMilli())},
		},
		"sandboxAccess": false,
	})
}

func subscriptionPlan() hostPlan {
	return jsonStatusPlan(200, map[string]any{
		"data": map[string]any{
			"planId":             "individual-goat",
			"status":             "active",
			"currentPeriodStart": "2026-09-01T00:00:00Z",
			"currentPeriodEnd":   time.Now().Add(20 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
	})
}

func usageSummaryPlan(totalCost any) hostPlan {
	return jsonStatusPlan(200, map[string]any{"totalCost": totalCost})
}

func bucketByWindow(t *testing.T, response any, window string) map[string]any {
	t.Helper()
	out, ok := response.(map[string]any)
	if !ok {
		t.Fatalf("quota response = %#v", response)
	}
	groups, ok := out["groups"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("quota groups = %#v", out["groups"])
	}
	buckets, ok := groups[0].(map[string]any)["buckets"].([]any)
	if !ok {
		t.Fatalf("quota buckets = %#v", groups[0])
	}
	for _, item := range buckets {
		bucket := item.(map[string]any)
		if bucket["window"] == window {
			return bucket
		}
	}
	t.Fatalf("bucket %q not found in %#v", window, buckets)
	return nil
}

func metricByKey(t *testing.T, response any, key string) map[string]any {
	t.Helper()
	out, ok := response.(map[string]any)
	if !ok {
		t.Fatalf("quota response = %#v", response)
	}
	summary, ok := out["summary"].([]any)
	if !ok {
		t.Fatalf("quota summary = %#v", out["summary"])
	}
	for _, item := range summary {
		metric := item.(map[string]any)
		if metric["key"] == key {
			return metric
		}
	}
	t.Fatalf("metric %q not found in %#v", key, summary)
	return nil
}

func TestQuotaFetchReportsWindowsBalanceAndPlan(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := apiKeyCredential()
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newRecordingHost(whoamiPlan(), creditsPlan(), subscriptionPlan(), usageSummaryPlan(123.45))
	s.SetHost(h.call)
	result, err := s.fetchQuota(jsonBytes(quotaRequest{StorageJSON: jsonBytes(credential), AuthID: credential.ID}))
	if err != nil {
		t.Fatalf("fetch quota: %v", err)
	}

	fiveHour := bucketByWindow(t, result, "five_hour")
	if fiveHour["remainingFraction"].(float64) != 0.75 {
		t.Fatalf("five_hour bucket = %#v", fiveHour)
	}
	if str(fiveHour["resetTime"]) == "" {
		t.Fatalf("five_hour resetTime missing: %#v", fiveHour)
	}
	weekly := bucketByWindow(t, result, "seven_day")
	if weekly["remainingFraction"].(float64) != 0.7 {
		t.Fatalf("seven_day bucket = %#v", weekly)
	}

	if balance := metricByKey(t, result, "command_code_balance"); balance["value"].(float64) != 460 {
		t.Fatalf("balance = %#v", balance)
	}
	if extra := metricByKey(t, result, "command_code_extra_credits"); extra["value"].(float64) != 60 {
		t.Fatalf("extra credits = %#v", extra)
	}
	if spent := metricByKey(t, result, "command_code_period_spent"); spent["value"].(float64) != 123.45 {
		t.Fatalf("spent = %#v", spent)
	}
	if days := metricByKey(t, result, "command_code_period_days_left"); days["value"].(float64) <= 0 {
		t.Fatalf("period days left = %#v", days)
	}
	subscription, ok := result.(map[string]any)["subscription"].(map[string]any)
	if !ok || subscription["plan"] != "GOAT" {
		t.Fatalf("subscription = %#v", result.(map[string]any)["subscription"])
	}

	calls := h.snapshot()
	if len(calls) != 4 {
		t.Fatalf("quota used %d upstream calls: %#v", len(calls), calls)
	}
	if calls[0].url != "https://api.commandcode.ai/alpha/whoami?limits=1" || calls[0].method != http.MethodGet {
		t.Fatalf("whoami call = %#v", calls[0])
	}
	if calls[1].url != "https://api.commandcode.ai/alpha/billing/credits?orgId=org-42" {
		t.Fatalf("credits call = %#v", calls[1])
	}
	if calls[2].url != "https://api.commandcode.ai/alpha/billing/subscriptions?orgId=org-42" {
		t.Fatalf("subscriptions call = %#v", calls[2])
	}
	if calls[3].url != "https://api.commandcode.ai/alpha/usage/summary?orgId=org-42&since=2026-09-01T00%3A00%3A00Z" {
		t.Fatalf("summary call = %#v", calls[3])
	}
	// 全部 /alpha 调用必须带 Bearer API key。
	for i, call := range calls {
		if got := call.header.Get("Authorization"); got != "Bearer cmd-test-key" {
			t.Fatalf("call %d authorization = %q", i, got)
		}
	}
}

// TestQuotaFetchMatchesLiveApiShapes 复刻实测确认过的上游响应形状（数值已脱敏）：
// 个人账户 whoami 无 org、windowLimits 与 credits 平级、planId 只在 subscriptions 里。
// 曾经把 windowLimits 当 credits 的子对象解析，导致窗口桶永远为空。
func TestQuotaFetchMatchesLiveApiShapes(t *testing.T) {
	s := registeredService(t, "")
	credential := apiKeyCredential()
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newRecordingHost(
		jsonStatusPlan(200, map[string]any{
			"success": true,
			"user":    map[string]any{"id": "7c9e6679-7425-40de-944b-e07fc1f90ae7", "name": "example-user", "userName": "example-user"},
			"org":     nil,
		}),
		jsonStatusPlan(200, map[string]any{
			"credits": map[string]any{
				"belowThreshold": false, "creditThreshold": 0,
				"monthlyCredits": 28.5, "purchasedCredits": 0, "freeCredits": 0,
			},
			"windowLimits": map[string]any{
				"limited": true, "exceeded": "weekly",
				"fiveHour": map[string]any{"used": 0, "cap": 10, "exceeded": false, "resetAt": 0},
				"weekly":   map[string]any{"used": 12.5, "cap": 12, "exceeded": true, "resetAt": 1790000000000},
			},
			"sandboxAccess": false,
		}),
		jsonStatusPlan(200, map[string]any{"success": true, "data": map[string]any{
			"planId": "individual-pro", "status": "active",
			"currentPeriodStart": "2026-09-01T00:00:00Z",
			"currentPeriodEnd":   "2026-10-01T00:00:00Z",
		}}),
		jsonStatusPlan(200, map[string]any{
			"totalCount": 42, "totalCost": 21.5, "totalMonthlyCredits": 21.5,
		}),
	)
	s.SetHost(h.call)
	result, err := s.fetchQuota(jsonBytes(quotaRequest{StorageJSON: jsonBytes(credential), AuthID: credential.ID}))
	if err != nil {
		t.Fatalf("fetch quota: %v", err)
	}
	if got := bucketByWindow(t, result, "five_hour")["remainingFraction"].(float64); got != 1 {
		t.Fatalf("five_hour remaining = %v", got)
	}
	// 7 天窗口已超额（12.5 / 12），必须钳到 0 而不是负数。
	if got := bucketByWindow(t, result, "seven_day")["remainingFraction"].(float64); got != 0 {
		t.Fatalf("seven_day remaining = %v", got)
	}
	// 月度窗口：上游不给上限，用「本周期已用 + 剩余月额度」还原（21.5 + 28.5 = 50）。
	monthly := bucketByWindow(t, result, "monthly")
	if got := monthly["remainingFraction"].(float64); math.Abs(got-0.57) > 1e-9 {
		t.Fatalf("monthly remaining = %v, want 28.5/50 = 0.57", got)
	}
	if monthly["resetTime"] != "2026-10-01T00:00:00Z" {
		t.Fatalf("monthly resetTime = %#v", monthly["resetTime"])
	}
	if desc := str(monthly["description"]); !strings.Contains(desc, "43.0%") {
		t.Fatalf("monthly description = %q", desc)
	}
	if plan := result.(map[string]any)["subscription"].(map[string]any)["plan"]; plan != "Pro" {
		t.Fatalf("plan = %#v", plan)
	}
	if balance := metricByKey(t, result, "command_code_balance"); balance["value"].(float64) != 28.5 {
		t.Fatalf("balance = %#v", balance)
	}
	// 个人账户没有 org：带上空 orgId 会让上游返回 400 Invalid UUID。
	for _, call := range h.snapshot() {
		if strings.Contains(call.url, "orgId") {
			t.Fatalf("orgId must be omitted when whoami has no org: %s", call.url)
		}
	}
}

// 按量付费（无窗口限制）不应导致整体失败，余额仍可展示。
func TestQuotaFetchSurvivesUnlimitedAccounts(t *testing.T) {
	s := registeredService(t, "")
	credential := apiKeyCredential()
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newRecordingHost(
		whoamiPlan(),
		jsonStatusPlan(200, map[string]any{
			"credits":      map[string]any{"monthlyCredits": 0, "purchasedCredits": 25, "freeCredits": 0},
			"windowLimits": map[string]any{"limited": false},
		}),
		jsonStatusPlan(200, map[string]any{"data": map[string]any{}}),
		jsonStatusPlan(200, map[string]any{}),
	)
	s.SetHost(h.call)
	result, err := s.fetchQuota(jsonBytes(quotaRequest{StorageJSON: jsonBytes(credential), AuthID: credential.ID}))
	if err != nil {
		t.Fatalf("fetch quota without window limits: %v", err)
	}
	if balance := metricByKey(t, result, "command_code_balance"); balance["value"].(float64) != 25 {
		t.Fatalf("balance = %#v", balance)
	}
	groups := result.(map[string]any)["groups"].([]any)
	if len(list(groups[0].(map[string]any)["buckets"])) != 0 {
		t.Fatalf("unexpected buckets: %#v", groups[0])
	}
}

func TestQuotaDescribeReportsProviderKey(t *testing.T) {
	describe := quotaDescribe().(map[string]any)
	// 硬约束：provider key 与 Provider 常量一致，管理面板靠它关联额度视图。
	if providers, ok := describe["supported_providers"].([]string); !ok || len(providers) != 1 || providers[0] != Provider {
		t.Fatalf("supported providers = %#v", describe["supported_providers"])
	}
	if describe["supports_reset"] != false {
		t.Fatalf("supports_reset = %#v", describe["supports_reset"])
	}
}

// 上游偶发抖动（5xx / 连接重置）必须重试：这是额度刷新超时的主要来源。
func TestHostRequestRetriesTransientFailures(t *testing.T) {
	s := registeredService(t, "")
	h := newRecordingHost(
		jsonStatusPlan(503, map[string]any{"error": "temporarily unavailable"}),
		jsonStatusPlan(200, map[string]any{"success": true, "user": map[string]any{"id": "user-1"}}),
	)
	s.SetHost(h.call)
	status, body, err := s.hostRequest("", http.MethodGet, "https://api.commandcode.ai/alpha/whoami?limits=1", http.Header{}, nil)
	if err != nil || status != 200 {
		t.Fatalf("retry result: status=%d body=%s err=%v", status, body, err)
	}
	if calls := h.snapshot(); len(calls) != 2 {
		t.Fatalf("expected 2 upstream attempts, got %d", len(calls))
	}
}

// 4xx 是确定性结果（比如 key 无效），重试只会拖慢报错。
func TestHostRequestDoesNotRetryClientErrors(t *testing.T) {
	s := registeredService(t, "")
	h := newRecordingHost(jsonStatusPlan(401, map[string]any{"error": "bad key"}))
	s.SetHost(h.call)
	status, _, err := s.hostRequest("", http.MethodGet, "https://api.commandcode.ai/alpha/whoami?limits=1", http.Header{}, nil)
	if err != nil || status != 401 {
		t.Fatalf("result: status=%d err=%v", status, err)
	}
	if calls := h.snapshot(); len(calls) != 1 {
		t.Fatalf("401 must not be retried, got %d attempts", len(calls))
	}
}

// whoami 只提供 orgId：它抖挂时额度视图仍应可用（个人账户本来就不带 orgId）。
func TestQuotaSurvivesWhoamiOutage(t *testing.T) {
	s := registeredService(t, "")
	credential := apiKeyCredential()
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newRecordingHost(
		jsonStatusPlan(502, map[string]any{"error": "bad gateway"}),
		jsonStatusPlan(502, map[string]any{"error": "bad gateway"}),
		jsonStatusPlan(502, map[string]any{"error": "bad gateway"}),
		creditsPlan(), subscriptionPlan(), usageSummaryPlan(12),
	)
	s.SetHost(h.call)
	result, err := s.fetchQuota(jsonBytes(quotaRequest{StorageJSON: jsonBytes(credential), AuthID: credential.ID}))
	if err != nil {
		t.Fatalf("whoami outage must not fail the whole quota fetch: %v", err)
	}
	if balance := metricByKey(t, result, "command_code_balance"); balance["value"].(float64) != 460 {
		t.Fatalf("balance = %#v", balance)
	}
	calls := h.snapshot()
	if len(calls) != 6 {
		t.Fatalf("expected 3 whoami attempts + 3 endpoint calls, got %d", len(calls))
	}
	// whoami 取不到 orgId 时后续调用不能带该参数（空值会被上游判为 Invalid UUID）。
	for _, call := range calls[3:] {
		if strings.Contains(call.url, "orgId") {
			t.Fatalf("orgId must be omitted when whoami is unavailable: %s", call.url)
		}
	}
}

func TestVerifyAPIKeyRejectsInvalidKeys(t *testing.T) {
	s := registeredService(t, "")
	h := newRecordingHost(jsonStatusPlan(401, map[string]any{"success": false, "error": map[string]any{"code": "UNAUTHORIZED", "message": "Invalid API key"}}))
	s.SetHost(h.call)
	if _, err := s.verifyAPIKey("callback-1", "bad-key"); err == nil || !strings.Contains(err.Error(), "Invalid API key") || statusOf(err) != 401 {
		t.Fatalf("verify error = %v (status %d)", err, statusOf(err))
	}
}

func TestImportCredentialValidatesKeyAndStoresUserName(t *testing.T) {
	s := registeredService(t, "")
	h := newRecordingHost(whoamiPlan())
	s.SetHost(h.call)
	response, err := s.management(jsonBytes(ManagementRequest{Method: "POST", Path: apiBase + "/credentials", Body: []byte(`{"api_key":"cmd-live-key"}`)}))
	if err != nil {
		t.Fatal(err)
	}
	management := response.(ManagementResponse)
	if management.StatusCode != 201 {
		t.Fatalf("import status = %d: %s", management.StatusCode, management.Body)
	}
	if len(h.saved) != 1 {
		t.Fatalf("saved credentials = %#v", h.saved)
	}
	if h.saved[0].Label != "goat-user" || h.saved[0].AccountID != "user-1" || h.saved[0].Type != Provider {
		t.Fatalf("saved credential = %#v", h.saved[0])
	}
	// 无效 key 必须被 whoami 校验拒绝。
	h2 := newRecordingHost(jsonStatusPlan(401, map[string]any{"success": false, "error": map[string]any{"message": "Invalid API key"}}))
	s.SetHost(h2.call)
	rejected, err := s.management(jsonBytes(ManagementRequest{Method: "POST", Path: apiBase + "/credentials", Body: []byte(`{"api_key":"short"}`)}))
	if err != nil {
		t.Fatal(err)
	}
	if rejected.(ManagementResponse).StatusCode != 400 {
		t.Fatalf("short key status = %d", rejected.(ManagementResponse).StatusCode)
	}
}

func TestRefreshModelsFetchesCatalogAndEndpoints(t *testing.T) {
	s := registeredService(t, "")
	h := newRecordingHost(jsonStatusPlan(200, map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "claude-sonnet-5", "supported_endpoints": []any{"/messages"}},
		map[string]any{"id": "deepseek/deepseek-v4-flash", "supported_endpoints": []any{"/chat/completions", "/responses"}},
	}}))
	s.SetHost(h.call)
	models, err := s.refreshModels("callback-1")
	if err != nil {
		t.Fatalf("refresh models: %v", err)
	}
	if len(models) != 2 || models[0].UpstreamID != "claude-sonnet-5" {
		t.Fatalf("models = %#v", models)
	}
	if !s.modelNeedsAnthropic("claude-sonnet-5") || s.modelNeedsAnthropic("deepseek/deepseek-v4-flash") {
		t.Fatal("endpoint catalog was not applied")
	}
	calls := h.snapshot()
	if len(calls) != 1 || calls[0].url != "https://api.commandcode.ai/provider/v1/models" {
		t.Fatalf("catalog call = %#v", calls)
	}
}
