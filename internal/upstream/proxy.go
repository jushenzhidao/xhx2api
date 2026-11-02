// 按账号粘性出口的 SOCKS5 代理。
//
// 为什么必须「按账号」而不是「整机一个代理」：上游对同一出口 IP 并发查多个
// 账号的积分很敏感。2026-11-02 实测，ai-prod 在 00:01 一次性跑了 11 个号的
// 签到，4 分钟后 /api/web/points/ 整个路径前缀开始回 403（空 body +
// X-TT-System-Error: 3），而同一 token 在本地与另一台机器上都是 200 —— 封的是
// 「IP + 路径」，不是凭证。给每个账号固定一个出口，等于把识别面收敛回单账号，
// 也顺带让「一次封禁打掉全部账号」变成「只影响那一个」。
//
// socks5h 的 h 指「主机名交给代理解析」：Go 的 x/net/proxy 在地址是域名时会发
// ATYP=3，解析发生在代理侧，本地拿不到真实 DNS 结果，与 socks5h 语义一致。
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"golang.org/x/net/proxy"
)

// ProxyAccountPlaceholder 是模板里代表账号 uid 的占位符。
const ProxyAccountPlaceholder = "{account}"

// 出口 IP 探测的兜底地址（配置里给了就用配置的）。
//
// 三个轮着试：单一站点在某些网络下不通，而「查不到出口 IP」会让排障回到
// 「403 但不知道是谁被封」的原点，所以这里宁可多试一个。
var defaultEgressChecks = []string{
	"https://api.ipify.org",
	"https://myip.ipip.net",
	"https://ipinfo.io/ip",
}

// ipv4RE 从任意文本里捞 IPv4（myip.ipip.net 回的是「当前 IP：1.2.3.4 来自于…」）。
var ipv4RE = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

// ProxyConfig 是按账号粘性出口的代理配置。
type ProxyConfig struct {
	// Enabled 打开后，所有带账号上下文的请求都走代理。
	Enabled bool
	// Template 形如 socks5h://user-{account}:pass@host:port。
	// {account} 会被替换成账号 uid；不含占位符时就是「所有账号共用一条」。
	Template string
	// DialTimeout 是建连上限（含 SOCKS5 握手）。
	DialTimeout time.Duration
	// EgressCheckURL 用来查出口 IP；空则用内置兜底列表。
	EgressCheckURL string
	// EgressTTL 是出口 IP 的缓存时长。出口是「按账号恒定」的，
	// 查一次能用很久；设太短等于给自己加探测流量。
	EgressTTL time.Duration
	// Scope 决定哪些请求走代理：ProxyScopePoints（默认）/ ProxyScopeAll。
	//
	// 为什么要有作用域：代理供应商往往只给部分用户名开通（2026-11-03 实测
	// 11 个账号里只有 2 个能建隧道，其余回 SOCKS5 码 3）。全局启用会让
	// 另外 9 个号的**聊天**也一起断——而聊天本来直连是好的。默认只让积分
	// 域走代理：那正是被 IP 封的路径，其余保持原样。
	Scope string
}

const (
	// ProxyScopePoints 只让积分相关请求（余额/流水/领取）走代理。
	ProxyScopePoints = "points"
	// ProxyScopeAll 所有带账号的请求都走代理。
	ProxyScopeAll = "all"
)

// normalizedScope 归一作用域（未知值按 points 处理：最保守）。
func normalizedScope(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ProxyScopeAll:
		return ProxyScopeAll
	default:
		return ProxyScopePoints
	}
}

// appliesTo 判断某个 URL 是否该走代理。
func (c ProxyConfig) appliesTo(rawURL string) bool {
	if !c.Enabled {
		return false
	}
	if normalizedScope(c.Scope) == ProxyScopeAll {
		return true
	}
	// 积分全域：/api/web/points/v1/*（余额、流水）与各端的 /login/points/grant。
	return strings.Contains(rawURL, "/api/web/points/") || strings.Contains(rawURL, "/points/grant")
}

