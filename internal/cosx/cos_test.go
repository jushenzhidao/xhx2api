package cosx

import (
	"strings"
	"testing"
	"time"
)

// TestSignMatchesReference 锁死签名算法。
//
// 期望值来自一份独立实现（Python 按 COS 官方文档逐步算的），用固定时间与
// 固定密钥产生。两边同时写错的概率很低；但它证明的是「实现符合我对文档的
// 理解」，真机是否正确还需要一次真实上传验证——没有密钥时只能做到这一步。
func TestSignMatchesReference(t *testing.T) {
	c := &Client{
		cfg:  Config{SecretID: "AKIDEXAMPLE", SecretKey: "SECRETEXAMPLE"},
		host: "demo-1250000000.cos.ap-guangzhou.myqcloud.com",
	}
	start, end := time.Unix(1700000000, 0), time.Unix(1700003600, 0)
	got := c.sign("PUT", "/xhx/abc.png", map[string]string{
		"host":      "demo-1250000000.cos.ap-guangzhou.myqcloud.com",
		"x-cos-acl": "public-read",
	}, start, end)

	want := "q-sign-algorithm=sha1&q-ak=AKIDEXAMPLE" +
		"&q-sign-time=1700000000;1700003600&q-key-time=1700000000;1700003600" +
		"&q-header-list=host;x-cos-acl&q-url-param-list=" +
		"&q-signature=0f49c997d5ebcd7cd9533a6ca6dc13a2aab7707e"
	if got != want {
		t.Fatalf("签名不匹配\n got=%s\nwant=%s", got, want)
	}
}

// TestSignHeaderListSorted 确认参与签名的头按字典序、键名小写。
// 顺序错了会直接 SignatureDoesNotMatch，而错误信息不会告诉你是排序问题。
func TestSignHeaderListSorted(t *testing.T) {
	c := &Client{cfg: Config{SecretID: "ak", SecretKey: "sk"}, host: "h"}
	got := c.sign("put", "/k", map[string]string{
		"x-cos-acl": "public-read", "host": "h", "Content-Type": "image/png",
	}, time.Unix(0, 0), time.Unix(1, 0))
	if !strings.Contains(got, "q-header-list=content-type;host;x-cos-acl") {
		t.Fatalf("header-list 未按字典序小写: %s", got)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"缺 secret_id", Config{SecretKey: "k", Bucket: "b-1", Region: "r"}},
		{"缺 secret_key", Config{SecretID: "i", Bucket: "b-1", Region: "r"}},
		{"缺 bucket", Config{SecretID: "i", SecretKey: "k", Region: "r"}},
		{"缺 region", Config{SecretID: "i", SecretKey: "k", Bucket: "b-1"}},
	}
	for _, c := range cases {
		if _, err := New(c.cfg); err == nil {
			t.Fatalf("%s 应当报错", c.name)
		}
	}
	if _, err := New(Config{SecretID: "i", SecretKey: "k", Bucket: "b-1", Region: "r"}); err != nil {
		t.Fatalf("完整配置不应报错: %v", err)
	}
}

func TestObjectURL(t *testing.T) {
	c, err := New(Config{SecretID: "i", SecretKey: "k", Bucket: "b-125", Region: "ap-guangzhou", Prefix: "xhx/prod"})
	if err != nil {
		t.Fatal(err)
	}
	// objectURL 收的是「完整对象键」，前缀由 Put 拼接（这里手动带上以还原）。
	if got := c.objectURL(c.cfg.Prefix + "ab.png"); got != "https://b-125.cos.ap-guangzhou.myqcloud.com/xhx/prod/ab.png" {
		t.Fatalf("默认域名 url 错: %s", got)
	}

	c2, err := New(Config{SecretID: "i", SecretKey: "k", Bucket: "b-125", Region: "ap-guangzhou",
		BaseURL: "https://cdn.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.objectURL("ab.png"); got != "https://cdn.example.com/ab.png" {
		t.Fatalf("自定义域名应当覆盖且去掉尾斜杠: %s", got)
	}

	// 配了 TTL 就要带签名参数。
	c3, err := New(Config{SecretID: "i", SecretKey: "k", Bucket: "b-125", Region: "ap-guangzhou",
		SignedTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := c3.objectURL("ab.png"); !strings.Contains(got, "q-signature=") {
		t.Fatalf("开了 SignedTTL 却没带签名: %s", got)
	}
}

// TestPrefixNormalized 确认前缀前后斜杠被归一，避免出现 //。
func TestPrefixNormalized(t *testing.T) {
	c, err := New(Config{SecretID: "i", SecretKey: "k", Bucket: "b-1", Region: "r", Prefix: "/a/b/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Prefix != "a/b/" {
		t.Fatalf("前缀归一错: %q", c.cfg.Prefix)
	}
}
