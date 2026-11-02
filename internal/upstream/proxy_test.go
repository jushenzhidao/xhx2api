package upstream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 最小 SOCKS5 服务端（只做 CONNECT + 用户名/密码认证）
//
// 为什么真起一个而不是 mock：要验证的正是「SOCKS5 握手里的用户名是不是带账号」。
// 把拨号器换成桩只能证明「调了 Dial」，证明不了用户名，而用户名就是粘性出口的
// 全部依据 —— 供应商按用户名 hash 分配出口 IP。

type socksRecord struct {
	mu     sync.Mutex
	users  []string
	dials  int
	closed bool
}

func (r *socksRecord) addUser(u string) {
	r.mu.Lock()
	r.users = append(r.users, u)
	r.mu.Unlock()
}

func (r *socksRecord) addDial() {
	r.mu.Lock()
	r.dials++
	r.mu.Unlock()
}

func (r *socksRecord) snapshot() ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.users...), r.dials
}

// startSOCKS5 起一个支持 用户名/密码 与 无认证 的 SOCKS5 服务端。
func startSOCKS5(t *testing.T) (addr string, rec *socksRecord) {
	t.Helper()
	rec = &socksRecord{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))

				// 1) 问候
				head := make([]byte, 2)
				if _, err := io.ReadFull(c, head); err != nil || head[0] != 5 {
					return
				}
				methods := make([]byte, int(head[1]))
				if _, err := io.ReadFull(c, methods); err != nil {
					return
				}
				wantAuth := false
				for _, m := range methods {
					if m == 2 {
						wantAuth = true
					}
				}
				if wantAuth {
					if _, err := c.Write([]byte{5, 2}); err != nil {
						return
					}
					// 2) 用户名/密码（RFC 1929）
					ver := make([]byte, 2)
					if _, err := io.ReadFull(c, ver); err != nil || ver[0] != 1 {
						return
					}
					uname := make([]byte, int(ver[1]))
					if _, err := io.ReadFull(c, uname); err != nil {
						return
					}
					plen := make([]byte, 1)
					if _, err := io.ReadFull(c, plen); err != nil {
						return
					}
					if _, err := io.ReadFull(c, make([]byte, int(plen[0]))); err != nil {
						return
					}
					rec.addUser(string(uname))
					if _, err := c.Write([]byte{1, 0}); err != nil {
						return
					}
				} else {
					if _, err := c.Write([]byte{5, 0}); err != nil {
						return
					}
				}

				// 3) 请求
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil || req[0] != 5 || req[1] != 1 {
					return
				}
				var host string
				switch req[3] {
				case 1:
					b := make([]byte, 4)
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = net.IP(b).String()
				case 3:
					l := make([]byte, 1)
					if _, err := io.ReadFull(c, l); err != nil {
						return
					}
					b := make([]byte, int(l[0]))
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = string(b)
				case 4:
					b := make([]byte, 16)
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = net.IP(b).String()
				default:
					return
				}
				pbuf := make([]byte, 2)
				if _, err := io.ReadFull(c, pbuf); err != nil {
					return
				}
				port := binary.BigEndian.Uint16(pbuf)

				rec.addDial()
				up, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 3*time.Second)
				if err != nil {
					_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				_ = c.SetDeadline(time.Time{})
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}(conn)
		}
	}()
	return ln.Addr().String(), rec
}

// ---------------------------------------------------------------------------
// 模板解析

func TestParseProxyTemplate(t *testing.T) {
	ep, err := parseProxyTemplate("socks5h://poolclient-{account}:s3cret@pool.livetest.cn:2090")
	if err != nil {
		t.Fatal(err)
	}
	if ep.host != "pool.livetest.cn:2090" {
		t.Fatalf("host = %q", ep.host)
	}
	if got := ep.sourceOf("7772869"); got != "poolclient-7772869:***@pool.livetest.cn:2090" {
		t.Fatalf("sourceOf = %q", got)
	}
	// 密码绝不能出现在展示形态里
	if strings.Contains(ep.String(), "s3cret") {
		t.Fatalf("密码泄漏: %s", ep.String())
	}
	if strings.Contains(ep.sourceOf("1"), "s3cret") {
		t.Fatalf("密码泄漏: %s", ep.sourceOf("1"))
	}
	// 不同账号必须带出不同用户名 —— 这是「出口按账号恒定」的唯一依据
	if ep.sourceOf("7772869") == ep.sourceOf("7774611") {
		t.Fatal("两个账号的代理用户名相同，粘性出口无从谈起")
	}
}