// normalize 补默认值。空模板但 Enabled=true 视为配置错误，由 configure 兜住。
func (c ProxyConfig) normalize() ProxyConfig {
	if c.DialTimeout <= 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.EgressTTL <= 0 {
		c.EgressTTL = 30 * time.Minute
	}
	return c
}

// proxyEndpoint 是模板解析后的形状。
type proxyEndpoint struct {
	host     string // host:port
	userTmpl string // 可能含 {account}
	pass     string
}

// String 返回脱敏后的展示形态（密码永远不进日志）。
func (e proxyEndpoint) String() string {
	user := e.userTmpl
	if e.pass != "" {
		return fmt.Sprintf("%s:***@%s", user, e.host)
	}
	return fmt.Sprintf("%s@%s", user, e.host)
}

// sourceOf 返回某个账号实际使用的代理身份（脱敏）。
func (e proxyEndpoint) sourceOf(uid string) string {
	user := strings.ReplaceAll(e.userTmpl, ProxyAccountPlaceholder, uid)
	if e.pass != "" {
		return fmt.Sprintf("%s:***@%s", user, e.host)
	}
	return fmt.Sprintf("%s@%s", user, e.host)
}

// parseProxyTemplate 解析代理模板。
func parseProxyTemplate(tpl string) (proxyEndpoint, error) {
	var ep proxyEndpoint
	tpl = strings.TrimSpace(tpl)
	if tpl == "" {
		return ep, errors.New("代理模板为空")
	}
	// net/url 不允许 userinfo 里出现 { 与 }（报 invalid userinfo），而模板的
	// 用户名本来就带 {account}。先换成一个纯字母占位符再解析，解析完换回来。
	// 这类「模板语法和 URL 语法打架」的坑必须在入口处理：否则用户填了标准
	// 形态的模板，程序只会说一句「模板无效」，完全看不出是花括号的问题。
	const marker = "accountplaceholder"
	safe := strings.ReplaceAll(tpl, ProxyAccountPlaceholder, marker)
	if strings.Contains(safe, "{") || strings.Contains(safe, "}") {
		return ep, errors.New("代理模板里有无法识别的花括号，只有 {account} 是占位符")
	}
	u, err := url.Parse(safe)
	if err != nil {
		return ep, fmt.Errorf("代理模板解析失败: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
	default:
		return ep, fmt.Errorf("只支持 socks5:// 或 socks5h://，实得 %q", u.Scheme)
	}
	if u.Host == "" {
		return ep, errors.New("代理模板缺少 host:port")
	}
	host, port, herr := net.SplitHostPort(u.Host)
	if herr != nil {
		return ep, fmt.Errorf("代理模板的 host 必须带端口: %w", herr)
	}
	if host == "" || port == "" {
		return ep, errors.New("代理模板的 host 与端口都不能为空")
	}
	ep.host = u.Host
	if u.User != nil {
		ep.userTmpl = strings.ReplaceAll(u.User.Username(), marker, ProxyAccountPlaceholder)
		ep.pass, _ = u.User.Password()
	}
	return ep, nil
}

// egressEntry 是一次出口 IP 查询的结果。
type egressEntry struct {
	ip  string
	at  time.Time
	err error // 非空表示查失败，同样要缓存，避免失败时被反复重试
}

// proxyManager 持有按账号的拨号器、transport 与出口 IP 缓存。
//
// transport 必须按账号分开：出口身份是拨号阶段（SOCKS5 用户名）决定的，
// 而 http.Transport 的 DialContext 拿不到请求信息，只能一个账号一个池。
type proxyManager struct {
	mu      sync.RWMutex
	cfg     ProxyConfig
	ep      proxyEndpoint
	epErr   error
	dialers map[string]proxy.Dialer
	trs     map[string]*http.Transport
	egress  map[string]egressEntry
	// probing 去重并发探测：同一账号同时只查一次出口 IP。
	probing map[string]bool

	// failedAt 记录「该账号的代理连不通」的时刻，用于临时跳过代理。
	//
	// 2026-11-03 线上教训：代理供应商只开通了部分用户名，其余用户名连
	// 1.1.1.1 都不通（SOCKS5 码 3）。没有这个冷却时，每个请求都要先等一次
	// 建连超时再失败，9 个账号的积分查询从「直连本来 200」降级成「全部失败」。
	// 有它以后：失败一次就记下来，冷却期内直接走直连，到期再试一次。
	failMu   sync.Mutex
	failedAt map[string]time.Time
}

// proxyFailCooldown 是代理连不通后「直接走直连」的时长。
//
// 5 分钟：够短，代理恢复后能较快重新用上；够长，不至于每个请求都先撞一次
// 超时。到期后自动再试一次（不是永久放弃）——供应商开通用户名后无需重启。
const proxyFailCooldown = 5 * time.Minute

func newProxyManager() *proxyManager {
	return &proxyManager{
		dialers:  map[string]proxy.Dialer{},
		trs:      map[string]*http.Transport{},
		egress:   map[string]egressEntry{},
		probing:  map[string]bool{},
		failedAt: map[string]time.Time{},
	}
}

// configure 热更新配置。模板/开关变化时清掉所有缓存：
// 旧 transport 绑的是旧出口，留着继续用等于「改了配置没生效」。
func (m *proxyManager) configure(cfg ProxyConfig) {
	cfg = cfg.normalize()
	m.mu.Lock()
	defer m.mu.Unlock()
	same := m.cfg.Enabled == cfg.Enabled && m.cfg.Template == cfg.Template
	m.cfg = cfg
	if same {
		return
	}
	m.ep, m.epErr = proxyEndpoint{}, nil
	if cfg.Enabled {
		m.ep, m.epErr = parseProxyTemplate(cfg.Template)
		if m.epErr != nil {
			log.Printf("[proxy] 代理配置无效，回落到直连：%v", m.epErr)
		}
	}
	m.dialers = map[string]proxy.Dialer{}
	m.trs = map[string]*http.Transport{}
	m.egress = map[string]egressEntry{}
	m.probing = map[string]bool{}
	m.failMu.Lock()
	m.failedAt = map[string]time.Time{}
	m.failMu.Unlock()
}

// usable 报告当前是否可以走代理。
func (m *proxyManager) usable() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Enabled && m.epErr == nil && m.ep.host != ""
}

