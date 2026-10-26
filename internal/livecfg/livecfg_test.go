package livecfg

import "testing"

// TestAliasLookup 守住快照查别名的两个约定：大小写不敏感、空表不 panic。
func TestAliasLookup(t *testing.T) {
	s := Snapshot{Aliases: map[string]string{
		"gpt-4o":                   "raccoon-chat-ml-5-5",
		"deepseek-v4-1-flash-1017": "sn-deepseek-v4-1-flash-1017",
	}}

	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"gpt-4o", "raccoon-chat-ml-5-5", true},
		{"GPT-4O", "raccoon-chat-ml-5-5", true}, // 客户端常把模型名大写
		{"deepseek-v4-1-flash-1017", "sn-deepseek-v4-1-flash-1017", true},
		{"DeepSeek-V4.1-Flash-1017", "", false}, // 点号不等同短横，不做模糊匹配
		{"gpt-9", "", false},
	}
	for _, c := range cases {
		got, ok := s.Alias(c.in)
		if ok != c.ok || got != c.want {
			t.Fatalf("Alias(%q) = (%q,%v)，期望 (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}

	// 空表：必须返回 false 而不是 panic（零值 Snapshot 会被大量代码路径用到）。
	if _, ok := (Snapshot{}).Alias("gpt-4o"); ok {
		t.Fatal("空别名表不应命中")
	}
}
