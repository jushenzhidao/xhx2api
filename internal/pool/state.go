// state.go 账号池运行时状态与持久化。
//
// state.json 存的是「重启后必须记住」的东西：冷却截止、熔断计数、累计用量、
// 最近错误。不存 token（那是 auths/ 的职责）——两套数据分开，账号文件可以
// 单独拷走，状态文件可以单独删掉重置统计。
package pool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State 是单账号的运行时状态。字段全部可序列化，InFlight 除外（重启后归零）。
type State struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	// Priority 供 priority 策略使用；数值越小越优先。默认 100。
	Priority int `json:"priority,omitempty"`

	Disabled bool `json:"disabled,omitempty"`
	Paused   bool `json:"paused,omitempty"`

	// CooldownUntil 是冷却截止（unix 秒）。软冷却/额度冷却/登录失效共用这个字段，
	// 具体原因放在 CooldownKind 里。
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	CooldownKind  string `json:"cooldown_kind,omitempty"`
	CooldownNote  string `json:"cooldown_note,omitempty"`

	// BreakerUntil 是熔断截止（unix 秒）；与 CooldownUntil 分开是因为熔断的
	// 语义是「上游持续异常」，面板要能单独显示「熔断中」而不是「冷却中」。
	BreakerUntil int64 `json:"breaker_until,omitempty"`
	DegradeUntil int64 `json:"degrade_until,omitempty"`

	FailCount     int `json:"fail_count,omitempty"`
	SoftRateLevel int `json:"soft_rate_level,omitempty"`
	SessionDead   int `json:"session_dead,omitempty"`

	// InFlight 是当前在途请求数；不落盘（进程重启后所有在途都随旧进程消失）。
	InFlight int `json:"-"`

	Credits     int64  `json:"credits,omitempty"`
	CreditAt    string `json:"credit_at,omitempty"`
	CheckedInAt string `json:"checked_in,omitempty"`

	LastError   string `json:"last_error,omitempty"`
	LastUsed    int64  `json:"last_used,omitempty"`
	LastSuccess int64  `json:"last_success,omitempty"`

	Total        int64 `json:"total,omitempty"`
	Success      int64 `json:"success,omitempty"`
	Fail         int64 `json:"fail,omitempty"`
	AvgLatencyMS int64 `json:"avg_latency_ms,omitempty"`
}

// snapshot 是 state.json 的顶层形状。
type snapshot struct {
	SavedAt time.Time `json:"saved_at"`
	Pool    []*State  `json:"pool"`
}

// Restore 从 state_file 恢复状态。
//
// 恢复规则：
//   - 已不存在的账号直接丢弃（auths/ 里删掉的号不该在状态文件里阴魂不散）；
//   - 冷却时刻已过期的清掉（重启时顺手做过期清理，省一轮定时任务）；
//   - 熔断/降权同理。
func (p *Pool) Restore(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	p.mu.Lock()
	defer p.mu.Unlock()
	restored := 0
	for _, st := range snap.Pool {
		if st == nil || st.UID == "" {
			continue
		}
		if _, ok := p.auths[st.UID]; !ok {
			continue
		}
		if st.CooldownUntil <= now {
			st.CooldownUntil, st.CooldownKind, st.CooldownNote = 0, "", ""
		}
		if st.BreakerUntil <= now {
			st.BreakerUntil = 0
		}
		if st.DegradeUntil <= now {
			st.DegradeUntil = 0
		}
		st.InFlight = 0
		if st.Priority <= 0 {
			st.Priority = 100
		}
		p.states[st.UID] = st
		// 账号文件里的积分/签到是更权威的落点（用户可能手工改过）。
		if a := p.auths[st.UID]; a != nil {
			snapAuth := a.Snapshot()
			if snapAuth.Credit > 0 {
				st.Credits = snapAuth.Credit
			}
			if snapAuth.CheckedIn != "" {
				st.CheckedInAt = snapAuth.CheckedIn
			}
		}
		restored++
	}
	p.dirtyMu.Lock()
	p.dirty = false
	p.dirtyMu.Unlock()
	return restored, nil
}

// markDirtyLocked 标记状态有变化（调用方必须已持锁）。
func (p *Pool) markDirtyLocked() {
	p.dirtyMu.Lock()
	p.dirty = true
	p.dirtyMu.Unlock()
}

// takeDirty 取走 dirty 标记。
func (p *Pool) takeDirty() bool {
	p.dirtyMu.Lock()
	defer p.dirtyMu.Unlock()
	was := p.dirty
	p.dirty = false
	return was
}

// flushMu 串行化落盘：退出信号与周期 flusher 可能同时调 Flush，两个写
// 同一个 `state.json.tmp` 会互相覆盖，后一个 rename 失败还会留下残留文件。
var flushMu sync.Mutex

// Flush 立即把状态写盘（原子替换）。
//
// 返回写盘错误但**不清理** dirty 标记的语义是刻意的：下一次 Flush 会重试，
// 调用方（定时器）不需要自己维护重试逻辑。
func (p *Pool) Flush() error {
	flushMu.Lock()
	defer flushMu.Unlock()
	path := p.Config().StateFile
	if path == "" {
		p.takeDirty()
		return nil
	}
	p.mu.RLock()
	snap := snapshot{SavedAt: time.Now(), Pool: make([]*State, 0, len(p.order))}
	for _, uid := range p.order {
		st := p.states[uid]
		if st == nil {
			continue
		}
		cp := *st
		cp.InFlight = 0
		snap.Pool = append(snap.Pool, &cp)
	}
	p.mu.RUnlock()
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	p.takeDirty()
	return nil
}

// StartFlusher 起一个周期落盘的后台任务（脏才写）。
//
// ctx 取消时**不**在这里做最后一次 Flush：停机流程（main）已经统一落盘一次，
// 两处同时写会撞同一个临时文件，且后台协程可能写到一半进程就退出了，
// 留下 0 字节的 state.json.tmp。
func (p *Pool) StartFlusher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if p.takeDirtyPeek() {
					_ = p.Flush()
				}
			}
		}
	}()
}

// takeDirtyPeek 只在脏时置回并返回 true（不消费标记，交给 Flush）。
func (p *Pool) takeDirtyPeek() bool {
	p.dirtyMu.Lock()
	defer p.dirtyMu.Unlock()
	return p.dirty
}

// SetPriority 设置账号优先级（面板可调）。
func (p *Pool) SetPriority(uid string, priority int) bool {
	if priority <= 0 {
		priority = 100
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[uid]
	if st == nil {
		return false
	}
	st.Priority = priority
	p.markDirtyLocked()
	return true
}

// Update 在持锁状态下修改状态（面板/调度器用，避免直接暴露 State 指针）。
func (p *Pool) Update(uid string, fn func(*State)) bool {
	if fn == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[uid]
	if st == nil {
		return false
	}
	fn(st)
	p.markDirtyLocked()
	return true
}

// Get 返回状态副本。
func (p *Pool) Get(uid string) (State, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st := p.states[uid]
	if st == nil {
		return State{}, false
	}
	cp := *st
	return cp, true
}
