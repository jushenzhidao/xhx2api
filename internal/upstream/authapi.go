// authapi.go 登录通道：手机号验证码 + 用户资料。
//
// 口径来自官方桌面端（/Applications/商汤小浣熊.app 内
// build/electron/main/client/account/accountRequests.js，2026-10-26 从 asar 解出）：
//
//	POST {auth}/send_sms        {nation_code, phone(密文), captcha_param}
//	POST {auth}/login_with_sms  {nation_code, phone(密文), sms_code, utm_source, inviter_user_id}
//
// 另有网页端在用的 IAM 通道（见 IamBase）：/sms/send + /auth/login/sms，
// 单据更干净（verification_id），但**不支持邀请归因字段**。
//
//	POST {auth}/login_with_password {nation_code, phone(密文), password(密文)}
//	GET  {auth}/user_info       → {id, name, phone, ...}
//
// 三个要点，都是实测确认过的：
//
//  1. **phone/password 必须加密**。明文直接 400 `params_encryted_error`
//     （"param phone encrypted error"）。算法在网页登录页里（chunks/469 的
//     `encryptCredential`）：AES-CFB + 随机 16 字节 IV，输出 base64(iv‖密文)。
//     密钥放在登录页内联配置的 `config.publicKey`——名字叫 publicKey，其实
//     是 16 字节对称密钥（CryptoJS 要求 key 为 16/24/32 字节）。
//  2. **send_sms 的 captcha_param 是阿里云验证码**（登录页 config.captcha:
//     scene=1pkmy0x3, prefix=hk1r5l, region=cn）。服务端强制校验：实测假票
//     返回 100006 `captcha_verify_error` + `{"captcha_result":false,"sms_result":false}`
//     ——也就是说这条链路绕不开那个滑块。所以短信登录的完整形态是
//     「面板里跑一次阿里云控件拿到 captchaVerifyParam」→ 发码 → 填码登录。
//  3. **IAM 票没有 refresh_token**。桌面端拿到的 access_token 是 IAM 票
//     （iss=xiaohuanxiong.com/iam，30 天），响应里不带 refresh_token，
//     客户端也明确不做续期（`refresh()` 见到 IAM 票直接抛 unauthorized）。
//     所以这类号在网关里「到期即失效」，需要重新登录——不要以为是解析漏了。
package upstream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rsfree/xhx2api/internal/auth"
)

const (
	// SmsSendPath 发送短信验证码（需要阿里云验证码票据）。
	SmsSendPath = "/send_sms"
	// SmsLoginPath 验证码登录。
	SmsLoginPath = "/login_with_sms"
	// IAMBase 是网页端（当前主力）认证服务前缀。
	//
	// 2026-10-26 从站点 chunk 里读出来的口径：网页登录走 IAM，
	// 桌面端那套 /api/web/auth/v1 是旧通道。两者都能换到同一份账号，
	// 但 IAM 是站点自己正在用的，优先走它。
	IamBase = "/api/iam/v1"
	// IamSmsSendPath 发码，返回 verification_id（后续登录要用）。
	IamSmsSendPath = "/sms/send"
	// IamSmsLoginPath 验证码登录。
	IamSmsLoginPath = "/auth/login/sms"
	// IamConfirmPath 确认 IAM 会话（authBase 下的端点，不是 IamBase）。
	//
	// 手机号登录换出来的 IAM 票默认是「已签发、未确认」：user_info 与 chat 一律回
	// HTTP 400 `{"code":200901,"message":"iam_confirm_required"}`，账号在池子里
	// 看着正常，实际一次都调不通（2026-10-29 真实号踩到）。
	// 网页端在每个受保护请求前都会先 POST 它（站点 chunk 里
	// `fetch("/api/web/auth/v1/iam/confirm")`，body 空对象，拿到 {"code":0} 才继续）。
	IamConfirmPath = "/iam/confirm"
	// CredentialKey 是网页登录页内联配置里的 AES 密钥。
	//
	// 上游换密钥的话，改这里即可；`EncryptCredential` 会校验长度（16/24/32），
	// 换错了会立刻报错而不是发出一个必然失败的请求。
	CredentialKey = "senseraccoon2023"
)

