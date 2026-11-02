package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// fakeOpts 控制桩的行为。三个维度都是实测里出现过的真实形态：
// 今天注册（没到发放时点）、已绑设备（有资格）、未绑设备（永远没分）、
// 以及 user_info 打不通（连状态都问不到）。
type fakeOpts struct {
	// created 按 uid 给 user_info.created_at；缺省用固定老号时间。
	created map[string]string
	// bound 按 uid 给 trusted_device.bound；nil 表示响应里不带 trusted_device。
	bound map[string]bool
	// breakUserInfo 让 user_info 返回 500，模拟「设备态查不到」。
	breakUserInfo bool
}

// fakeUpstream 造一个「只有注册礼包流水、从未发过 daily_grant」的上游。
func fakeUpstream(t *testing.T, opts fakeOpts) *upstream.Client {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("/api/web/points/v1/balance", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
			"available_points": 9000, "daily_points": 0, "monthly_points": 0,
			"reward_points": 9000, "topup_points": 0,
		}})
	})
	mux.HandleFunc("/api/web/points/v1/bills", func(w http.ResponseWriter, r *http.Request) {
		// 只有 reward_grant：没有 daily_grant
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
			"items": []any{map[string]any{"biz_type": "reward_grant", "created_at": "2026-10-29T17:07:31+08:00"}},
		}})
	})
	mux.HandleFunc("/api/web/desktop/v1/login/points/grant", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"granted": false}})
	})
	mux.HandleFunc("/api/web/auth/v1/user_info", func(w http.ResponseWriter, r *http.Request) {
		if opts.breakUserInfo {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		uid := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		at, ok := opts.created[uid]
		if !ok {
			at = "2026-10-26T09:42:45.92469+08:00"
		}
		data := map[string]any{"id": uid, "name": "号-" + uid, "created_at": at}
		if opts.bound != nil {
			if b, ok := opts.bound[uid]; ok {
				state := map[bool]string{true: "bound", false: "unbound"}[b]
				data["trusted_device"] = map[string]any{
					"bound": b, "device_bind_state": state,
					"reason": map[bool]string{true: "", false: "session_without_bound_device"}[b],
				}
			}
		}
		writeJSON(w, map[string]any{"code": 0, "data": data})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.SetAgentHosts(srv.URL+"/api/web/llm/v2", srv.URL+"/api/web/auth/v1")
	up.SetAPIHosts(srv.URL+"/api/web/points/v1", srv.URL)
	up.SetPersistHook(func(a *auth.Auth) error { return nil })
	return up
}

// fakeUpstreamCounting 在 fakeUpstream 之上数 user_info 命中次数，用来验证缓存。
func fakeUpstreamCounting(t *testing.T, opts fakeOpts, hits *atomic.Int64) *upstream.Client {
	t.Helper()
	up := fakeUpstream(t, opts)
	// 直接包一层 Transport：比复制整个桩干净，也不会让两边行为漂移。
	inner := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/user_info") {
			hits.Add(1)
		}
		return inner.RoundTrip(r)
	})
	up.StreamHTTP.Transport = up.HTTP.Transport
	return up
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeUpstream 的旧签名（只有 created）保留成薄封装，避免改一堆调用点。
func fakeUpstreamCreated(t *testing.T, created map[string]string) *upstream.Client {
	return fakeUpstream(t, fakeOpts{created: created})
}

func newScheduler(t *testing.T, up *upstream.Client, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New(pool.Config{StateFile: ""})
	dir := t.TempDir()
	auths := make([]*auth.Auth, 0, len(uids))
	for _, uid := range uids {
		a := &auth.Auth{Data: auth.Data{
			UID: uid, Nickname: "号-" + uid, AccessToken: "tok-" + uid,
			ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(),
		}}
		a.FilePath = dir + "/" + auth.FileNameFor(uid)
		auths = append(auths, a)
	}
	p.SyncToDir(auths)
	return New(p, up, Config{}), p
}

// 今天注册的号：bills 里还没有 daily_grant 是正常的，必须报 pending 而不是 ineligible。
func TestCheckinTodayRegisteredIsPending(t *testing.T) {
	now := time.Now().In(shanghai())
	up := fakeUpstreamCreated(t, map[string]string{"2001": now.Format(time.RFC3339)})
	s, p := newScheduler(t, up, "2001")

	a := p.AuthByUID("2001")
	if a == nil {
		t.Fatal("账号未入池")
	}
	out := s.CheckinAccount(context.Background(), a)
	if out.Status != "pending" {
		t.Fatalf("今天注册的号应报 pending，实得 %q（%s）", out.Status, out.Message)
	}
	// 顺带把上游注册时间落盘，下一轮不必再问。
	if a.RegisterTime().IsZero() {
		t.Fatal("取到的注册时间没有落盘")
	}
}

