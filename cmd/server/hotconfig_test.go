package main

import (
	"testing"

	"github.com/rsfree/xhx2api/internal/livecfg"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/reqlog"
	"github.com/rsfree/xhx2api/internal/scheduler"
	"github.com/rsfree/xhx2api/internal/upstream"
	"github.com/rsfree/xhx2api/internal/usage"
)

// 保存配置后必须把 schedule 段推给调度器。
//
// 回归的是 2026-11-02 线上踩到的「保存成功但不生效」：applyHotConfig 里
// 只热更新了 key/策略/冷却，完全没碰 scheduler，于是面板上改签到时点、
// 并发、余额刷新周期全部要重启才生效——界面还提示「已保存」，用户会以为
// 是配置写错了或者程序有 bug，排半天。
func TestApplyHotConfigPushesSchedule(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{})
	p := pool.New(pool.Config{})
	up := upstream.New()
	rec := usage.New("")
	requestLog := reqlog.New(reqlog.Config{})
	sch := scheduler.New(p, up, scheduler.Config{
		CheckinHours: []int{9}, KeepaliveHours: []int{3},
		CheckinEnabled: true, KeepaliveEnabled: true,
		BalanceRefreshEnabled: true, BalanceRefreshMinutes: 5,
		Concurrency: 2, NewAccountGraceDays: 3,
	})

	c := &Config{}
	c.Schedule.CheckinHours = []int{7, 19}
	c.Schedule.KeepaliveHours = []int{4}
	c.Schedule.CheckinEnabled = false
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BalanceRefreshEnabled = false
	c.Schedule.BalanceRefreshMinutes = 90
	c.Schedule.Concurrency = 6
	c.Schedule.NewAccountGraceDays = 7
	c.Schedule.IncludeDisabledInTasks = true

	applyHotConfig(c, live, p, up, rec, requestLog, sch)

	got := sch.Config()
	if got.CheckinEnabled {
		t.Error("签到时点开关没热更新")
	}
	if len(got.CheckinHours) != 2 || got.CheckinHours[0] != 7 {
		t.Errorf("签到时点没热更新: %v", got.CheckinHours)
	}
	if len(got.KeepaliveHours) != 1 || got.KeepaliveHours[0] != 4 {
		t.Errorf("保活时点没热更新: %v", got.KeepaliveHours)
	}
	if got.BalanceRefreshEnabled {
		t.Error("余额刷新开关没热更新")
	}
	if got.BalanceRefreshMinutes != 90 {
		t.Errorf("余额刷新周期没热更新: %d", got.BalanceRefreshMinutes)
	}
	if got.Concurrency != 6 {
		t.Errorf("并发没热更新: %d", got.Concurrency)
	}
	if got.NewAccountGraceDays != 7 || !got.IncludeDisabled {
		t.Errorf("静默期/含已禁用没热更新: %d %v", got.NewAccountGraceDays, got.IncludeDisabled)
	}
}

// 清空小时列表不能把用户刚删的设置又补回默认值。
func TestApplyHotConfigKeepsHoursWhenCleared(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{})
	p := pool.New(pool.Config{})
	up := upstream.New()
	sch := scheduler.New(p, up, scheduler.Config{
		CheckinHours: []int{9}, KeepaliveHours: []int{3},
		CheckinEnabled: true, KeepaliveEnabled: true,
		BalanceRefreshMinutes: 5, Concurrency: 2,
	})

	c := &Config{} // 小时列表为空
	applyHotConfig(c, live, p, up, usage.New(""), reqlog.New(reqlog.Config{}), sch)

	if got := sch.Config(); len(got.CheckinHours) != 1 || got.CheckinHours[0] != 9 {
		t.Errorf("空列表应保留原值 9，实得 %v", got.CheckinHours)
	}
}