func TestParseProxyTemplateRejectsBad(t *testing.T) {
	for _, tpl := range []string{"", "http://host:1", "socks5://host-without-port", "socks5h://:1"} {
		if _, err := parseProxyTemplate(tpl); err == nil {
			t.Fatalf("%q 应当解析失败", tpl)
		}
	}
}

// ---------------------------------------------------------------------------
// 端到端：按账号走各自的 SOCKS5 用户名

func TestProxyRoutesByAccount(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	defer srv.Close()

	addr, rec := startSOCKS5(t)

	c := New()
	c.SetProxy(ProxyConfig{
		Enabled:  true,
		Template: "socks5h://poolclient-{account}:pw@" + addr,
		Scope:    ProxyScopeAll,
	})

	a1 := &auth.Auth{Data: auth.Data{UID: "7772869", AccessToken: "t1"}}
	a2 := &auth.Auth{Data: auth.Data{UID: "7774611", AccessToken: "t2"}}
	for _, a := range []*auth.Auth{a1, a2} {
		resp, err := c.doJSON(context.Background(), a, http.MethodGet, srv.URL+"/x", nil, nil)
		if err != nil {
			t.Fatalf("账号 %s 请求失败: %v", a.UID, err)
		}
		if resp.Status != 200 {
			t.Fatalf("账号 %s 状态 %d", a.UID, resp.Status)
		}
	}

	users, _ := rec.snapshot()
	if len(users) != 2 {
		t.Fatalf("应当有两次带认证的握手，实得 %v", users)
	}
	if users[0] != "poolclient-7772869" || users[1] != "poolclient-7774611" {
		t.Fatalf("代理用户名没带账号: %v", users)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 2 {
		t.Fatalf("上游收到 %d 次请求，应为 2（说明请求没真的穿隧道）", hits)
	}
}

// 同一账号复用 transport：不能每次请求都新开一条连接池。
func TestProxyTransportReusedPerAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	defer srv.Close()
	addr, rec := startSOCKS5(t)

	c := New()
	c.SetProxy(ProxyConfig{Enabled: true, Template: "socks5h://u-{account}:pw@" + addr, Scope: ProxyScopeAll})
	a := &auth.Auth{Data: auth.Data{UID: "9001", AccessToken: "t"}}

	for i := 0; i < 3; i++ {
		if _, err := c.doJSON(context.Background(), a, http.MethodGet, srv.URL+"/x", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	users, dials := rec.snapshot()
	if len(users) != 1 {
		t.Fatalf("同一账号只该有一次 SOCKS 握手（复用连接池），实得 %d 次: %v", len(users), users)
	}
	_ = dials
}

// 未启用代理时必须完全走原来的共享 client（既有行为不能变）。
func TestProxyDisabledUsesSharedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	defer srv.Close()
	addr, rec := startSOCKS5(t)

	c := New()
	c.SetProxy(ProxyConfig{Enabled: false, Template: "socks5h://u-{account}:pw@" + addr, Scope: ProxyScopeAll})
	a := &auth.Auth{Data: auth.Data{UID: "9002", AccessToken: "t"}}
	if _, err := c.doJSON(context.Background(), a, http.MethodGet, srv.URL+"/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if users, _ := rec.snapshot(); len(users) != 0 {
		t.Fatalf("代理关闭时不该有任何握手，实得 %v", users)
	}
	if c.ProxyEnabled() {
		t.Fatal("ProxyEnabled 应为 false")
	}
}

// 模板非法时降级为直连并保持可用（一次配置笔误不该让全站不可用）。
func TestProxyBadTemplateFallsBackToDirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	defer srv.Close()

	c := New()
	c.SetProxy(ProxyConfig{Enabled: true, Template: "http://nope:1"})
	if c.ProxyEnabled() {
		t.Fatal("非法模板不应报告为已启用")
	}
	a := &auth.Auth{Data: auth.Data{UID: "9003", AccessToken: "t"}}
	if _, err := c.doJSON(context.Background(), a, http.MethodGet, srv.URL+"/x", nil, nil); err != nil {
		t.Fatalf("应回落到直连而不是报错: %v", err)
	}
}

