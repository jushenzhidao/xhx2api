package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 刚补绑设备的号不能被判成 ineligible。
//
// 这条守的是一个「修好 A 反而打开 B」的坑：ineligible 原本的条件是
// 「bound=true 但流水里从来没有 daily_grant」，那在「绑定只能靠官方客户端、
// 代码补不了」的前提下是对的。但补绑接口打通之后，我们刚把号绑上，
// 流水里当然一条都没有 —— 此时判 ineligible 就等于「绑好了也永远不再检查」，
// 把这批号白废掉。
//
// 宽限期内应报 pending（等上游首次发放），而不是 ineligible。
func TestCheckinJustBoundDeviceIsPending(t *testing.T) {
	up := fakeUpstream(t, fakeOpts{
		// fakeUpstream 默认就是「只有注册礼包、从未发过 daily_grant」
		bound: map[string]bool{"3001": true},
	})
	s, p := newScheduler(t, up, "3001")

	a := p.AuthByUID("3001")
	// 模拟「刚刚补绑成功」
	a.DeviceBound = "bound"
	a.DeviceCheckedAt = time.Now().Format(time.RFC3339)
	a.DeviceBoundAt = time.Now().Format(time.RFC3339)

	out := s.CheckinAccount(context.Background(), a)
	if out.Status != "pending" {
		t.Fatalf("刚绑设备的号应报 pending，实得 %q（%s）", out.Status, out.Message)
	}
	if !strings.Contains(out.Message, "刚补绑") {
		t.Fatalf("文案要说明是刚绑上、在等上游发放：%s", out.Message)
	}
}

// 绑定很久、依然从未发过分的号，仍然判 ineligible。
//
// 宽限期不能变成「永远 pending」：真无资格的号会天天占一次上游请求，
// 而且面板上永远显示「稍后再试」，用户看不出这个号其实已经被上游放弃了。
func TestCheckinLongBoundNeverGrantedIsIneligible(t *testing.T) {
	up := fakeUpstream(t, fakeOpts{
		bound: map[string]bool{"3002": true},
	})
	s, p := newScheduler(t, up, "3002")

	a := p.AuthByUID("3002")
	a.DeviceBound = "bound"
	a.DeviceCheckedAt = time.Now().Format(time.RFC3339)
	// 10 天前绑的，早就过了宽限期
	a.DeviceBoundAt = time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339)

	out := s.CheckinAccount(context.Background(), a)
	if out.Status != "ineligible" {
		t.Fatalf("绑了很久仍无发放应报 ineligible，实得 %q（%s）", out.Status, out.Message)
	}
}

// 绑定时刻未知时沿用老判断，但必须给出可执行的出路。
//
// 我们自己的补绑一定会写 device_bound_at，所以「未知」= 绑定发生在系统之外。
// 这种情况沿用「老号 + 从未发分 = 上游不给」的判断，但不能只说「无需签到」：
// 万一用户刚在官方客户端绑过，他需要知道「点补绑设备可以同步状态」。
func TestCheckinUnknownBindTimeGivesWayOut(t *testing.T) {
	up := fakeUpstream(t, fakeOpts{
		bound: map[string]bool{"3003": true},
	})
	s, p := newScheduler(t, up, "3003")

	a := p.AuthByUID("3003")
	a.DeviceBound = "bound"
	a.DeviceCheckedAt = time.Now().Format(time.RFC3339)
	// 故意不设 DeviceBoundAt（模拟老凭证）

	out := s.CheckinAccount(context.Background(), a)
	if out.Status != "ineligible" {
		t.Fatalf("绑定时刻未知的老号仍应判 ineligible，实得 %q", out.Status)
	}
	if !strings.Contains(out.Message, "补绑设备") {
		t.Fatalf("文案必须告诉用户「补绑设备」这条出路：%s", out.Message)
	}
}

// boundRecently 的基本语义。
func TestBoundRecently(t *testing.T) {
	now := time.Now()
	if _, ok := boundRecently("", now); ok {
		t.Fatal("空值应返回 ok=false（不知道 ≠ 很久以前）")
	}
	if _, ok := boundRecently("不是时间", now); ok {
		t.Fatal("解析失败应返回 ok=false")
	}
	d, ok := boundRecently(now.Add(-2*time.Hour).Format(time.RFC3339), now)
	if !ok || d < time.Hour || d > 3*time.Hour {
		t.Fatalf("2 小时前绑定，应得到约 2h，实得 %v ok=%v", d, ok)
	}
}