var (
	// ErrCaptchaRejected 人机验证没通过（票据无效/过期/未填）。
	ErrCaptchaRejected = errors.New("人机验证未通过")
	// ErrSmsCodeRejected 短信验证码不正确或已过期。
	ErrSmsCodeRejected = errors.New("短信验证码不正确或已过期")
	// ErrPhoneRejected 手机号本身不被接受（格式/未注册等）。
	ErrPhoneRejected = errors.New("手机号不被接受")
)

// EncryptCredential 复刻网页端的 `encryptCredential(value, publicKey)`。
//
//	AES-128-CFB(key = utf8(publicKey), iv = 16 随机字节, 不填充)
//	输出 = base64(iv ‖ 密文)
//
// CryptoJS 的 `mode.CFB` 与 Go 的 cipher.NewCFBEncrypter 同为 CFB-128，
// 分块行为一致；这条已经用真实手机号验证过（换掉加密后错误码从
// params_encryted_error 变成 sms_code_verify_error，说明上游解出来了）。
func EncryptCredential(plain, key string) (string, error) {
	keyBytes := []byte(key)
	switch len(keyBytes) {
	case 16, 24, 32:
	default:
		return "", fmt.Errorf("加密密钥长度必须是 16/24/32 字节，当前 %d", len(keyBytes))
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return "", err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	src := []byte(plain)
	dst := make([]byte, len(src))
	cipher.NewCFBEncrypter(block, iv).XORKeyStream(dst, src)
	return base64.StdEncoding.EncodeToString(append(iv, dst...)), nil
}

// EncryptCredentialDefault 用内置密钥加密。
func EncryptCredentialDefault(plain string) (string, error) {
	return EncryptCredential(plain, CredentialKey)
}

// SmsSendResult 是 send_sms 的结果。
type SmsSendResult struct {
	// CaptchaOK 上游认为人机验证通过。
	CaptchaOK bool
	// Sent 短信已发出。
	Sent bool
	// VerificationID 是 IAM 发码后返回的凭证，登录时必须带上。
	VerificationID string
	// Via 说明这次走的哪条通道（"iam" 网页 / "desktop" 桌面端旧通道）。
	Via string
}

// SendSmsCode 给手机号发一条登录验证码。
//
// captchaParam 是阿里云验证码控件回调里的 captchaVerifyParam。上游强制校验，
// 传空或假值会得到 ErrCaptchaRejected（实测 100006）。
func (c *Client) SendSmsCode(ctx context.Context, nationCode, phone, captchaParam string, preferLegacy bool) (*SmsSendResult, error) {
	nation, err := normalizeNation(nationCode)
	if err != nil {
		return nil, err
	}
	phone = normalizePhone(phone)
	if phone == "" {
		return nil, fmt.Errorf("%w: 手机号为空", ErrPhoneRejected)
	}
	if strings.TrimSpace(captchaParam) == "" {
		// 不发请求：上游一定拒，省一次往返也让错误更直白。
		return nil, fmt.Errorf("%w: 缺少人机验证票据（captcha_param）", ErrCaptchaRejected)
	}
	encrypted, err := EncryptCredentialDefault(phone)
	if err != nil {
		return nil, err
	}
	// preferLegacy 由调用方决定：要记邀请归因时必须走桌面端通道，
	// **发码和验证必须在同一条通道上**——两条通道的验证码不通用。
	// 实测踩过：IAM 发码 + 桌面端验码，用户填对了也报 sms_code_verify_error。
	if !preferLegacy {
		// 先走 IAM（站点正在用的通道）。
		if data, ierr := c.iamCall(ctx, IamSmsSendPath, map[string]string{
			"nation_code":   nation,
			"phone":         encrypted,
			"captcha_token": strings.TrimSpace(captchaParam),
			"scene":         "login",
		}, "发送验证码"); ierr == nil {
			id := text(data, "verification_id")
			if id == "" {
				return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊未返回 verification_id"}
			}
			return &SmsSendResult{CaptchaOK: true, Sent: true, VerificationID: id, Via: "iam"}, nil
		} else if !isEndpointMissing(ierr) {
			return nil, ierr
		}
	}
	// 桌面端通道（要带邀请归因，或 IAM 端点不存在时）。
	data, err := c.smsCall(ctx, SmsSendPath, map[string]string{
		"nation_code":   nation,
		"phone":         encrypted,
		"captcha_param": strings.TrimSpace(captchaParam),
	}, "发送验证码")
	if err != nil {
		return nil, err
	}
	captchaOK, _ := data["captcha_result"].(bool)
	sent, _ := data["sms_result"].(bool)
	if !captchaOK && sent {
		// 上游自己的客户端认为这是不可能的响应，我们也不装作成功。
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊返回了矛盾的验证码结果（未过人机验证却已发送）"}
	}
	if !sent {
		return &SmsSendResult{CaptchaOK: captchaOK, Sent: false}, fmt.Errorf("%w: 上游未发送短信", ErrCaptchaRejected)
	}
	return &SmsSendResult{CaptchaOK: captchaOK, Sent: true, Via: "desktop"}, nil
}

// LoginWithSms 用手机号 + 验证码换票。
//
// 返回的 auth.Auth 里 UID 已经用 user_info 纠正成 office 域的数字 id
// （token 里的 sub 是 pu-… 的 IAM 主体，直接当 uid 会让同一个号在池子里出现两行）。
// 如果上游只给了 IAM 票，RefreshToken 为空——这类票本来就不能续期。
func (c *Client) LoginWithSms(ctx context.Context, nationCode, phone, code, verificationID, inviterUserID string) (*auth.Auth, error) {
	nation, err := normalizeNation(nationCode)
	if err != nil {
		return nil, err
	}
	phone = normalizePhone(phone)
	if phone == "" {
		return nil, fmt.Errorf("%w: 手机号为空", ErrPhoneRejected)
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return nil, fmt.Errorf("%w: 验证码应为 6 位数字", ErrSmsCodeRejected)
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return nil, fmt.Errorf("%w: 验证码应为 6 位数字", ErrSmsCodeRejected)
		}
	}
	encrypted, err := EncryptCredentialDefault(phone)
	if err != nil {
		return nil, err
	}
	// 带邀请人时**不能**走 IAM：它没有归因字段，注册会记成自然流量。
	// 这时优先走桌面端通道（它认 inviter_user_id），哪怕稍微旧一点。
	wantAttribution := strings.TrimSpace(inviterUserID) != ""
	if strings.TrimSpace(verificationID) != "" && !wantAttribution {
		if data, ierr := c.iamCall(ctx, IamSmsLoginPath, map[string]string{
			"nation_code":       nation,
			"phone":             encrypted,
			"verification_id":   strings.TrimSpace(verificationID),
			"verification_code": code,
		}, "短信登录"); ierr == nil {
			access := stripBearerToken(text(data, "access_token", "accessToken"))
			if access == "" {
				return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊短信登录未返回 access_token"}
			}
			// 不确认的 IAM 票后面每个请求都是 400 iam_confirm_required，
			// 与其把一张废票写进池子（uid 还会停在 pu-…），不如在这里说清。
			// 端点不存在时降级继续（老站点没有这一步）。
			if cerr := c.ConfirmIAM(ctx, access); cerr != nil && !isEndpointMissing(cerr) {
				return nil, fmt.Errorf("IAM 会话确认失败: %w", cerr)
			}
			return c.finishLogin(ctx, &auth.Auth{Data: auth.Data{
				AccessToken: access,
				Nickname:    text(data, "name", "nickname"),
				ExpiresAt:   auth.JWTExpiry(access),
				UID:         auth.JWTUserID(access),
			}})
		} else if !isEndpointMissing(ierr) {
			return nil, ierr
		}
	}
	// 桌面端通道支持邀请归因字段，IAM 不支持（它的 body 只有四个字段，
	// 多塞会被严格校验拒），所以归因只在回落路径上生效。
	inviter := strings.TrimSpace(inviterUserID)
	if inviter != "" {
		digits := true
		for _, r := range inviter {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if !digits {
			return nil, fmt.Errorf("%w: 邀请人 uid 必须是纯数字（实际 %q）", ErrPhoneRejected, inviter)
		}
	}
	data, err := c.smsCall(ctx, SmsLoginPath, map[string]string{
		"nation_code":     nation,
		"phone":           encrypted,
		"sms_code":        code,
		"utm_source":      "",
		"inviter_user_id": inviter,
	}, "短信登录")
	if err != nil {
		return nil, err
	}
	access := stripBearerToken(text(data, "access_token", "accessToken"))
	if access == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊短信登录未返回 access_token"}
	}
	account := &auth.Auth{Data: auth.Data{
		AccessToken:  access,
		RefreshToken: stripBearerToken(text(data, "refresh_token", "refreshToken")),
		Nickname:     text(data, "name", "nickname", "office_org_name"),
		ExpiresAt:    auth.JWTExpiry(access),
		UID:          auth.JWTUserID(access),
	}}
	return c.finishLogin(ctx, account)
}