// usableFor 报告「这个账号此刻是否该走代理」：全局启用、作用域命中、
// 且不在失败冷却窗口里。
func (m *proxyManager) usableFor(uid, rawURL string) bool {
	if !m.usable() {
		return false
	}
	if !m.appliesTo(rawURL) {
		return false
	}
	return !m.inFailCooldown(uid)
}

// appliesTo 判断某 URL 是否该走代理（按当前配置的作用域）。
func (m *proxyManager) appliesTo(rawURL string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.appliesTo(rawURL)
}

// sourceOf 返回账号的代理身份（脱敏），未启用时返回空串。
func (m *proxyManager) sourceOf(uid string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.cfg.Enabled || m.epErr != nil {
		return ""
	}
	return m.ep.sourceOf(uid)
}

// dialerFor 取（并缓存）某账号的 SOCKS5 拨号器。
func (m *proxyManager) dialerFor(uid string) (proxy.Dialer, error) {
	m.mu.RLock()
	if d, ok := m.dialers[uid]; ok {
		m.mu.RUnlock()
		return d, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.dialers[uid]; ok {
		return d, nil
	}
	var auth *proxy.Auth
	user := strings.ReplaceAll(m.ep.userTmpl, ProxyAccountPlaceholder, uid)
	if user != "" || m.ep.pass != "" {
		auth = &proxy.Auth{User: user, Password: m.ep.pass}
	}
	d, err := proxy.SOCKS5("tcp", m.ep.host, auth, proxy.Direct)
	if err != nil {
		return nil, err
	}
	m.dialers[uid] = d
	return d, nil
}

// transportFor 取（并缓存）某账号专属的 transport。
//
// headerTimeout 是「等响应头」的上限，生图要传 0（它耗时 20-200s，
// 120s 的响应头超时会卡在边上）。
func (m *proxyManager) transportFor(uid string, headerTimeout time.Duration) (*http.Transport, error) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	key := fmt.Sprintf("%s|%s", uid, headerTimeout)
	m.mu.RLock()
	if tr, ok := m.trs[key]; ok {
		m.mu.RUnlock()
		return tr, nil
	}
	m.mu.RUnlock()

	d, err := m.dialerFor(uid)
	if err != nil {
		return nil, err
	}

	tr := newTransport(headerTimeout)
	// 按账号的连接池要收窄：11 个账号各 200 条空闲连接是纯浪费。
	tr.MaxIdleConns = 16
	tr.MaxIdleConnsPerHost = 8
	// 显式关掉环境变量代理：连不连代理只能由本配置决定。
	// 否则容器里一个 HTTP_PROXY 就能把出站悄悄挪回被封的 IP。
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, derr := m.dialFor(ctx, d, cfg, network, addr)
		if derr != nil {
			m.noteDialFailure(uid)
			// 包成可识别类型：出口不可用和「账号有问题」是两件事，
			// 混为一谈会让一次代理故障把好号全打进熔断。
			return nil, &proxyDialError{uid: uid, addr: addr, err: derr}
		}
		return conn, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.trs[key]; ok {
		return existing, nil
	}
	m.trs[key] = tr
	return tr, nil
}