// 出口 IP 探测：走该账号的出口，且结果按 TTL 缓存。
func TestEgressIPProbeAndCache(t *testing.T) {
	var mu sync.Mutex
	var probes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		probes++
		mu.Unlock()
		fmt.Fprint(w, "203.0.113.7")
	}))
	defer srv.Close()
	addr, rec := startSOCKS5(t)

	c := New()
	c.SetProxy(ProxyConfig{
		Enabled:        true,
		Template:       "socks5h://u-{account}:pw@" + addr,
		Scope:          ProxyScopeAll,
		EgressCheckURL: srv.URL,
		EgressTTL:      time.Hour,
	})
	a := &auth.Auth{Data: auth.Data{UID: "9004", AccessToken: "t"}}

	if ip := c.EgressIP(context.Background(), a.UID); ip != "203.0.113.7" {
		t.Fatalf("出口 IP = %q", ip)
	}
	// 第二次必须命中缓存，不再探测
	c.EgressIP(context.Background(), a.UID)
	mu.Lock()
	got := probes
	mu.Unlock()
	if got != 1 {
		t.Fatalf("TTL 内应复用缓存，实际探测 %d 次", got)
	}
	if users, _ := rec.snapshot(); len(users) == 0 {
		t.Fatal("探测也必须走该账号的出口，不能直连")
	}
	if !strings.Contains(c.ProxyRoute(context.Background(), a.UID), "出口=203.0.113.7") {
		t.Fatalf("ProxyRoute 没带上出口: %s", c.ProxyRoute(context.Background(), a.UID))
	}
}

// 代理连不通必须判成 gateway（不罚号、不熔断），不能判成 server。
//
// 回归的是一个真实的放大路径：ai-prod 的 points 域被 IP 封之后改用代理，
// 若代理某一跳抽风连不上，server 类错误会连续累加 FailCount，3 次就把
// 好号打进熔断——一次链路故障变成「账号全灭」。
func TestProxyDialFailureIsGatewayNotServer(t *testing.T) {
	c := New()
	// 指向一个必然拒绝连接的端口：127.0.0.1:1
	c.SetProxy(ProxyConfig{
		Enabled:     true,
		Template:    "socks5h://u-{account}:pw@127.0.0.1:1",
		Scope:       ProxyScopeAll,
		DialTimeout: 500 * time.Millisecond,
	})
	a := &auth.Auth{Data: auth.Data{UID: "9100", AccessToken: "t"}}

	_, err := c.doJSON(context.Background(), a, http.MethodGet, "http://example.invalid/x", nil, nil)
	if err == nil {
		t.Fatal("应当失败")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型不对: %T %v", err, err)
	}
	if ue.Kind != ErrGateway {
		t.Fatalf("代理连不通应为 gateway（不罚号），实得 %v", ue.Kind)
	}
	if ue.Kind.Minor() != true {
		t.Fatal("gateway 必须是 Minor：换号但不惩罚账号")
	}
}