// ConfirmIAM 把 IAM 票从「已签发」推进到「可用」。
//
// 幂等：已确认的票重复调用仍返回 {"code":0}（2026-10-29 实测连打三次）。
// 上游把失败也放在 HTTP 400 + 信封 code=200901 里，所以状态码和 code 都要判。
func (c *Client) ConfirmIAM(ctx context.Context, token string) error {
	token = stripBearerToken(token)
	if token == "" {
		return &Error{Kind: ErrClient, Status: 400, Msg: "token 为空"}
	}
	resp, err := c.doJSON(ctx, http.MethodPost, c.authBase()+IamConfirmPath, map[string]any{},
		map[string]string{"Accept": "application/json", "Authorization": "Bearer " + token})
	if err != nil {
		return err
	}
	var envelope struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
	}
	if jerr := json.Unmarshal(resp.Body, &envelope); jerr != nil {
		return &Error{Kind: ErrServer, Status: 502,
			Msg: "IAM 会话确认未返回有效 JSON: " + truncate(string(resp.Body), 160)}
	}
	if envelope.Code.String() == "0" && resp.Status >= 200 && resp.Status < 300 {
		return nil
	}
	msg := strings.TrimSpace(envelope.Message)
	if msg == "" {
		msg = fmt.Sprintf("IAM 会话确认失败（HTTP %d，code=%s）", resp.Status, envelope.Code.String())
	}
	return &Error{Kind: Classify(resp.Status, string(resp.Body)), Status: resp.Status, Msg: msg}
}