// noteDialFailure 记下「该账号这次经代理没连上」。
func (m *proxyManager) noteDialFailure(uid string) {
	m.failMu.Lock()
	if m.failedAt == nil {
		m.failedAt = map[string]time.Time{}
	}
	m.failedAt[uid] = time.Now()
	m.failMu.Unlock()
}

// inFailCooldown 报告该账号是否处于「代理刚失败过、先走直连」的窗口。
func (m *proxyManager) inFailCooldown(uid string) bool {
	m.failMu.Lock()
	defer m.failMu.Unlock()
	at, ok := m.failedAt[uid]
	if !ok {
		return false
	}
	if time.Since(at) > proxyFailCooldown {
		// 过期就地清理：下一个请求会重新尝试代理，代理恢复后自动回归。
		delete(m.failedAt, uid)
		return false
	}
	return true
}

// dialFor 做实际拨号（含 DialTimeout）。
func (m *proxyManager) dialFor(ctx context.Context, d proxy.Dialer, cfg ProxyConfig, network, addr string) (net.Conn, error) {
	// x/net/proxy 的 socks5.Dialer 实现了 ContextDialer，能吃到 ctx 的
	// 取消与超时；退化路径手动兜一个 goroutine，避免没有 deadline 卡死。
	if cd, ok := d.(proxy.ContextDialer); ok {
		dctx := ctx
		if cfg.DialTimeout > 0 {
			var cancel context.CancelFunc
			dctx, cancel = context.WithTimeout(ctx, cfg.DialTimeout)
			defer cancel()
		}
		return cd.DialContext(dctx, network, addr)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, derr := d.Dial(network, addr)
		ch <- result{conn, derr}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

// proxyDialError 表示「经代理连不通」，不是账号的问题。
//
// 单独成类型是为了让上游客户端能把它和真正的上游 5xx 区分开：后者要熔断
// 账号，前者绝不能——代理挂了却去罚账号，等于把链路故障放大成账号全灭。
type proxyDialError struct {
	uid  string
	addr string
	err  error
}

func (e *proxyDialError) Error() string {
	return fmt.Sprintf("代理出口不可用（账号 %s，目标 %s）：%v", e.uid, e.addr, e.err)
}

func (e *proxyDialError) Unwrap() error { return e.err }

// clientFor 给某账号造一个走代理的 http.Client。
func (m *proxyManager) clientFor(uid string, headerTimeout, totalTimeout time.Duration) (*http.Client, error) {
	tr, err := m.transportFor(uid, headerTimeout)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: totalTimeout, Transport: tr}, nil
}

// cachedEgress 取未过期的出口 IP（可能为空）。
func (m *proxyManager) cachedEgress(uid string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.egress[uid]
	if !ok {
		return ""
	}
	if m.cfg.EgressTTL > 0 && time.Since(entry.at) > m.cfg.EgressTTL {
		return ""
	}
	return entry.ip
}

// EgressIP 返回账号的出口 IP；没有缓存就同步查一次。
//
// 查不到返回 ""：日志里显示「出口=?」比编一个地址诚实——排障时错的地址
// 会把排查方向带到别的机器上去。
func (m *proxyManager) EgressIP(ctx context.Context, uid string) string {
	if !m.usable() || uid == "" {
		return ""
	}
	if ip := m.cachedEgress(uid); ip != "" {
		return ip
	}
	// 同一账号并发只探测一次，其余等结果。
	m.mu.Lock()
	if m.probing[uid] {
		m.mu.RUnlock()
		return m.cachedEgress(uid)
	}
	entry, hasCache := m.egress[uid]
	if hasCache && m.cfg.EgressTTL > 0 && time.Since(entry.at) <= m.cfg.EgressTTL {
		m.mu.Unlock()
		return entry.ip
	}
	m.probing[uid] = true
	m.mu.Unlock()

	ip, err := m.probeEgress(ctx, uid)

	m.mu.Lock()
	m.egress[uid] = egressEntry{ip: ip, at: time.Now(), err: err}
	delete(m.probing, uid)
	m.mu.Unlock()
	if err != nil {
		log.Printf("[proxy] 账号 %s 出口 IP 查询失败：%v", uid, err)
	}
	return ip
}

// probeEgress 通过该账号的出口查一次公网 IP。
func (m *proxyManager) probeEgress(ctx context.Context, uid string) (string, error) {
	cl, err := m.clientFor(uid, 10*time.Second, 8*time.Second)
	if err != nil {
		return "", err
	}
	urls := defaultEgressChecks
	m.mu.RLock()
	if strings.TrimSpace(m.cfg.EgressCheckURL) != "" {
		urls = []string{strings.TrimSpace(m.cfg.EgressCheckURL)}
	}
	m.mu.RUnlock()

	var lastErr error
	for _, u := range urls {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if rerr != nil {
			lastErr = rerr
			continue
		}
		resp, derr := cl.Do(req)
		if derr != nil {
			lastErr = derr
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s 返回 HTTP %d", u, resp.StatusCode)
			continue
		}
		if ip := firstIP(string(raw)); ip != "" {
			return ip, nil
		}
		lastErr = fmt.Errorf("%s 未返回可识别的 IP", u)
	}
	if lastErr == nil {
		lastErr = errors.New("无可用探测地址")
	}
	return "", lastErr
}

// firstIP 从响应文本里取第一个看起来合法的 IPv4。
func firstIP(raw string) string {
	for _, cand := range ipv4RE.FindAllString(raw, -1) {
		ip := net.ParseIP(cand)
		if ip != nil && ip.To4() != nil {
			return cand
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 账号上下文

type accountCtxKey struct{}

// withAccount 把账号 uid 挂到 ctx 上，供 clientFor 选出口。
func withAccount(ctx context.Context, uid string) context.Context {
	if uid == "" {
		return ctx
	}
	return context.WithValue(ctx, accountCtxKey{}, uid)
}

// accountUID 取 ctx 上的账号 uid（没有则空串）。
func accountUID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	uid, _ := ctx.Value(accountCtxKey{}).(string)
	return uid
}

// ---------------------------------------------------------------------------
// Client 对外接口

// uidOf 取账号 uid（nil 安全）。
func uidOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return a.UID
}

// SetProxy 热更新代理配置。
func (c *Client) SetProxy(cfg ProxyConfig) {
	c.proxy.configure(cfg)
}

// ProxyEnabled 报告代理是否生效（配置合法且启用）。
func (c *Client) ProxyEnabled() bool { return c.proxy.usable() }

// ProxySource 返回账号实际使用的代理身份（已脱敏，密码不出现在任何输出里）。
// 未启用代理时返回空串。
func (c *Client) ProxySource(uid string) string { return c.proxy.sourceOf(uid) }

// EgressIP 返回账号的出口 IP（带缓存；查不到返回空串）。
func (c *Client) EgressIP(ctx context.Context, uid string) string {
	return c.proxy.EgressIP(ctx, uid)
}

// ProxyRoute 返回一行「来源 + 出口」描述，供日志与面板复用。
//
// 来源是「账号 → 代理身份」，出口是实测的公网 IP。两者必须一起看：
// 同一个出口 IP 配了多个账号用户，等于没分散；出口与预期不符说明
// 代理把粘性做丢了（很多代理供应商是按用户名hash的，用户名错了就串）。
func (c *Client) ProxyRoute(ctx context.Context, uid string) string {
	src := c.proxy.sourceOf(uid)
	if src == "" {
		return "来源=直连 出口=本机"
	}
	eg := c.proxy.cachedEgress(uid)
	if eg == "" {
		eg = "?"
	}
	return fmt.Sprintf("来源=%s 出口=%s", src, eg)
}

// LogProxyEgress 探测并打印每个账号的「来源 + 出口 IP」。
//
// 启动时跑一次：以后任何 403 都能直接对上「是哪个出口被拦」，
// 不用再猜是凭证还是 IP。并发压到 2，避免自己先把代理打满。
func (c *Client) LogProxyEgress(ctx context.Context, uids []string) {
	if !c.proxy.usable() || len(uids) == 0 {
		return
	}
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			ip := c.proxy.EgressIP(ctx, uid)
			if ip == "" {
				ip = "?"
			}
			log.Printf("[proxy] 账号 %s %s", uid, c.ProxyRoute(ctx, uid))
		}(uid)
	}
	wg.Wait()
}

