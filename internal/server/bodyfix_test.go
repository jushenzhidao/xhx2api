package server

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestLiftMaxTokens 覆盖真号实测出来的那组边界（2026-10-26，sn-sensenova-6-8-flash）：
// max_tokens=512 时思考吃满预算、正文回空，768 起才稳定出正文。
func TestLiftMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64 // 期望的字段值；0 = 字段不该出现
	}{
		{"16 抬到下限", `{"max_tokens":16}`, 1024},
		{"512 也抬（实测仍不够）", `{"max_tokens":512}`, 1024},
		{"768 也抬（宁稳勿省）", `{"max_tokens":768}`, 1024},
		{"给够了不动", `{"max_tokens":4096}`, 4096},
		{"等于下限不动", `{"max_tokens":1024}`, 1024},
		{"没给就不发明", `{}`, 0},
		{"0 当没给", `{"max_tokens":0}`, 0},
		// 负数不是合法额度：原样保留（不抬、不删），让上游自己报错。
		{"负数原样保留", `{"max_tokens":-1}`, -1},
		{"responses 字段名也认", `{"max_output_tokens":16}`, 1024},
		{"chat 的 max_completion_tokens 也认", `{"max_completion_tokens":16}`, 1024},
		{"字符串不认（不是数字就不动）", `{"max_tokens":"16"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := liftMaxTokens([]byte(tc.in), 1024)
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("输出不是 JSON: %v", err)
			}
			var v float64
			for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
				if f, ok := got[k].(float64); ok {
					v = f
				}
			}
			if v != tc.want {
				t.Fatalf("出站额度 = %v, want %v（输出 %s）", v, tc.want, out)
			}
			// 其他字段必须原样保留（不能因为抬额度把 messages 弄丢）。
			if tc.in == `{"max_tokens":16}` {
				if _, hasModel := got["model"]; hasModel {
					t.Fatal("不该凭空加字段")
				}
			}
		})
	}
}

func TestLiftMaxTokensDisabled(t *testing.T) {
	in := `{"max_tokens":16}`
	if out := liftMaxTokens([]byte(in), 0); string(out) != in {
		t.Fatalf("floor=0 应当原样返回，got %s", out)
	}
}

func TestLiftMaxTokensKeepsInvalidBody(t *testing.T) {
	in := "not json"
	if out := liftMaxTokens([]byte(in), 1024); string(out) != in {
		t.Fatalf("非法 JSON 被改动了: %s", out)
	}
}

func TestLiftMaxTokensPreservesOtherFields(t *testing.T) {
	in := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"你好"}],"stream":true}`
	out := liftMaxTokens([]byte(in), 1024)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "m" || got["stream"] != true {
		t.Fatalf("其他字段被改坏: %s", out)
	}
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages 丢了: %s", out)
	}
	if got["max_tokens"].(float64) != 1024 {
		t.Fatalf("max_tokens 没抬: %v", got["max_tokens"])
	}
}

// TestRaiseMaxTokensOnlyRaises 守卫「加额只抬不降」。
//
// 重试用例里最容易写错的就是「无条件把 max_tokens 设成 8192」——
// 客户端本来要 32768，被降成 8192 会直接截断回答。
func TestRaiseMaxTokensOnlyRaises(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 期望 max_tokens 的值
	}{
		{"小额度抬到 8192", `{"max_tokens":1024}`, "8192"},
		{"已更大则不动", `{"max_tokens":32768}`, "32768"},
		{"没给字段就补上", `{"messages":[]}`, "8192"},
		{"max_output_tokens 也认", `{"max_output_tokens":512}`, "8192"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := raiseMaxTokens([]byte(tc.in), 8192)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("输出不是 JSON: %s", out)
			}
			got := obj["max_tokens"]
			if got == nil {
				got = obj["max_output_tokens"]
			}
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("%s → max_tokens=%v，期望 %s", tc.in, got, tc.want)
			}
		})
	}
}
