package pool

import (
	"context"
	"testing"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

func newTestPool(t *testing.T, uids ...string) *Pool {
	t.Helper()
	p := New(Config{StateFile: t.TempDir() + "/state.json"})
	var auths []*auth.Auth
	for _, uid := range uids {
		auths = append(auths, &auth.Auth{Data: auth.Data{UID: uid, Nickname: uid, AccessToken: "tok"}})
	}
	p.SyncToDir(auths)
	return p
}

func TestPickRoundRobin(t *testing.T) {
	p := newTestPool(t, "a", "b", "c")
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		lease, err := p.Pick(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen[lease.UID]++
		lease.ReleaseSuccess(0)
	}
	for _, uid := range []string{"a", "b", "c"} {
		if seen[uid] != 2 {
			t.Fatalf("uid %s 被选中 %d 次，期望 2（轮转不均）: %v", uid, seen[uid], seen)
		}
	}
}

func TestPickExcludesFailed(t *testing.T) {
	p := newTestPool(t, "a", "b")
	exclude := map[string]bool{"a": true}
	for i := 0; i < 4; i++ {
		lease, err := p.Pick(exclude, 0)
		if err != nil {
			t.Fatal(err)
		}
		if lease.UID == "a" {
			t.Fatal("被排除的账号仍被选中")
		}
		lease.ReleaseSuccess(0)
	}
}

func TestNoAccount(t *testing.T) {
	p := newTestPool(t)
	if _, err := p.Pick(nil, 0); err != ErrNoAccount {
		t.Fatalf("空池应返回 ErrNoAccount, got %v", err)
	}
}

func TestPlanLimitCooldownThenRevive(t *testing.T) {
	p := newTestPool(t, "a")
	lease, err := p.Pick(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kind := "plan_limit"
	lease.Release(&kind, "额度不足", time.Second)

	if _, err := p.Pick(nil, 0); err == nil {
		t.Fatal("额度冷却期间不该选到该账号")
	}
	entry := p.List()[0]
	if entry.CooldownKind != "plan_limit" || entry.CooldownUntil == 0 {
		t.Fatalf("冷却状态没落上: %+v", entry)
	}
	// 冷却不改变「可用账号」的语义，只是让 Pick 跳过。
	if counts := p.Counts(); counts.Cooling != 1 || counts.Healthy != 0 {
		t.Fatalf("Counts = %+v", counts)
	}
	// 人工解冻后恢复。
	if !p.Revive("a") {
		t.Fatal("Revive 失败")
	}
	if _, err := p.Pick(nil, 0); err != nil {
		t.Fatalf("解冻后应当可选: %v", err)
	}
}

func TestSoftRateBackoff(t *testing.T) {
	p := newTestPool(t, "a")
	p.Configure(func(c *Config) { c.SoftRateBase = time.Second; c.SoftRateMax = 4 * time.Second })
	for i := 0; i < 3; i++ {
		lease, err := p.Pick(nil, 0)
		if err != nil {
			t.Fatalf("第 %d 次 Pick: %v", i, err)
		}
		kind := "soft_rate"
		lease.Release(&kind, "429", 0)
		// 退避是 1s→2s→4s，测试里不等真实时间：直接清掉冷却继续。
		p.Update("a", func(st *State) { st.CooldownUntil = 0 })
	}
	st, _ := p.Get("a")
	if st.SoftRateLevel < 2 {
		t.Fatalf("软冷却等级没有递增: %d", st.SoftRateLevel)
	}
	// 成功一次要清零退避等级。
	lease, err := p.Pick(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	lease.ReleaseSuccess(time.Millisecond)
	st, _ = p.Get("a")
	if st.SoftRateLevel != 0 || st.CooldownUntil != 0 {
		t.Fatalf("成功后没清零: %+v", st)
	}
}

func TestSessionDeadDisablesAfterThree(t *testing.T) {
	p := newTestPool(t, "a")
	p.Configure(func(c *Config) { c.SoftRateBase = time.Second })
	for i := 0; i < 3; i++ {
		lease, err := p.Pick(nil, 0)
		if err != nil {
			t.Fatalf("第 %d 次 Pick: %v", i, err)
		}
		kind := "session_dead"
		lease.Release(&kind, "401", 0)
		// 冷却 2 分钟（生产值），测试里直接清掉。
		p.Update("a", func(st *State) { st.CooldownUntil = 0 })
	}
	st, _ := p.Get("a")
	if !st.Disabled {
		t.Fatalf("连续 3 次 401 应当自动禁用: %+v", st)
	}
	if _, err := p.Pick(nil, 0); err == nil {
		t.Fatal("禁用账号不该被选中")
	}
}

func TestBreakerTrips(t *testing.T) {
	p := newTestPool(t, "a")
	p.Configure(func(c *Config) { c.BreakerThresh = 2; c.BreakerCool = time.Second })
	for i := 0; i < 2; i++ {
		lease, err := p.Pick(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		kind := "server"
		lease.Release(&kind, "500", 0)
	}
	st, _ := p.Get("a")
	if st.BreakerUntil == 0 {
		t.Fatalf("连续 5xx 应当熔断: %+v", st)
	}
	if _, err := p.Pick(nil, 0); err == nil {
		t.Fatal("熔断期间不该被选中")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := p.Pick(nil, 0); err != nil {
		t.Fatalf("熔断到期后应恢复: %v", err)
	}
}

func TestClientErrorDoesNotPunish(t *testing.T) {
	p := newTestPool(t, "a")
	lease, _ := p.Pick(nil, 0)
	kind := "client"
	lease.Release(&kind, "invalid model", 0)
	st, _ := p.Get("a")
	if st.CooldownUntil != 0 || st.Disabled {
		t.Fatalf("参数错误不该罚号: %+v", st)
	}
	if _, err := p.Pick(nil, 0); err != nil {
		t.Fatalf("仍应可选: %v", err)
	}
}

func TestPickUIDAndPause(t *testing.T) {
	p := newTestPool(t, "a", "b")
	lease, err := p.PickUID("b")
	if err != nil {
		t.Fatal(err)
	}
	if lease.UID != "b" {
		t.Fatalf("PickUID 选到了 %s", lease.UID)
	}
	lease.ReleaseSuccess(0)
	p.SetPaused("b", true)
	if _, err := p.PickUID("b"); err == nil {
		t.Fatal("暂停账号不该被选中")
	}
	p.SetPaused("b", false)
	if _, err := p.PickUID("b"); err != nil {
		t.Fatalf("恢复后应可选: %v", err)
	}
}

func TestStatePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{StateFile: dir + "/state.json"})
	p.SyncToDir([]*auth.Auth{{Data: auth.Data{UID: "a", AccessToken: "t"}}})
	lease, _ := p.Pick(nil, 0)
	kind := "plan_limit"
	lease.Release(&kind, "欠费", time.Second)
	p.NoteCredits("a", 42)
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}

	p2 := New(Config{StateFile: dir + "/state.json"})
	p2.SyncToDir([]*auth.Auth{{Data: auth.Data{UID: "a", AccessToken: "t"}}})
	restored, err := p2.Restore(dir + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatalf("恢复数量 = %d", restored)
	}
	st, ok := p2.Get("a")
	if !ok || st.CooldownKind != "plan_limit" || st.Credits != 42 {
		t.Fatalf("状态没恢复: %+v", st)
	}
	if st.InFlight != 0 {
		t.Fatal("在途计数不该被持久化")
	}
}

func TestStateDropsRemovedAccount(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{StateFile: dir + "/state.json"})
	p.SyncToDir([]*auth.Auth{{Data: auth.Data{UID: "a", AccessToken: "t"}}})
	_ = p.Flush()

	// 账号文件被删掉后，状态不该阴魂不散地留在池里。
	p2 := New(Config{StateFile: dir + "/state.json"})
	p2.SyncToDir(nil)
	restored, err := p2.Restore(dir + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	if restored != 0 {
		t.Fatalf("已删除账号的状态仍被恢复 %d 条", restored)
	}
}

func TestExpiredCooldownClearedOnRestore(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{StateFile: dir + "/state.json"})
	p.SyncToDir([]*auth.Auth{{Data: auth.Data{UID: "a", AccessToken: "t"}}})
	p.Update("a", func(st *State) { st.CooldownUntil = time.Now().Add(-time.Hour).Unix(); st.CooldownKind = "soft_rate" })
	_ = p.Flush()

	p2 := New(Config{StateFile: dir + "/state.json"})
	p2.SyncToDir([]*auth.Auth{{Data: auth.Data{UID: "a", AccessToken: "t"}}})
	if _, err := p2.Restore(dir + "/state.json"); err != nil {
		t.Fatal(err)
	}
	st, _ := p2.Get("a")
	if st.CooldownUntil != 0 || st.CooldownKind != "" {
		t.Fatalf("过期冷却没清理: %+v", st)
	}
}

func TestMaxInFlightSoftCap(t *testing.T) {
	p := newTestPool(t, "a")
	p.Configure(func(c *Config) { c.MaxInFlight = 1 })
	first, err := p.Pick(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 第二个请求仍然能选到（软上限）：宁可多排一个，也不要把请求直接拒掉。
	second, err := p.Pick(nil, 0)
	if err != nil {
		t.Fatalf("软上限不该拒绝请求: %v", err)
	}
	st, _ := p.Get("a")
	if st.InFlight != 2 {
		t.Fatalf("在途 = %d, want 2", st.InFlight)
	}
	first.ReleaseSuccess(0)
	second.ReleaseSuccess(0)
	st, _ = p.Get("a")
	if st.InFlight != 0 {
		t.Fatalf("释放后仍在途 %d", st.InFlight)
	}
}

func TestNextPlanReset(t *testing.T) {
	// 凌晨 3 点：重置应在当天 4 点。
	loc := shanghai()
	at3 := time.Date(2026, 10, 26, 3, 0, 0, 0, loc)
	reset := NextPlanReset(at3)
	if reset.Hour() != 4 || reset.Day() != 26 {
		t.Fatalf("3 点 → %v", reset)
	}
	// 上午 10 点：重置应推到次日 4 点。
	at10 := time.Date(2026, 10, 26, 10, 0, 0, 0, loc)
	reset = NextPlanReset(at10)
	if reset.Hour() != 4 || reset.Day() != 27 {
		t.Fatalf("10 点 → %v", reset)
	}
}

// TestPickWaitQueuesWhenFull 守卫「在途满时先等、不硬塞给上游」。
//
// 上游单账号并发 12 起回 429 limiter_request_error（实测）。所以名额满的时候
// 等一会儿比打过去换一个 60 秒冷却划算。
func TestPickWaitQueuesWhenFull(t *testing.T) {
	p := newTestPool(t, "a")
	p.Configure(func(c *Config) { c.MaxInFlight = 1 })

	lease, err := p.Pick(nil, 0)
	if err != nil {
		t.Fatalf("第一个请求应当拿到号: %v", err)
	}
	// 200ms 后归还名额。
	go func() {
		time.Sleep(200 * time.Millisecond)
		lease.ReleaseSuccess(0)
	}()

	start := time.Now()
	lease2, err := p.PickWait(context.Background(), nil, 0, 2*time.Second)
	waited := time.Since(start)
	if err != nil {
		t.Fatalf("等待后应当拿到号: %v", err)
	}
	defer lease2.ReleaseSuccess(0)
	if waited < 150*time.Millisecond {
		t.Fatalf("应当真的等了（实际 %v）", waited)
	}
	if waited > 1500*time.Millisecond {
		t.Fatalf("等太久（实际 %v）", waited)
	}
}

// TestPickWaitFallsBackWhenTimeout 等不到名额时软回退（宁可多排一个也别 503）。
func TestPickWaitFallsBackWhenTimeout(t *testing.T) {
	p := newTestPool(t, "a")
	p.Configure(func(c *Config) { c.MaxInFlight = 1 })
	lease, err := p.Pick(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.ReleaseSuccess(0)

	start := time.Now()
	lease2, err := p.PickWait(context.Background(), nil, 0, 120*time.Millisecond)
	if err != nil {
		t.Fatalf("软回退不该报错: %v", err)
	}
	defer lease2.ReleaseSuccess(0)
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("应当等满再回退（实际 %v）", elapsed)
	}
	if lease2.UID != "a" {
		t.Fatalf("回退也要选到唯一那个号: %q", lease2.UID)
	}
}

// TestPickWaitContextCanceled 客户端断开时立刻返回，不占着名额空等。
func TestPickWaitContextCanceled(t *testing.T) {
	p := newTestPool(t, "a")
	p.Configure(func(c *Config) { c.MaxInFlight = 1 })
	lease, _ := p.Pick(nil, 0)
	defer lease.ReleaseSuccess(0)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.PickWait(ctx, nil, 0, 5*time.Second); err == nil {
		t.Fatal("ctx 取消后应当返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("不该等满 5s（实际 %v）", elapsed)
	}
}