// logProxiedFailure 在代理启用时把「来源 + 出口」打进错误日志。
//
// 只记 401/403/429/5xx：这些才可能与「出口被风控」有关。参数类 400 是
// 调用方的问题，记出口没有信息量，还会把日志刷满（线上 16 条/3 分钟的教训）。
func (c *Client) logProxiedFailure(ctx context.Context, method, path string, status int) {
	if !c.proxy.usable() {
		return
	}
	uid := accountUID(ctx)
	if uid == "" {
		return
	}
	switch {
	case status == 401, status == 403, status == 429, status == 0, status >= 500:
	default:
		return
	}
	// 用缓存值，不在错误路径上再去探测出口（那会把一次失败拖成两次超时）。
	eg := c.proxy.cachedEgress(uid)
	if eg == "" {
		eg = "?"
	}
	st := "连接失败"
	if status > 0 {
		st = fmt.Sprintf("HTTP %d", status)
	}
	// 打一行而不是两行：日志里「来源」和「出口」必须能一眼对上同一个请求，
	// 拆成两行后中间会插进别的账号的输出，grep 时对不齐。
	log.Printf("[proxy] 账号=%s 来源=%s 出口=%s %s %s -> %s",
		uid, c.proxy.sourceOf(uid), eg, method, path, st)
}
