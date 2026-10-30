// Package scheduler 跑账号的周期任务：每日签到、token 保活、余额刷新。
//
// 几条来自上游实测的结论，直接决定了这里的判定口径：
//
//  1. **每日 +300 是服务端 00:00（东八区）自动发的**，不存在「调接口就发分」的
//     客户端开关。grant 接口是登录弹窗式的手动领取，领过回 granted:false。
//     所以「签到成功」以 bills 流水里当天有 daily_grant 为准，不是看返回码。
//  2. **新号当天没有 daily_grant 是正常的**：发放是定时任务，当天注册的号要等
//     次日首批。报「签到失败」是谎报，要单独识别成「待发放」。
//  3. **有的号压根没有每日积分资格**（bills 里从来只有 reward_grant）。这种号
//     再怎么领也不会有，同样不该记成失败。
//  4. **保活只续期，不做任何业务动作**。上游对非官方客户端的识别有一部分来自
//     行为模式，保活越「像人」越好——只续 token 是最小足迹。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// Config 是调度参数（面板可热改）。
type Config struct {
	CheckinHours     []int
	KeepaliveHours   []int
	CheckinEnabled   bool
	KeepaliveEnabled bool
	// BalanceRefreshMinutes 余额刷新周期（分钟），<=0 关闭。
	BalanceRefreshMinutes int
	// IncludeDisabled 让保号类任务对已禁用账号也执行。
	// 语义：面板「禁用」= 不参与选号，不等于「停止保号」——被禁用的号仍然需要
	// 签到与续期，否则解禁时额度已清零、token 已过期。
	IncludeDisabled bool
	// NewAccountGraceDays 新号静默期（天）：进池 N 天内的号只做保号动作。
	NewAccountGraceDays int
	// Concurrency 单轮任务并发账号数（默认 2，避免同一出口瞬间打多个账号）。
	Concurrency int
}

// Scheduler 是周期任务执行器。
type Scheduler struct {
	pool *pool.Pool
	up   *upstream.Client

	mu  sync.RWMutex
	cfg Config

	// lastRun 记录每个 job 最近一次执行的「日期+小时」键，避免同一小时重复跑。
	runMu   sync.Mutex
	lastRun map[string]string

	// mobileOnce 记录已尝试过手机端首登奖励的账号（上游幂等，进程内记忆即可）。
	mobileMu   sync.Mutex
	mobileDone map[string]bool

	// 最近一次执行结果（面板展示）。
	resultMu sync.RWMutex
	results  map[string]JobResult
}

