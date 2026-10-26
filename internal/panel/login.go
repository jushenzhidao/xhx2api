// login.go 面板侧的登录会话：微信扫码、浏览器环回授权、授权码直接换票。
//
// 三种通道各有适用场景：
//
//	扫码      手机上微信扫一下就完成，不需要在跑网关的机器上开浏览器，
//	          也不需要公网回调。部署在服务器上时最省事的路径。
//	环回授权  必须在浏览器能访问 127.0.0.1:<网关端口> 的机器上用（本机/局域网），
//	          但走的是官方 authorize 页，能过页面上的人机验证。
//	授权码    上面两条都不通时的兜底：用户在自己浏览器里走完授权，把
//	          office-raccoon:// 回调链接整条粘进来。
package panel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// loginTTL 一次性登录会话的寿命。
const loginTTL = 6 * time.Minute

// loginSession 是一次进行中的登录。
type loginSession struct {
	kind      string // "qr" / "device"
	qr        *upstream.QrLogin
	qrContent string // 二维码里编的 URL（面板渲染 PNG 用）
	device    *upstream.DeviceLogin
	createdAt time.Time

	// 设备授权的回调已经送到（state 由 DeviceLogin 自己校验）。
	// 结果只允许取一次。
	credential *auth.Auth
	pollErr    error
}

// loginManager 管理进行中的登录会话。
type loginManager struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

func newLoginManager() *loginManager {
	return &loginManager{sessions: map[string]*loginSession{}}
}

// gcLocked 清理超时会话（调用方已持锁）。
func (m *loginManager) gcLocked() {
	now := time.Now()
	for id, sess := range m.sessions {
		if now.Sub(sess.createdAt) > loginTTL {
			delete(m.sessions, id)
		}
	}
}

// StartQr 开一个扫码会话。
func (m *loginManager) StartQr(client *upstream.Client) (id, content string, err error) {
	qr, err := upstream.NewQrLogin(client)
	if err != nil {
		return "", "", err
	}
	// 必须先求值 QRContent 再存会话：qrContent 是命名返回值，直接写 content
	// 会在赋值那一刻把空串存进会话（二维码端点于是永远 404）。
	content = qr.QRContent()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	m.sessions[qr.State()] = &loginSession{kind: "qr", qr: qr, qrContent: content, createdAt: time.Now()}
	return qr.State(), content, nil
}

// PollQr 轮询扫码状态。
//
// 返回值：status（中文文案）、凭证（成功时）、错误。
func (m *loginManager) PollQr(ctx context.Context, id string) (string, *auth.Auth, error) {
	m.mu.Lock()
	sess := m.sessions[id]
	m.mu.Unlock()
	if sess == nil || sess.kind != "qr" {
		return "", nil, errors.New("扫码会话不存在或已过期，请重新发起")
	}
	if sess.credential != nil {
		m.takeCredential(id)
		return "已授权", sess.credential, nil
	}
	creds, done, err := sess.qr.Poll(ctx)
	if err != nil {
		if errors.Is(err, upstream.ErrQrCanceled) {
			m.mu.Lock()
			delete(m.sessions, id)
			m.mu.Unlock()
		}
		return sess.qr.LastStatus(), nil, err
	}
	if done && creds != nil {
		sess.credential = creds
		return "已授权", creds, nil
	}
	return sess.qr.LastStatus(), nil, nil
}

// QrContent 返回扫码会话的二维码内容（未知 id 返回空）。
func (m *loginManager) QrContent(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess := m.sessions[id]; sess != nil && sess.kind == "qr" {
		return sess.qrContent
	}
	return ""
}

// takeCredential 取走凭证并清掉会话。
func (m *loginManager) takeCredential(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// StartDevice 开一个浏览器环回授权会话。
func (m *loginManager) StartDevice(client *upstream.Client, callbackBase, appName string) (id, authURL string, err error) {
	device, err := upstream.NewDeviceLogin(client, callbackBase, appName)
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	m.sessions[device.State()] = &loginSession{kind: "device", device: device, createdAt: time.Now()}
	return device.State(), device.AuthURL(), nil
}

// AcceptCallback 收下浏览器送回的授权码。
func (m *loginManager) AcceptCallback(state, code string) error {
	m.mu.Lock()
	sess := m.sessions[state]
	m.mu.Unlock()
	if sess == nil || sess.kind != "device" || sess.device == nil {
		return errors.New("授权会话不存在或已过期，请回面板重新发起")
	}
	return sess.device.AcceptCallback(state, code)
}

// PollDevice 轮询环回授权是否已拿到 code 并换票完成。
func (m *loginManager) PollDevice(ctx context.Context, id string) (*auth.Auth, bool, error) {
	m.mu.Lock()
	sess := m.sessions[id]
	m.mu.Unlock()
	if sess == nil || sess.kind != "device" || sess.device == nil {
		return nil, false, errors.New("授权会话不存在或已过期，请重新发起")
	}
	creds, done, err := sess.device.Poll(ctx)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		return nil, false, err
	}
	if done && creds != nil {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		return creds, true, nil
	}
	return nil, false, nil
}

// ExchangeCode 直接用授权码换票（不经会话）。
func (m *loginManager) ExchangeCode(ctx context.Context, client *upstream.Client, code string) (*auth.Auth, error) {
	return client.PrepareAccount(ctx, map[string]any{"code": code})
}

// sessionsCount 返回进行中的会话数（面板状态展示）。
func (m *loginManager) sessionsCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	return len(m.sessions)
}
