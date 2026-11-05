// 用量统计：按「天 × 账号 × 模型」聚合代理侧看到的 token 用量。
//
// 重要前提：这是**代理侧计数**（上游返回的 usage，缺失时按字符估算），
// 不等于上游计费积分。CodeArts 的积分换算比例未公开，而余额接口
// （GET {opengw}/api/v1/user/tokens/balance）自 2026-10-22 起被上游关闭
// （error_code=4000 "invalid channel, group not found"），拿不到真实余额。
//
// 数据落盘到 data/state.json.usage.json，保留 30 天；内存聚合 + 延迟合并写盘，
// 不阻塞请求路径。
package server

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	usageRetentionDays = 30
	usageFlushDelay    = 2 * time.Second
)

// usageCounts 一组用量计数。
type usageCounts struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func (u *usageCounts) add(o usageCounts) {
	u.Requests += o.Requests
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.TotalTokens += o.TotalTokens
}

// usageRow 一行持久化记录（天+账号+模型 唯一）。
type usageRow struct {
	Day     string      `json:"day"`
	Account string      `json:"account"`
	Model   string      `json:"model"`
	Counts  usageCounts `json:"counts"`
}

// usageByKey 聚合输出（账号/模型/天 三个维度共用）。
type usageByKey struct {
	Key      string `json:"key"`
	Nickname string `json:"nickname,omitempty"`
	usageCounts
}

// usageReport 面板/接口返回的汇总。
type usageReport struct {
	Days      int          `json:"days"`
	Total     usageCounts  `json:"total"`
	Today     usageCounts  `json:"today"`
	ByAccount []usageByKey `json:"by_account"`
	ByModel   []usageByKey `json:"by_model"`
	ByDay     []usageByKey `json:"by_day"`
}

// usageStore 内存聚合 + 合并写盘。
type usageStore struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	rows  map[string]*usageRow // key: day\x00account\x00model
	dirty bool
	timer *time.Timer
}

func newUsageStore(path string) *usageStore {
	s := &usageStore{path: path, now: time.Now, rows: map[string]*usageRow{}}
	s.load()
	return s
}

func usageKey(day, account, model string) string { return day + "\x00" + account + "\x00" + model }

// add 记一次请求的用量（非阻塞，合并写盘）。
func (s *usageStore) add(account, model string, c usageCounts) {
	if s == nil {
		return
	}
	day := s.now().Format("2006-01-02")
	k := usageKey(day, account, model)
	s.mu.Lock()
	row, ok := s.rows[k]
	if !ok {
		row = &usageRow{Day: day, Account: account, Model: model}
		s.rows[k] = row
	}
	row.Counts.add(c)
	s.dirty = true
	if s.timer == nil {
		s.timer = time.AfterFunc(usageFlushDelay, func() {
			s.mu.Lock()
			s.timer = nil
			s.mu.Unlock()
			s.flush()
		})
	}
	s.mu.Unlock()
}

// flush 把内存数据写盘（保留 usageRetentionDays 天）。失败只记日志。
func (s *usageStore) flush() {
	if s == nil || s.path == "" {
		return
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	cutoff := s.now().AddDate(0, 0, -usageRetentionDays).Format("2006-01-02")
	out := make([]usageRow, 0, len(s.rows))
	for k, row := range s.rows {
		if row.Day < cutoff {
			delete(s.rows, k) // 过期天直接丢
			continue
		}
		out = append(out, *row)
	}
	s.dirty = false
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Model < out[j].Model
	})
	raw, err := json.MarshalIndent(map[string]any{"version": 1, "rows": out}, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("usage: write %s failed: %v", s.path, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("usage: rename %s failed: %v", s.path, err)
	}
}

func (s *usageStore) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var data struct {
		Rows []usageRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Printf("usage: %s 解析失败，忽略: %v", s.path, err)
		return
	}
	for _, row := range data.Rows {
		row := row
		s.rows[usageKey(row.Day, row.Account, row.Model)] = &row
	}
}

// snapshot 汇总最近 days 天（含今天）的用量。
func (s *usageStore) snapshot(days int) usageReport {
	rep := usageReport{Days: days, ByAccount: []usageByKey{}, ByModel: []usageByKey{}, ByDay: []usageByKey{}}
	if s == nil {
		return rep
	}
	if days <= 0 || days > usageRetentionDays {
		days = 7
	}
	rep.Days = days
	cutoff := s.now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	today := s.now().Format("2006-01-02")

	s.mu.Lock()
	byAccount := map[string]*usageCounts{}
	byModel := map[string]*usageCounts{}
	byDay := map[string]*usageCounts{}
	for _, row := range s.rows {
		if row.Day < cutoff {
			continue
		}
		rep.Total.add(row.Counts)
		if row.Day == today {
			rep.Today.add(row.Counts)
		}
		if byAccount[row.Account] == nil {
			byAccount[row.Account] = &usageCounts{}
		}
		byAccount[row.Account].add(row.Counts)
		if byModel[row.Model] == nil {
			byModel[row.Model] = &usageCounts{}
		}
		byModel[row.Model].add(row.Counts)
		if byDay[row.Day] == nil {
			byDay[row.Day] = &usageCounts{}
		}
		byDay[row.Day].add(row.Counts)
	}
	s.mu.Unlock()

	rep.ByAccount = sortedUsage(byAccount)
	rep.ByModel = sortedUsage(byModel)
	rep.ByDay = sortedUsage(byDay)
	return rep
}

// sortedUsage map → 按 total_tokens 降序的切片。
func sortedUsage(m map[string]*usageCounts) []usageByKey {
	out := make([]usageByKey, 0, len(m))
	for k, c := range m {
		out = append(out, usageByKey{Key: k, usageCounts: *c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// usageCountsFromMap 从 OpenAI 风格 usage map 里取值（数值可能是 float64 或 int）。
func usageCountsFromMap(m map[string]any) usageCounts {
	num := func(v any) int64 {
		switch t := v.(type) {
		case float64:
			return int64(t)
		case int64:
			return t
		case int:
			return int64(t)
		case json.Number:
			n, _ := t.Int64()
			return n
		}
		return 0
	}
	c := usageCounts{
		PromptTokens:     num(m["prompt_tokens"]),
		CompletionTokens: num(m["completion_tokens"]),
		TotalTokens:      num(m["total_tokens"]),
	}
	if d, ok := m["completion_tokens_details"].(map[string]any); ok {
		c.ReasoningTokens = num(d["reasoning_tokens"])
	}
	if c.ReasoningTokens == 0 {
		c.ReasoningTokens = num(m["reasoning_tokens"])
	}
	if c.TotalTokens == 0 {
		c.TotalTokens = c.PromptTokens + c.CompletionTokens
	}
	return c
}