// JobResult 是一次任务执行的汇总。
type JobResult struct {
	Job       string    `json:"job"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	OK        int       `json:"ok"`
	Failed    int       `json:"failed"`
	Skipped   int       `json:"skipped"`
	Details   []string  `json:"details,omitempty"`
}

// New 建调度器。
func New(p *pool.Pool, up *upstream.Client, cfg Config) *Scheduler {
	cfg = normalize(cfg)
	return &Scheduler{
		pool: p, up: up, cfg: cfg,
		lastRun:    map[string]string{},
		mobileDone: map[string]bool{},
		results:    map[string]JobResult{},
	}
}

func normalize(cfg Config) Config {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{3}
	}
	if cfg.BalanceRefreshMinutes <= 0 {
		cfg.BalanceRefreshMinutes = 5
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	return cfg
}

// Configure 热更新参数。
func (s *Scheduler) Configure(fn func(*Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	s.cfg = normalize(s.cfg)
}

// Config 返回当前参数。
func (s *Scheduler) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Run 起主循环（每分钟检查一次时点）。
func (s *Scheduler) Run(ctx context.Context) {
	s.runDueJobs(ctx, time.Now())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.runDueJobs(ctx, now)
		}
	}
}

// StartBalanceRefresh 起独立的余额刷新循环（分钟级，不受「小时」约束）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context) {
	// 启动后先等 20 秒：让服务先开始接流量，避免启动瞬间打一堆上游请求。
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	s.RefreshBalances(ctx)
	for {
		interval := time.Duration(s.Config().BalanceRefreshMinutes) * time.Minute
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			s.RefreshBalances(ctx)
		}
	}
}

// runDueJobs 检查当前时点该跑哪些任务。
func (s *Scheduler) runDueJobs(ctx context.Context, now time.Time) {
	cfg := s.Config()
	hourKey := now.In(shanghai()).Format("2006-01-02T15")
	hour := now.In(shanghai()).Hour()

	if cfg.CheckinEnabled && containsInt(cfg.CheckinHours, hour) && s.markIfNew("checkin", hourKey) {
		go s.checkinAll(ctx)
	}
	if cfg.KeepaliveEnabled && containsInt(cfg.KeepaliveHours, hour) && s.markIfNew("keepalive", hourKey) {
		go s.keepaliveAll(ctx)
	}
}

// markIfNew 同一次「小时键」只允许跑一次；返回 true 表示本次应当执行。
func (s *Scheduler) markIfNew(job, key string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.lastRun[job] == key {
		return false
	}
	s.lastRun[job] = key
	return true
}

// ---------------------------------------------------------------------------
// 每日签到

// CheckinOutcome 是单账号签到结果。
type CheckinOutcome struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Status   string `json:"status"` // ok / already / pending / ineligible / failed
	Credits  int64  `json:"credits"`
	Message  string `json:"message"`
}

// CheckinAccount 对单个账号执行签到。
//
// 返回的 Status：
//
//	already    今天已有 daily_grant（最理想，不用做任何事）
//	ok         本次 claim 后确认到账
//	pending    新号/上游还没发，今天再等等
//	ineligible 上游从未给它发过每日积分
//	failed     真的失败了
func (s *Scheduler) CheckinAccount(ctx context.Context, a *auth.Auth) CheckinOutcome {
	out := CheckinOutcome{UID: a.UID, Nickname: a.Snapshot().Nickname}
	checked, credits, err := s.up.CheckinStatus(ctx, a)
	if err != nil {
		out.Status = "failed"
		out.Message = err.Error()
		return out
	}
	out.Credits = credits
	s.pool.NoteCredits(a.UID, credits)
	if checked {
		out.Status = "already"
		out.Message = "今日每日积分已到账"
		_ = a.SaveAtomic()
		return out
	}
	// 未到账：先补一次登录领取（幂等），再以流水复核。
	granted, claimErr := s.up.CheckinClaim(ctx, a)
	today, ever, billErr := s.up.DailyGrant(ctx, a)
	if billErr == nil && today {
		// 流水确认到账就记「今天已签」：granted=false 只说明本次领取是空操作，
		// 不影响「今日已发放」这个事实（漏记会让面板一直显示未签）。
		a.MarkCheckedIn()
		out.Status = "ok"
		if granted {
			out.Message = "本次领取成功，流水已确认到账"
		} else {
			out.Message = "上游今日已发放（本次领取为幂等空操作）"
		}
		_ = a.SaveAtomic()
		return out
	}
	if claimErr != nil && billErr != nil {
		out.Status = "failed"
		out.Message = fmt.Sprintf("领取失败：%v；流水查询也失败：%v", claimErr, billErr)
		return out
	}
	// 今天注册的号：发放是次日 00:00 的首批，此刻 bills 里必然一条 daily_grant 都没有。
	// 这条必须排在「从来没有」前面——否则刚注册几小时的号会被报成 ineligible，
	// 那句「历史上从未获得每日积分」对它就是谎报（实测踩过：手机号新号报 ineligible）。
	if reg := s.registerTime(ctx, a); !reg.IsZero() &&
		reg.In(shanghai()).Format("2006-01-02") == time.Now().In(shanghai()).Format("2006-01-02") {
		out.Status = "pending"
		out.Message = "账号今天注册，每日积分次日 00:00 首批发放"
		return out
	}
	if billErr == nil && !ever {
		// 注册已过一天仍从未有过 daily_grant：这个号确实没有每日积分资格（不是失败）。
		out.Status = "ineligible"
		out.Message = "该账号历史上从未获得每日积分（上游未开通），无需签到"
		return out
	}
	out.Status = "pending"
	out.Message = "上游今日尚未发放（服务端 00:00 定时任务），稍后再试"
	return out
}

// registerTime 取账号的上游注册时间。
//
// 凭证里通常没有这个字段（裸 token 导入、手机号登录都不带 register_at），
// 这时向上游 user_info 问一次并落盘——只问一次，之后走本地缓存。
// 拿不到就返回零值，调用方按「老号」处理（宁可报 ineligible 也不谎报 pending）。
func (s *Scheduler) registerTime(ctx context.Context, a *auth.Auth) time.Time {
	if reg := a.RegisterTime(); !reg.IsZero() {
		return reg
	}
	t, ok := s.up.UserCreatedAt(ctx, a)
	if !ok {
		return time.Time{}
	}
	a.SetRegisterAt(t)
	_ = a.SaveAtomic()
	return t
}

// checkinAll 对全池账号跑一轮签到。
func (s *Scheduler) checkinAll(ctx context.Context) JobResult {
	started := time.Now()
	result := JobResult{Job: "checkin", StartedAt: started}
	uids := s.pool.UIDs()
	cfg := s.Config()
	run := make(chan string, len(uids))
	for _, uid := range uids {
		run <- uid
	}
	close(run)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for uid := range run {
				if ctx.Err() != nil {
					return
				}
				entry, ok := s.pool.Get(uid)
				if !ok {
					continue
				}
				if entry.Disabled && !cfg.IncludeDisabled {
					mu.Lock()
					result.Skipped++
					mu.Unlock()
					continue
				}
				a := s.pool.AuthByUID(uid)
				if a == nil {
					continue
				}
				outcome := s.CheckinAccount(ctx, a)
				mu.Lock()
				switch outcome.Status {
				case "ok", "already":
					result.OK++
					s.pool.NoteCheckedIn(uid, time.Now())
				case "pending", "ineligible":
					result.Skipped++
				default:
					result.Failed++
				}
				result.Details = append(result.Details,
					fmt.Sprintf("%s(%s): %s — %s", displayName(outcome), uid, outcome.Status, outcome.Message))
				mu.Unlock()
				// 顺手领手机端首登奖励（每号只试一次）。
				s.tryMobileGrant(ctx, a)
			}
		}()
	}
	wg.Wait()
	result.EndedAt = time.Now()
	s.recordResult(result)
	log.Printf("[scheduler] 签到完成：成功 %d / 跳过 %d / 失败 %d（%s）",
		result.OK, result.Skipped, result.Failed, result.EndedAt.Sub(result.StartedAt).Round(time.Second))
	return result
}

// tryMobileGrant 领手机端首登奖励（与桌面端额度独立的一笔）。
func (s *Scheduler) tryMobileGrant(ctx context.Context, a *auth.Auth) {
	s.mobileMu.Lock()
	if s.mobileDone[a.UID] {
		s.mobileMu.Unlock()
		return
	}
	s.mobileDone[a.UID] = true
	s.mobileMu.Unlock()
	if _, err := s.up.ClaimMobileGrant(ctx, a); err != nil {
		log.Printf("[scheduler] 手机端首登奖励 %s: %v", a.UID, err)
	}
}

// CheckinAll 供面板手动触发。
func (s *Scheduler) CheckinAll(ctx context.Context) JobResult { return s.checkinAll(ctx) }

// CheckinOne 供面板手动触发单号签到。
func (s *Scheduler) CheckinOne(ctx context.Context, uid string) (CheckinOutcome, error) {
	a := s.pool.AuthByUID(uid)
	if a == nil {
		return CheckinOutcome{}, fmt.Errorf("账号 %s 不存在", uid)
	}
	out := s.CheckinAccount(ctx, a)
	if out.Status == "ok" || out.Status == "already" {
		s.pool.NoteCheckedIn(uid, time.Now())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 保活

// keepaliveAll 给所有账号做一次「临期就续期」。
func (s *Scheduler) keepaliveAll(ctx context.Context) JobResult {
	started := time.Now()
	result := JobResult{Job: "keepalive", StartedAt: started}
	cfg := s.Config()
	for _, uid := range s.pool.UIDs() {
		if ctx.Err() != nil {
			break
		}
		entry, ok := s.pool.Get(uid)
		if !ok {
			continue
		}
		if entry.Disabled && !cfg.IncludeDisabled {
			result.Skipped++
			continue
		}
		a := s.pool.AuthByUID(uid)
		if a == nil || !a.CanRefresh() {
			result.Skipped++
			continue
		}
		// 保活窗口放宽到 6 小时：比请求路径的 10 分钟激进得多，避免每次请求都要刷。
		refreshed, err := s.up.RefreshTokenIfNeeded(ctx, a, 6*time.Hour)
		if err != nil {
			result.Failed++
			result.Details = append(result.Details, fmt.Sprintf("%s: %v", uid, err))
			s.pool.Update(uid, func(st *pool.State) { st.LastError = "保活续期失败: " + err.Error() })
			continue
		}
		if refreshed {
			result.OK++
		} else {
			result.Skipped++
		}
	}
	result.EndedAt = time.Now()
	s.recordResult(result)
	if result.OK > 0 || result.Failed > 0 {
		log.Printf("[scheduler] 保活完成：续期 %d / 跳过 %d / 失败 %d", result.OK, result.Skipped, result.Failed)
	}
	return result
}

// KeepaliveAll 供面板手动触发。
func (s *Scheduler) KeepaliveAll(ctx context.Context) JobResult { return s.keepaliveAll(ctx) }

// ---------------------------------------------------------------------------
// 余额刷新

// RefreshBalances 刷新全池积分，并在「余额恢复」时解冻额度冷却。
func (s *Scheduler) RefreshBalances(ctx context.Context) JobResult {
	started := time.Now()
	result := JobResult{Job: "balance", StartedAt: started}
	cfg := s.Config()
	for _, uid := range s.pool.UIDs() {
		if ctx.Err() != nil {
			break
		}
		entry, ok := s.pool.Get(uid)
		if !ok {
			continue
		}
		if entry.Disabled && !cfg.IncludeDisabled {
			result.Skipped++
			continue
		}
		a := s.pool.AuthByUID(uid)
		if a == nil {
			continue
		}
		usage, err := s.up.UserEntUsageDetail(ctx, a)
		if err != nil {
			result.Failed++
			result.Details = append(result.Details, fmt.Sprintf("%s: %v", uid, err))
			continue
		}
		result.OK++
		s.pool.NoteCredits(uid, usage.Remain)
		s.pool.Update(uid, func(st *pool.State) {
			st.CreditAt = time.Now().Format(time.RFC3339)
		})
		_ = a.SaveAtomic()
		// 额度冷却 + 余额恢复 → 自动解冻（但不动人工禁用）。
		if usage.Remain > 0 && entry.CooldownKind == "plan_limit" && !entry.Disabled {
			s.pool.Revive(uid)
			log.Printf("[scheduler] %s 余额恢复为 %d，已解除额度冷却", uid, usage.Remain)
		}
	}
	result.EndedAt = time.Now()
	if result.Failed > 0 {
		log.Printf("[scheduler] 余额刷新：成功 %d / 失败 %d", result.OK, result.Failed)
	}
	s.recordResult(result)
	return result
}

// RefreshOne 刷新单号余额（面板调用）。
func (s *Scheduler) RefreshOne(ctx context.Context, uid string) (upstream.EntUsage, error) {
	a := s.pool.AuthByUID(uid)
	if a == nil {
		return upstream.EntUsage{}, fmt.Errorf("账号 %s 不存在", uid)
	}
	usage, err := s.up.UserEntUsageDetailWithExpiry(ctx, a)
	if err != nil {
		return usage, err
	}
	s.pool.NoteCredits(uid, usage.Remain)
	s.pool.Update(uid, func(st *pool.State) { st.CreditAt = time.Now().Format(time.RFC3339) })
	_ = a.SaveAtomic()
	return usage, nil
}

// ---------------------------------------------------------------------------
// 结果记录

func (s *Scheduler) recordResult(result JobResult) {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	if s.results == nil {
		s.results = map[string]JobResult{}
	}
	s.results[result.Job] = result
}

// Results 返回最近结果。
func (s *Scheduler) Results() map[string]JobResult {
	s.resultMu.RLock()
	defer s.resultMu.RUnlock()
	out := make(map[string]JobResult, len(s.results))
	for k, v := range s.results {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// 辅助

func shanghai() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

func containsInt(list []int, v int) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func displayName(o CheckinOutcome) string {
	if o.Nickname != "" {
		return o.Nickname
	}
	return o.UID
}
