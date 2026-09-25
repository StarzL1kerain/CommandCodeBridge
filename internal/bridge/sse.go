package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// SSEDecoder tolerates arbitrary network chunk boundaries, CRLF and multiline data.
type SSEDecoder struct {
	buffer []byte
	data   []string
	size   int
	done   bool
	max    int
	event  string
}

func (d *SSEDecoder) Feed(b []byte, emit func([]byte, string) error) error {
	d.buffer = append(d.buffer, b...)
	if len(d.buffer) > d.max {
		return fail(502, "SSE 数据帧超过配置上限")
	}
	for {
		i := bytes.IndexByte(d.buffer, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSuffix(string(d.buffer[:i]), "\r")
		d.buffer = d.buffer[i+1:]
		if line == "" {
			if len(d.data) > 0 {
				payload := []byte(strings.Join(d.data, "\n"))
				if e := emit(payload, d.event); e != nil {
					return e
				}
			}
			d.data = nil
			d.size = 0
			d.event = ""
			continue
		}
		if strings.HasPrefix(line, "data:") {
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			d.data = append(d.data, v)
			d.size += len(v)
			if d.size > d.max {
				return fail(502, "SSE 数据帧超过配置上限")
			}
		}
		if strings.HasPrefix(line, "event:") {
			d.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	return nil
}
func (d *SSEDecoder) End() error {
	if strings.TrimSpace(string(d.buffer)) != "" || len(d.data) > 0 {
		return fail(502, "上游流结束时 SSE 数据帧不完整")
	}
	return nil
}

type completion struct {
	root      map[string]any
	choices   map[int64]map[string]any
	tools     map[int64]map[int64]map[string]any
	usage     map[string]any
	finished  bool
	done      bool
	hasOutput bool
}

func newCompletion() *completion {
	return &completion{root: map[string]any{}, choices: map[int64]map[string]any{}, tools: map[int64]map[int64]map[string]any{}}
}
func (c *completion) observe(j map[string]any) {
	for _, k := range []string{"id", "created", "model", "system_fingerprint", "provider"} {
		if v, ok := j[k]; ok {
			c.root[k] = v
		}
	}
	if u := object(j["usage"]); u != nil {
		if c.usage == nil {
			c.usage = map[string]any{}
		}
		mergeObjects(c.usage, u, false)
	}
	for _, v := range list(j["choices"]) {
		ch := object(v)
		idx := number(ch["index"])
		dest := c.choices[idx]
		if dest == nil {
			dest = map[string]any{"index": idx, "message": map[string]any{"role": "assistant"}, "finish_reason": nil}
			c.choices[idx] = dest
		}
		delta := object(ch["delta"])
		msg := object(dest["message"])
		for k, v := range delta {
			switch k {
			case "tool_calls":
				if c.tools[idx] == nil {
					c.tools[idx] = map[int64]map[string]any{}
				}
				for _, tv := range list(v) {
					tc := object(tv)
					ti := number(tc["index"])
					t := c.tools[idx][ti]
					if t == nil {
						t = map[string]any{}
						c.tools[idx][ti] = t
					}
					for tk, tv := range tc {
						if tk == "index" {
							continue
						}
						if tk == "function" {
							f := object(t[tk])
							if f == nil {
								f = map[string]any{}
								t[tk] = f
							}
							mergeObjects(f, object(tv), true)
						} else if tv != nil {
							t[tk] = tv
						}
					}
				}
				c.hasOutput = true
			case "content", "reasoning_content", "reasoning", "refusal":
				if s := str(v); s != "" {
					msg[k] = str(msg[k]) + s
					c.hasOutput = true
				}
			case "function_call":
				f := object(msg[k])
				if f == nil {
					f = map[string]any{}
					msg[k] = f
				}
				mergeObjects(f, object(v), true)
				c.hasOutput = true
			default:
				if v != nil {
					if m := object(v); m != nil {
						old := object(msg[k])
						if old == nil {
							old = map[string]any{}
							msg[k] = old
						}
						mergeObjects(old, m, false)
					} else if a, ok := v.([]any); ok {
						msg[k] = append(list(msg[k]), a...)
					} else {
						msg[k] = v
					}
				}
			}
		}
		if ch["finish_reason"] != nil {
			dest["finish_reason"] = ch["finish_reason"]
			c.finished = true
		}
		if ch["logprobs"] != nil {
			lp := object(dest["logprobs"])
			if lp == nil {
				lp = map[string]any{}
				dest["logprobs"] = lp
			}
			for key, value := range object(ch["logprobs"]) {
				if a, ok := value.([]any); ok {
					lp[key] = append(list(lp[key]), a...)
				} else if value != nil {
					lp[key] = value
				}
			}
		}
	}
}
func (c *completion) allFinished() bool {
	if len(c.choices) == 0 {
		return false
	}
	for _, ch := range c.choices {
		if ch["finish_reason"] == nil {
			return false
		}
	}
	return true
}
func (c *completion) result(model string) ([]byte, error) {
	if !c.done || !c.allFinished() {
		return nil, fail(502, "上游流在收到完整内容与 [DONE] 之前就结束了")
	}
	if !c.hasOutput {
		return nil, fail(500, emptyContentMessage)
	}
	out := c.root
	out["object"] = "chat.completion"
	out["model"] = model
	indices := []int{}
	for i := range c.choices {
		indices = append(indices, int(i))
	}
	sort.Ints(indices)
	choices := []any{}
	for _, i := range indices {
		ch := c.choices[int64(i)]
		if tools := c.tools[int64(i)]; len(tools) > 0 {
			ti := []int{}
			for k := range tools {
				ti = append(ti, int(k))
			}
			sort.Ints(ti)
			a := []any{}
			for _, k := range ti {
				a = append(a, tools[int64(k)])
			}
			object(ch["message"])["tool_calls"] = a
		}
		choices = append(choices, ch)
	}
	out["choices"] = choices
	if c.usage != nil {
		out["usage"] = c.usage
	}
	return json.Marshal(out)
}
func list(v any) []any { a, _ := v.([]any); return a }
func mergeObjects(dst, src map[string]any, concat bool) {
	for k, v := range src {
		if m := object(v); m != nil {
			d := object(dst[k])
			if d == nil {
				d = map[string]any{}
				dst[k] = d
			}
			mergeObjects(d, m, concat)
		} else if concat {
			if s, ok := v.(string); ok {
				dst[k] = str(dst[k]) + s
			} else if v != nil {
				dst[k] = v
			}
		} else if v != nil {
			dst[k] = v
		}
	}
}
func unwrap(body []byte) (map[string]any, error) {
	j, e := decodeObject(body)
	if e != nil {
		return nil, e
	}
	if j["error"] != nil || j["success"] == false {
		return nil, fail(500, errorMessage(j))
	}
	if len(list(j["choices"])) == 0 {
		return nil, fail(502, "Command Code 响应缺少 choices")
	}
	return j, nil
}

// Only explicit response routing facts count as actual providers. Candidate lists never do.
// Command Code 未承诺在响应里给出底层供应商；这里只认显式 provider 字段。
func observeMetadata(j map[string]any, entry *LogEntry, attempt *Attempt) {
	if p := str(j["provider"]); p != "" {
		entry.Provider = p
		entry.ProviderSource = "provider"
		attempt.Provider = p
		attempt.ProviderSource = "provider"
	}
	if u := object(j["usage"]); u != nil {
		entry.PromptTokens = number(u["prompt_tokens"])
		entry.CompletionTokens = number(u["completion_tokens"])
		entry.CachedTokens = number(object(u["prompt_tokens_details"])["cached_tokens"])
		entry.ReasoningTokens = number(object(u["completion_tokens_details"])["reasoning_tokens"])
	}
}
func contentStarted(j map[string]any) bool {
	for _, ch := range list(j["choices"]) {
		d := object(object(ch)["delta"])
		if str(d["content"]) != "" || str(d["reasoning_content"]) != "" || str(d["reasoning"]) != "" || len(list(d["tool_calls"])) > 0 {
			return true
		}
	}
	return false
}
func isEmptyError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, signal := range emptyContentSignals() {
		if strings.Contains(message, strings.ToLower(signal)) {
			return true
		}
	}
	return false
}

var errStreamDone = errors.New("stream complete")
