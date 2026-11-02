package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rsfree/xhx2api/internal/auth"
)

// BindDevice 必须按官方客户端的口径发请求：路径、请求头、body 字段一个都不能少。
//
// 这条路是从官方桌面客户端的 app.asar 里读出来的（DesktopTrustedDeviceApi.
// bindCurrentDevice），靠猜永远猜不到 —— 真实路径是 /devices_current_bind
// （复数 + 下划线），而我们此前试过的 device/bind、device_bind 全是 404。
// 所以这里用桩把「服务端实际收到什么」钉死，任何一处漂移都会立刻红。
func TestBindDeviceSendsOfficialShape(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	var gotHeader http.Header
	var gotBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		gotPath, gotHeader, gotBody = r.URL.Path, r.Header.Clone(), body
		mu.Unlock()
		// 上游真实响应形状
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"device_id":"dev-abc-123","session_id":"web-1","trusted_device":true}}`))
	}))
	defer srv.Close()

	c := New()
	c.SetAgentHosts("", srv.URL) // auth 域指向桩

	a := &auth.Auth{Data: auth.Data{UID: "7000001", AccessToken: "tok"}}
	deviceID, err := c.BindDevice(context.Background(), a)
	if err != nil {
		t.Fatalf("BindDevice 失败: %v", err)
	}
	if deviceID != "dev-abc-123" {
		t.Fatalf("device_id = %q", deviceID)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/devices_current_bind" {
		t.Errorf("路径 = %q，应为 /devices_current_bind", gotPath)
	}
	// 四个头是官方客户端固定带的，服务端用它们做一致性校验
	for name, want := range map[string]string{
		"X-Client-Platform": "desktop-windows",
		"X-Client-Version":  "v1.1.0",
	} {
		if v := gotHeader.Get(name); v != want {
			t.Errorf("%s = %q，应为 %q", name, v, want)
		}
	}
	if gotHeader.Get("X-Client-Device-ID") == "" {
		t.Error("X-Client-Device-ID 为空：上游靠它认设备，缺了等于每次都是新机器")
	}
	if gotBody["client_platform"] != "desktop-windows" || gotBody["os"] != "windows" {
		t.Errorf("平台字段不对: %v", gotBody)
	}
	if gotBody["application"] != "desktop" || gotBody["client_device_id_source"] != "unknown" {
		t.Errorf("固定字段不对: %v", gotBody)
	}
	if gotBody["client_device_id"] == "" || gotBody["device_name"] == "" {
		t.Errorf("缺少设备标识: %v", gotBody)
	}

	// 落盘字段：绑定后必须记住 client_device_id，否则下次会换新设备
	if a.ClientDeviceID == "" || a.ClientDeviceID != gotBody["client_device_id"] {
		t.Errorf("ClientDeviceID 未持久化或与请求不一致: %q vs %q", a.ClientDeviceID, gotBody["client_device_id"])
	}
	if a.DeviceBound != "bound" {
		t.Errorf("DeviceBound = %q，应为 bound", a.DeviceBound)
	}
}

// client_device_id 必须稳定复用：重复绑定不能换新设备。
//
// 换新 = 同一账号不断声明新设备，正是风控最敏感的模式。
func TestBindDeviceReusesClientDeviceID(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		seen = append(seen, body["client_device_id"])
		mu.Unlock()
		_, _ = w.Write([]byte(`{"code":0,"data":{"device_id":"dev-fixed","trusted_device":true}}`))
	}))
	defer srv.Close()

	c := New()
	c.SetAgentHosts("", srv.URL)
	a := &auth.Auth{Data: auth.Data{UID: "7000002", AccessToken: "tok"}}

	for i := 0; i < 3; i++ {
		if _, err := c.BindDevice(context.Background(), a); err != nil {
			t.Fatalf("第 %d 次绑定失败: %v", i+1, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("应发 3 次，实得 %d", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[0] {
			t.Fatalf("第 %d 次换了 client_device_id（%s → %s），等于换机器", i+1, seen[0], seen[i])
		}
	}
	if strings.TrimSpace(seen[0]) == "" {
		t.Fatal("client_device_id 为空")
	}
}

// 不同账号必须拿到不同的 client_device_id。
func TestClientDeviceIDUniquePerAccount(t *testing.T) {
	a := newClientDeviceID()
	b := newClientDeviceID()
	if a == b {
		t.Fatal("两次生成相同 UUID")
	}
	// v4 形态：第 13 位是 4
	if len(a) != 36 || a[14] != '4' {
		t.Fatalf("不是 UUIDv4 形态: %s", a)
	}
}

// 上游回 200 但没有 device_id 时必须报错，不能当成功。
//
// 假装成功会让「补绑」变成静默失败：面板显示 OK，但号还是 unbound，
// 用户第二天发现没分时已经无从追查。
func TestBindDeviceRequiresDeviceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{}}`))
	}))
	defer srv.Close()

	c := New()
	c.SetAgentHosts("", srv.URL)
	a := &auth.Auth{Data: auth.Data{UID: "7000003", AccessToken: "tok"}}
	if _, err := c.BindDevice(context.Background(), a); err == nil {
		t.Fatal("没有 device_id 时必须报错")
	}
	if a.DeviceBound == "bound" {
		t.Fatal("失败时不能把账号标成 bound")
	}
}