// 同一账号重复建 client 必须复用同一条 transport（否则每个请求一个新连接池）。
func TestProxyClientReusesTransport(t *testing.T) {
	c := New()
	c.SetProxy(ProxyConfig{Enabled: true, Template: "socks5h://u-{account}:pw@127.0.0.1:9"})
	cl1, err := c.proxy.clientFor("9005", time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cl2, err := c.proxy.clientFor("9005", time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if cl1.Transport != cl2.Transport {
		t.Fatal("同账号同类型应复用 transport")
	}
	cl3, err := c.proxy.clientFor("9006", time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if cl3.Transport == cl1.Transport {
		t.Fatal("不同账号必须是各自的 transport，否则出口无法区分")
	}
}

// 默认作用域只覆盖积分域：聊天直连、积分走代理。
//
// 这不是洁癖：代理供应商常常只开通部分用户名，全局启用会让其余账号的
// 聊天也一起断。默认值的取舍必须偏向「不破坏已经正常的链路」。
func TestProxyScopeDefaultsToPoints(t *testing.T) {
	cfg := ProxyConfig{Enabled: true}
	if !cfg.appliesTo("https://xiaohuanxiong.com/api/web/points/v1/balance") {
		t.Fatal("积分余额应走代理")
	}
	if !cfg.appliesTo("https://xiaohuanxiong.com/api/web/points/v1/bills?paging.limit=100&type=income") {
		t.Fatal("积分流水应走代理")
	}
	if !cfg.appliesTo("https://xiaohuanxiong.com/api/web/desktop/v1/login/points/grant") {
		t.Fatal("领取积分应走代理")
	}
	if cfg.appliesTo("https://xiaohuanxiong.com/api/web/llm/v2/chat/completions") {
		t.Fatal("聊天不该走代理（默认作用域）")
	}
	if cfg.appliesTo("https://xiaohuanxiong.com/api/web/auth/v1/user_info") {
		t.Fatal("用户资料不该走代理（默认作用域）")
	}

	all := ProxyConfig{Enabled: true, Scope: ProxyScopeAll}
	if !all.appliesTo("https://xiaohuanxiong.com/api/web/llm/v2/chat/completions") {
		t.Fatal("all 作用域下聊天也应走代理")
	}
	if (ProxyConfig{Enabled: false, Scope: ProxyScopeAll}).appliesTo("https://x/api/web/points/v1/balance") {
		t.Fatal("关闭时一律不走代理")
	}
}

// 代理不通时必须立刻回落直连，业务不能断。
//
// 2026-11-03 线上真实场景：代理供应商只开通了 11 个用户名里的 2 个，其余
// 回 SOCKS5 码 3。而这些账号直连本来是 200。如果代理失败就直接报错，等于
// 「为了绕开 IP 风控，反而把 9 个好好的号做成了不可用」——比不加代理更糟。
func TestProxyDialFailureFallsBackToDirect(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, `{"code":0,"echo":%q}`, string(body))
	}))
	defer srv.Close()

	c := New()
	c.SetProxy(ProxyConfig{
		Enabled:     true,
		Template:    "socks5h://u-{account}:pw@127.0.0.1:1",
		Scope:       ProxyScopeAll,
		DialTimeout: 300 * time.Millisecond,
	})
	a := &auth.Auth{Data: auth.Data{UID: "9200", AccessToken: "t"}}

	// 带 body 的 POST：回落时必须重发 body，否则上游会收到空体
	resp, err := c.doJSON(context.Background(), a, http.MethodPost, srv.URL+"/x",
		map[string]any{"k": "v"}, nil)
	if err != nil {
		t.Fatalf("代理不通时应回落直连成功，实得: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status = %d", resp.Status)
	}
	if !strings.Contains(string(resp.Body), `\"k\":\"v\"`) && !strings.Contains(string(resp.Body), "k") {
		t.Fatalf("回落直连后 body 丢了: %s", resp.Body)
	}
	if hits.Load() != 1 {
		t.Fatalf("上游应收到 1 次请求，实得 %d", hits.Load())
	}
}

// 代理刚失败过的账号要在冷却期内直接走直连，不要每个请求都先撞一次超时。
//
// 这决定了「9 个用户名不通」时的实际体验：没有冷却，每次查余额都要先等
// 一次建连失败；有冷却，第一个请求撞一次，之后都直接走直连。
func TestProxyFailCooldownSkipsProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0}`)
	}))
	defer srv.Close()

	// 一个只计数、不服务的端口：accept 后立刻关闭，制造 SOCKS 握手失败
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var attempts atomic.Int64
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			attempts.Add(1)
			conn.Close()
		}
	}()

	c := New()
	c.SetProxy(ProxyConfig{
		Enabled:     true,
		Template:    "socks5h://u-{account}:pw@" + ln.Addr().String(),
		Scope:       ProxyScopeAll,
		DialTimeout: 300 * time.Millisecond,
	})
	a := &auth.Auth{Data: auth.Data{UID: "9201", AccessToken: "t"}}

	for i := 0; i < 3; i++ {
		if _, derr := c.doJSON(context.Background(), a, http.MethodGet, srv.URL+"/x", nil, nil); derr != nil {
			t.Fatalf("第 %d 次应回落直连成功: %v", i+1, derr)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("冷却期内只该撞一次代理，实际建连 %d 次", got)
	}
	if !c.proxy.inFailCooldown("9201") {
		t.Fatal("失败后应处于冷却窗口")
	}
	// 别的账号不受影响：冷却按账号隔离
	if c.proxy.inFailCooldown("9202") {
		t.Fatal("冷却不该跨账号生效")
	}
}