// 注册已过一天、bills 里从来没有 daily_grant：这个号就是没资格，报 ineligible。
func TestCheckinOldAccountNeverGrantedIsIneligible(t *testing.T) {
	up := fakeUpstream(t, fakeOpts{
		created: map[string]string{"2002": "2026-10-26T09:42:45.92469+08:00"},
		bound:   map[string]bool{"2002": true},
	})
	s, p := newScheduler(t, up, "2002")

	a := p.AuthByUID("2002")
	if a == nil {
		t.Fatal("账号未入池")
	}
	out := s.CheckinAccount(context.Background(), a)
	if out.Status != "ineligible" {
		t.Fatalf("老号且从未发分应报 ineligible，实得 %q（%s）", out.Status, out.Message)
	}
}

// 老号 + 从未发分 + 未绑设备：这是线上 11 个号里 9 个的真实形态，
// 必须报 unbound（说清该做什么），不能报 ineligible（听着像上游故障）。
func TestCheckinUnboundDeviceReportsUnbound(t *testing.T) {
	up := fakeUpstream(t, fakeOpts{
		bound: map[string]bool{"2003": false},
	})
	s, p := newScheduler(t, up, "2003")

	out := s.CheckinAccount(context.Background(), p.AuthByUID("2003"))
	if out.Status != "unbound" {
		t.Fatalf("未绑设备应报 unbound，实得 %q（%s）", out.Status, out.Message)
	}
	if out.DeviceBound != "unbound" {
		t.Fatalf("结果里应带 device_bound=unbound，实得 %q", out.DeviceBound)
	}
	if !strings.Contains(out.Message, "可信设备") {
		t.Fatalf("文案要让用户知道是设备绑定问题：%s", out.Message)
	}
}

// unbound 是账号状态、不是本次执行失败：批量签到里必须进 Skipped。
// 否则面板每天一条红字，真正的网络/凭证错误会被淹掉。
func TestCheckinUnboundCountedAsSkipped(t *testing.T) {
	up := fakeUpstream(t, fakeOpts{
		bound: map[string]bool{"2004": false, "2005": true},
	})
	s, p := newScheduler(t, up, "2004", "2005")
	if p.AuthByUID("2004") == nil {
		t.Fatal("账号未入池")
	}

	result := s.CheckinAll(context.Background())
	if result.Failed != 0 {
		t.Fatalf("不该有失败，实得 failed=%d：%v", result.Failed, result.Details)
	}
	if result.Skipped != 2 {
		t.Fatalf("unbound 与 ineligible 都该跳过，实得 skipped=%d：%v", result.Skipped, result.Details)
	}
}

// 设备态查不到（user_info 500、本地又没有缓存）：只能报待定。
// 把「问不到」说成「无资格」就是拿猜测当结论——宁可下一轮再试。
func TestCheckinDeviceUnknownStaysPending(t *testing.T) {
	up := fakeUpstream(t, fakeOpts{breakUserInfo: true})
	s, p := newScheduler(t, up, "2006")

	out := s.CheckinAccount(context.Background(), p.AuthByUID("2006"))
	if out.Status != "pending" {
		t.Fatalf("设备态未知应报 pending，实得 %q（%s）", out.Status, out.Message)
	}
	if out.Status == "ineligible" || out.Status == "unbound" {
		t.Fatal("不知道就是不知道，不能编一个结论")
	}
}

// 设备态要落盘并复用：签到是每天跑的，不缓存等于每天给每个号多打一次 user_info。
func TestDeviceStateCachedBetweenRuns(t *testing.T) {
	var hits atomic.Int64
	up := fakeUpstreamCounting(t, fakeOpts{bound: map[string]bool{"2007": false}}, &hits)
	s, p := newScheduler(t, up, "2007")
	a := p.AuthByUID("2007")

	s.CheckinAccount(context.Background(), a)
	first := hits.Load()
	if first == 0 {
		t.Fatal("首轮应当查一次 user_info")
	}
	s.CheckinAccount(context.Background(), a)
	if got := hits.Load(); got != first {
		t.Fatalf("12h 内应复用缓存，user_info 又被打了 %d 次", got-first)
	}
	// 落盘要带上状态，重启后不用重新问。
	if snap := a.Snapshot(); snap.DeviceBound != "unbound" || snap.DeviceCheckedAt == "" {
		t.Fatalf("设备态没落盘：%+v", snap)
	}
}
