// 可信设备绑定：/api/web/auth/v1/devices_current_bind
//
// 这一步的来龙去脉值得写清楚，因为它决定了「为什么有些号永远没有每日积分」。
//
// 上游把「有没有每日积分资格」绑定在 `trusted_device.bound` 上（bound=false 的
// 号从来没拿到过 daily_grant，实测 11 个号里 9 个如此）。而 `device_id` 是
// **服务端**在绑定接口里生成并发下来的，客户端伪造不了：
//
//   - 自造 `x-device-id` / `X-Device-Id` / `x-raccoon-device-id` 三种头，
//     user_info 里 device_id 依旧是空、bound 依旧是 false；
//   - 页面上的所有 `/device*`、`/bind*` 路径都回 404 —— 真实路径是
//     `/devices_current_bind`（复数 + 下划线），靠猜是猜不到的。
//
// 路径是从官方桌面客户端（macOS 版 app.asar）里读出来的：
// `DesktopTrustedDeviceApi.bindCurrentDevice()`。同一个类里还有
// `/devices_current_heartbeat`，说明绑定之外还有心跳保活。
//
// 绑定成功后上游回 `{"device_id": "...", "trusted_device": true}`，
// 再查 user_info 立刻变成 bound=true / needs_bind=false。所以对已经在池子里的
// unbound 账号，**不需要重新登录**，调一次这个接口就能补上。
package upstream

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rsfree/xhx2api/internal/auth"
)

// DeviceBindPath 是绑定当前会话可信设备的端点（auth 域下的相对路径）。
const DeviceBindPath = "/devices_current_bind"

// DeviceHeartbeatPath 保活当前设备。绑完不心跳，上游可能把设备判成离线。
const DeviceHeartbeatPath = "/devices_current_heartbeat"

// deviceBindPlatform 与 deviceBindOS 取自官方客户端的常量表
// （resolveDesktopClientPlatform / resolveDesktopOs）：
// darwin→desktop-macos/macos、win32→desktop-windows/windows、linux→desktop-linux/linux。
//
// 这里固定成 Windows：池子里的号一直以 desktop-windows-x64 的身份出站
// （clientIDHeaders），绑定时换平台等于自己制造身份不一致。
const (
	deviceBindPlatform  = "desktop-windows"
	deviceBindOS        = "windows"
	deviceBindOSVersion = "10.0.19045"
	deviceBindApp       = "desktop"
)

// BindDevice 给账号绑定一个可信设备，返回上游分配的 device_id。
//
// 幂等：已绑定的号重复调用仍返回同一个 device_id（上游按 client_device_id 认）。
func (c *Client) BindDevice(ctx context.Context, a *auth.Auth) (string, error) {
	if a == nil {
		return "", &Error{Kind: ErrClient, Status: 400, Msg: "账号为空"}
	}
	token := a.JWT()
	if token == "" {
		return "", &Error{Kind: ErrClient, Status: 400, Msg: "账号没有可用令牌"}
	}

	// client_device_id 是「这台设备」的稳定标识，必须持久化后复用。
	// 每次绑都换一个 UUID，等于同一账号不断声明新设备——这正是风控最敏感的模式。
	deviceID := strings.TrimSpace(a.ClientDeviceID)
	if deviceID == "" {
		deviceID = newClientDeviceID()
	}

	body, err := json.Marshal(map[string]string{
		"client_device_id_source": "unknown",
		"client_device_id":        deviceID,
		"client_platform":         deviceBindPlatform,
		"client_version":          c.effectiveVersion(),
		"device_name":             deviceNameOf(a.UID),
		"os":                      deviceBindOS,
		"os_version":              deviceBindOSVersion,
		"application":             deviceBindApp,
		"push_permission":         "unknown",
	})
	if err != nil {
		return "", err
	}

	headers := c.clientIDHeaders()
	// 官方客户端在这四个头上带的是绑定专用的写法：X-Client-Device-ID 必带，
	// 平台不带 -x64 后缀。服务端显然用它们做一致性校验，所以照抄。
	//
	// ⚠️ 必须先删掉小写那份再写大写：clientIDHeaders() 用的是小写键，
	// 而 http.Header.Set 会把 `x-client-platform` 和 `X-Client-Platform`
	// 归一到同一个规范键 —— 两个都留着时，map 遍历顺序随机，
	// 实际发出去的值就随机（测试里抓到过一次发成 desktop-windows-x64）。
	delete(headers, "x-client-platform")
	delete(headers, "x-client-version")
	headers["X-Client-Platform"] = deviceBindPlatform
	headers["X-Client-Device-ID"] = deviceID
	headers["X-Client-Version"] = c.effectiveVersion()
	headers["X-Request-Id"] = newClientDeviceID()

	resp, err := c.doJSON(ctx, a, http.MethodPost, c.authBase()+DeviceBindPath,
		json.RawMessage(body), headers)
	if err != nil {
		return "", err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return "", &Error{Kind: Classify(resp.Status, string(resp.Body)), Status: resp.Status,
			Msg: fmt.Sprintf("绑定可信设备失败（HTTP %d）：%s", resp.Status, truncate(string(resp.Body), 200))}
	}

	var envelope struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			DeviceID      string `json:"device_id"`
			TrustedDevice bool   `json:"trusted_device"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal(resp.Body, &envelope); jerr != nil {
		return "", &Error{Kind: ErrServer, Status: 502,
			Msg: fmt.Sprintf("绑定设备返回无法解析: %s", truncate(string(resp.Body), 160))}
	}
	bound := strings.TrimSpace(envelope.Data.DeviceID)
	if bound == "" {
		return "", &Error{Kind: ErrServer, Status: 502,
			Msg: fmt.Sprintf("上游没有返回 device_id: %s", truncate(string(resp.Body), 160))}
	}

	// 落盘：下次绑定/心跳要复用同一个 id，重启后也不能丢。
	a.ClientDeviceID = deviceID
	a.DeviceBound = "bound"
	return bound, nil
}

// effectiveVersion 当前出站客户端版本。
func (c *Client) effectiveVersion() string { return c.currentVersion() }

// newClientDeviceID 生成一个 UUIDv4（与官方客户端 desktop-device-identity.json
// 里的 clientDeviceId 同形态）。
//
// 用 crypto/rand 生成真随机，不从 uid 派生：这个 id 的语义是「一台机器的身份」，
// 让它和账号有任何可推导关系，等于把「同一批号」这件事写进了标识里。
// 不引第三方 uuid 包，16 字节 + 版本位足够。
func newClientDeviceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源坏了也不能回退到可预测的值：宁可让调用方失败。
		panic("crypto/rand 不可用: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// deviceNameOf 造一个像模像样的 Windows 机器名。
//
// 不用固定串：多账号报同一个机器名，等于自己把「这是一批脚本号」写在脸上。
// 也不用真主机名：宿主机的名字不该泄漏给上游。
func deviceNameOf(uid string) string {
	sum := sha256.Sum256([]byte("device-name|" + uid))
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ0123456789"
	out := make([]byte, 7)
	for i := range out {
		out[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return "DESKTOP-" + string(out)
}
