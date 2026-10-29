package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// fakeUpstream 造一个「只有注册礼包流水、从未发过 daily_grant」的上游。
//
// created 按 uid 给各自的 user_info.created_at，用来区分新号与老号。
func fakeUpstream(t *testing.T, created map[string]string) *upstream.Client {
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
		uid := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		at, ok := created[uid]
		if !ok {
			at = "2026-10-26T09:42:45.92469+08:00"
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
			"id": uid, "name": "号-" + uid, "created_at": at,
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.SetAgentHosts(srv.URL+"/api/web/llm/v2", srv.URL+"/api/web/auth/v1")
	up.SetAPIHosts(srv.URL+"/api/web/points/v1", srv.URL)
	up.SetPersistHook(func(a *auth.Auth) error { return nil })
	return up
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
	up := fakeUpstream(t, map[string]string{"2001": now.Format(time.RFC3339)})
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
	up := fakeUpstream(t, map[string]string{"2002": "2026-10-26T09:42:45.92469+08:00"})
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
