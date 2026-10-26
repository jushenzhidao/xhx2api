package server

import "testing"

// TestShouldRotateImage 锁定「生图失败后要不要换号」的契约。
//
// 核心一条：client（499，调用方自己断开）绝不换号。这条曾经写成黑名单
// `kind != "client" && kind != "session_dead" && ...`，读起来像「client 不换」，
// 实际含义是「只有 client 之外的三类才不换」——client 反而落进可换号那一侧。
// 2026-10-26 线上实测：客户端 20s 超时断开后，网关换了第二个号又完整跑了一遍，
// 两次都记 499，白烧一次上游生成。
func TestShouldRotateImage(t *testing.T) {
	cases := []struct {
		kind string
		want bool
		why  string
	}{
		{"session_dead", true, "登录态失效：换号可能就通了"},
		{"server", true, "上游 5xx：换号"},
		{"gateway", true, "网关侧异常：换号"},
		{"client", false, "调用方已断开，换号只会白烧配额"},
		{"plan_limit", false, "额度不足，换号没意义"},
		{"soft_rate", false, "429 由外层原地重试处理，不走换号"},
		{"param_error", false, "参数错，换号也一样错"},
	}
	for _, c := range cases {
		if got := shouldRotateImage(c.kind); got != c.want {
			t.Errorf("shouldRotateImage(%q) = %v，期望 %v（%s）", c.kind, got, c.want, c.why)
		}
	}
}

// TestClientDisconnectNeverRotates 断言对话与生图两条路径对 client 的判断一致。
//
// 对话路径走 shouldRotate 的白名单，生图走 shouldRotateImage；两边若不统一，
// 就会出现「对话不换号、生图换号」的隐性分叉。
func TestClientDisconnectNeverRotates(t *testing.T) {
	if shouldRotate("client") {
		t.Error("对话路径会对 client 换号：调用方已断开，换号是白烧配额")
	}
	if shouldRotateImage("client") {
		t.Error("生图路径会对 client 换号：调用方已断开，换号是白烧配额")
	}
}
