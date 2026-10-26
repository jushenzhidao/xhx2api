// Package usage 记录逐请求用量：token 数、耗时、账号归属与协议。
//
// 小浣熊按**积分**计费，而 token 数与积分之间没有公开的换算表（上游只在账单里
// 给结果）。所以这里只记 token 与请求数，**不编造金额**——把「估算的人民币」
// 写进报表会让用户拿它去对账，误差全算在网关头上。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Event 是一次请求的用量记录。
type Event struct {
	Time       time.Time `json:"time"`
	UID        string    `json:"uid,omitempty"`
	Account    string    `json:"account,omitempty"`
	Model      string    `json:"model,omitempty"`
	Protocol   string    `json:"protocol,omitempty"` // chat / messages / responses
	Stream     bool      `json:"stream,omitempty"`
	Status     int       `json:"status"`
	ErrorKind  string    `json:"error_kind,omitempty"`
	Prompt     int64     `json:"prompt_tokens"`
	Completion int64     `json:"completion_tokens"`
	Total      int64     `json:"total_tokens"`
	DurationMS int64     `json:"duration_ms"`
	TTFBMS     int64     `json:"ttfb_ms,omitempty"`
	Attempts   int       `json:"attempts,omitempty"`
}

// DayBucket 是按日聚合。
type DayBucket struct {
	Date       string `json:"date"`
	Requests   int64  `json:"requests"`
	Failures   int64  `json:"failures"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Total      int64  `json:"total_tokens"`
}

// AccountStat 是按账号聚合。
type AccountStat struct {
	UID        string `json:"uid"`
	Account    string `json:"account,omitempty"`
	Requests   int64  `json:"requests"`
	Failures   int64  `json:"failures"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Total      int64  `json:"total_tokens"`
	LastUsed   string `json:"last_used,omitempty"`
}

// Recorder 是并发安全的用量记录器。
type Recorder struct {
	mu   sync.RWMutex
	path string
	// lifetime 是进程启动以来的累计（面板顶部大数字）。
	lifetime AccountStat
	// accounts 按 uid 聚合。
	accounts map[string]*AccountStat
	// days 按日期聚合（保留 62 天，够看两个月趋势）。
	days map[string]*DayBucket
	// recent 是最近 200 条明细（面板「用量明细」）。
	recent []Event
	dirty  bool
}

const (
	maxRecent = 200
	maxDays   = 62
)

// New 建记录器；path 为空 = 不落盘（测试/裸用）。
func New(path string) *Recorder {
	r := &Recorder{path: path, accounts: map[string]*AccountStat{}, days: map[string]*DayBucket{}}
	if path != "" {
		_ = r.load()
	}
	return r
}

// Record 记一笔。并发安全；落盘由 Flush 周期做。
func (r *Recorder) Record(ev Event) {
	if r == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lifetime.Requests++
	r.lifetime.Prompt += ev.Prompt
	r.lifetime.Completion += ev.Completion
	r.lifetime.Total += ev.Total
	if ev.Status >= 400 || ev.ErrorKind != "" {
		r.lifetime.Failures++
	}
	if ev.UID != "" {
		stat := r.accounts[ev.UID]
		if stat == nil {
			stat = &AccountStat{UID: ev.UID, Account: ev.Account}
			r.accounts[ev.UID] = stat
		}
		stat.Requests++
		stat.Prompt += ev.Prompt
		stat.Completion += ev.Completion
		stat.Total += ev.Total
		stat.LastUsed = ev.Time.Format(time.RFC3339)
		if ev.Status >= 400 || ev.ErrorKind != "" {
			stat.Failures++
		}
	}
	day := ev.Time.Format("2006-01-02")
	bucket := r.days[day]
	if bucket == nil {
		bucket = &DayBucket{Date: day}
		r.days[day] = bucket
		r.pruneDaysLocked()
	}
	bucket.Requests++
	bucket.Prompt += ev.Prompt
	bucket.Completion += ev.Completion
	bucket.Total += ev.Total
	if ev.Status >= 400 || ev.ErrorKind != "" {
		bucket.Failures++
	}
	r.recent = append(r.recent, ev)
	if len(r.recent) > maxRecent {
		r.recent = r.recent[len(r.recent)-maxRecent:]
	}
	r.dirty = true
}

// pruneDaysLocked 只留最近 maxDays 天（按日期字符串排序即时间序）。
func (r *Recorder) pruneDaysLocked() {
	if len(r.days) <= maxDays {
		return
	}
	keys := make([]string, 0, len(r.days))
	for k := range r.days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys[:len(keys)-maxDays] {
		delete(r.days, k)
	}
}

// Snapshot 是面板用的整体视图。
type Snapshot struct {
	Lifetime AccountStat   `json:"lifetime"`
	Accounts []AccountStat `json:"accounts"`
	Days     []DayBucket   `json:"days"`
	Recent   []Event       `json:"recent"`
}

// Snapshot 取当前快照（按请求数降序）。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := Snapshot{Lifetime: r.lifetime}
	for _, stat := range r.accounts {
		out.Accounts = append(out.Accounts, *stat)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].Requests > out.Accounts[j].Requests })
	for _, d := range r.days {
		out.Days = append(out.Days, *d)
	}
	sort.Slice(out.Days, func(i, j int) bool { return out.Days[i].Date < out.Days[j].Date })
	out.Recent = append(out.Recent, r.recent...)
	if len(out.Recent) > maxRecent {
		out.Recent = out.Recent[len(out.Recent)-maxRecent:]
	}
	return out
}

// Reset 清空统计（面板「重置用量」）。
func (r *Recorder) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.lifetime = AccountStat{}
	r.accounts = map[string]*AccountStat{}
	r.days = map[string]*DayBucket{}
	r.recent = nil
	r.dirty = true
	r.mu.Unlock()
	_ = r.Flush()
}

// flushFile 是落盘结构。
type flushFile struct {
	SavedAt  time.Time               `json:"saved_at"`
	Lifetime AccountStat             `json:"lifetime"`
	Accounts map[string]*AccountStat `json:"accounts"`
	Days     map[string]*DayBucket   `json:"days"`
}

// flushMu 串行化落盘（退出信号与周期 flusher 可能同时进来）。
var flushMu sync.Mutex

// Flush 落盘（脏才写）。
func (r *Recorder) Flush() error {
	if r == nil || r.path == "" {
		return nil
	}
	flushMu.Lock()
	defer flushMu.Unlock()
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	payload := flushFile{SavedAt: time.Now(), Lifetime: r.lifetime, Accounts: r.accounts, Days: r.days}
	r.dirty = false
	r.mu.Unlock()
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// load 从磁盘恢复累计。
func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	var payload flushFile
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	r.lifetime = payload.Lifetime
	if payload.Accounts != nil {
		r.accounts = payload.Accounts
	}
	if payload.Days != nil {
		r.days = payload.Days
	}
	return nil
}

// StartFlusher 周期落盘。
func (r *Recorder) StartFlusher(stop <-chan struct{}, interval time.Duration) {
	if r == nil || r.path == "" {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				// 同 pool.StartFlusher：最后一次落盘由停机流程负责。
				return
			case <-ticker.C:
				_ = r.Flush()
			}
		}
	}()
}