// isIAMConfirmRequired 判断错误是不是「IAM 票未确认」（上游 200901）。
//
// 只能在 iam/confirm 补确认；换号、重试、熔断都没意义，账号本身没坏。
func isIAMConfirmRequired(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "iam_confirm")
}

// finishLogin 用 user_info 补齐 uid/昵称，并做最后的合法性检查。
func (c *Client) finishLogin(ctx context.Context, account *auth.Auth) (*auth.Auth, error) {
	if uid, nick, _, err := c.GetUserInfo(ctx, account); err == nil {
		if uid != "" {
			account.UID = uid
		}
		if nick != "" {
			account.Nickname = nick
		}
	}
	if account.UID == "" {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: auth.ErrNoIdentity.Error()}
	}
	if account.Nickname == "" {
		account.Nickname = account.UID
	}
	return account, nil
}

// Profile 是 user_info 里网关关心的字段。
type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	NationCode  string `json:"nation_code"`
	Email       string `json:"email,omitempty"`
	PasswordSet bool   `json:"password_set"`
	WeChatBound bool   `json:"wechat_bound"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// FetchProfile 用裸 token 查资料（客户端目录扫描用：先确认票还活着，再拿真实 uid）。
func (c *Client) FetchProfile(ctx context.Context, token string) (*Profile, error) {
	token = stripBearerToken(token)
	if token == "" {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: "token 为空"}
	}
	resp, err := c.doJSON(ctx, http.MethodGet, c.authBase()+UserInfoPath, nil, map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer " + token,
	})
	if err != nil {
		return nil, err
	}
	data, err := payloadOf(resp, "用户资料")
	if err != nil {
		return nil, err
	}
	return &Profile{
		ID:          text(data, "id", "user_id", "uid"),
		Name:        text(data, "name", "nickname"),
		Phone:       text(data, "phone"),
		NationCode:  text(data, "nation_code"),
		Email:       text(data, "email"),
		PasswordSet: text(data, "password_status") != "unset",
		WeChatBound: boolOf(data["wechat_bound"]),
		CreatedAt:   text(data, "created_at"),
	}, nil
}

// authHeaders 是登录类请求的头：桌面端除 Content-Type 外只带 Authorization。
func (c *Client) authHeaders(a *auth.Auth) map[string]string {
	h := map[string]string{"Accept": "application/json"}
	if a != nil {
		h["Authorization"] = "Bearer " + a.JWT()
	}
	return h
}

// iamCall 调 IAM 接口。
//
// IAM 的信封与桌面端那套不一样：成功是 `{"code":"OK","data":…}`（**字符串**码），
// 失败是 `{"code":"COMMON_INVALID_ARGUMENT","message":…}` 且常配 HTTP 400。
// 所以不能复用 smsCall。
func (c *Client) iamCall(ctx context.Context, path string, body any, action string) (map[string]any, error) {
	_, origin := c.Origins()
	if origin == "" {
		origin = Origin
	}
	resp, err := c.doJSON(ctx, http.MethodPost, strings.TrimRight(origin, "/")+IamBase+path, body,
		map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(resp.Body, &envelope); jerr != nil {
		return nil, &Error{Kind: ErrServer, Status: 502,
			Msg: fmt.Sprintf("小浣熊 %s未返回有效 JSON: %s", action, truncate(string(resp.Body), 160))}
	}
	if envelope.Code != "" && envelope.Code != "OK" {
		return nil, iamBusinessError(envelope.Code, envelope.Message)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, &Error{Kind: Classify(resp.Status, string(resp.Body)), Status: resp.Status,
			Msg: fmt.Sprintf("小浣熊 %s失败（HTTP %d）", action, resp.Status)}
	}
	var data map[string]any
	if len(envelope.Data) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(envelope.Data))
		decoder.UseNumber()
		_ = decoder.Decode(&data)
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// iamBusinessError 把 IAM 的错误码/文案翻成哨兵错误。
//
// IAM 的 code 是粗粒度字符串（COMMON_INVALID_ARGUMENT 什么都用），真正有信息量的是
// message（"captcha_token invalid" / "verification code invalid"），所以按文案判。
func iamBusinessError(code, message string) error {
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = code
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "captcha"):
		return fmt.Errorf("%w: %s", ErrCaptchaRejected, msg)
	case strings.Contains(lower, "verification") || strings.Contains(lower, "code"):
		return fmt.Errorf("%w: %s", ErrSmsCodeRejected, msg)
	case strings.Contains(lower, "phone"):
		return fmt.Errorf("%w: %s", ErrPhoneRejected, msg)
	case strings.HasPrefix(code, "COMMON_RATE") || code == "TOO_MANY_REQUESTS":
		return fmt.Errorf("%w: %s", ErrSmsCodeRejected, msg)
	}
	return &Error{Kind: ErrClient, Status: 400, Msg: msg}
}

// isEndpointMissing 判断错误是不是「这条路径不存在」——用于在老部署上回退旧通道。
// 只有明确的 404/405 才回退：429/5xx 回退会变成双重发码。
func isEndpointMissing(err error) bool {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusMethodNotAllowed
	}
	return false
}

// smsCall 发一个登录类请求并按业务码翻译错误。
//
// 不能直接用 payloadOf：它对非 2xx **先看状态码**，而上游把
// 100006 captcha_verify_error 这类业务错误也用 HTTP 400 返回，于是
// payloadOf 只会给出「HTTP 400」并把业务码丢掉——面板上就变成一句看不懂的
// 「upstream client (http 400)」，用户不知道该重过验证码还是该重填手机号。
// 这里先解信封里的 code，再退回状态码。
func (c *Client) smsCall(ctx context.Context, path string, body any, action string) (map[string]any, error) {
	resp, err := c.doJSON(ctx, http.MethodPost, c.authBase()+path, body, c.authHeaders(nil))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Details string          `json:"details"`
		Data    json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(resp.Body, &envelope); jerr != nil {
		return nil, &Error{Kind: ErrServer, Status: 502,
			Msg: fmt.Sprintf("小浣熊 %s未返回有效 JSON: %s", action, truncate(string(resp.Body), 160))}
	}
	if envelope.Code != 0 {
		return nil, smsBusinessError(envelope.Code, envelope.Message, envelope.Details)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, &Error{Kind: Classify(resp.Status, string(resp.Body)), Status: resp.Status,
			Msg: fmt.Sprintf("小浣熊 %s失败（HTTP %d）", action, resp.Status)}
	}
	var data map[string]any
	if len(envelope.Data) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(envelope.Data))
		decoder.UseNumber()
		_ = decoder.Decode(&data)
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// smsBusinessError 把业务码翻译成可行动的哨兵错误。
//
// 码值来自上游 auth 服务：桌面端 accountRequests.js 里明确列了
// 200003/200009 = unauthorized、200901-200905 = service；下面这三个是实测出来的。
func smsBusinessError(code int, message, details string) error {
	base := strings.TrimSpace(message)
	if base == "" {
		base = fmt.Sprintf("上游返回 code=%d", code)
	}
	if detail := strings.TrimSpace(details); detail != "" && !strings.Contains(base, detail) {
		base = base + "（" + detail + "）"
	}
	switch code {
	case 100006:
		return fmt.Errorf("%w: %s", ErrCaptchaRejected, base)
	case 100007:
		return fmt.Errorf("%w: %s", ErrSmsCodeRejected, base)
	case 100002, 100003:
		return fmt.Errorf("%w: %s", ErrPhoneRejected, base)
	}
	return &Error{Kind: ErrClient, Status: 400, Msg: base}
}

// normalizeNation 归一国家码（默认 86）。
func normalizeNation(raw string) (string, error) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "+"))
	if raw == "" {
		return "86", nil
	}
	if len(raw) > 4 {
		return "", fmt.Errorf("%w: 国家码最多 4 位", ErrPhoneRejected)
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%w: 国家码只能是数字", ErrPhoneRejected)
		}
	}
	return raw, nil
}

// normalizePhone 去掉空格与连字符（网页端也是这么处理的）。
func normalizePhone(raw string) string {
	replacer := strings.NewReplacer(" ", "", "-", "", "\t", "")
	return strings.TrimSpace(replacer.Replace(raw))
}

// boolOf 读 JSON 布尔（上游有时给 bool，有时给 0/1 或 "true"）。
func boolOf(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case int64:
		return v != 0
	case string:
		return v == "true" || v == "1"
	}
	return false
}
