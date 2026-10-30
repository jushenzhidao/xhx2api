package upstream

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEncryptCredentialRoundTrip 守卫加密口径：base64(iv‖AES-CFB 密文)，无填充。
//
// 这条格式是上游唯一接受的形状——真号实测：换成明文立刻 400
// `params_encryted_error`，换成这个形状后同样的手机号能走到
// `sms_code_verify_error`（说明上游解出来了）。所以谁改这里，测试必须先红。
func TestEncryptCredentialRoundTrip(t *testing.T) {
	const phone = "13800000000"
	enc, err := EncryptCredentialDefault(phone)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("输出不是标准 base64: %v", err)
	}
	// CFB 不改变长度：16 字节 IV + 明文长度。
	if len(raw) != 16+len(phone) {
		t.Fatalf("长度 %d，期望 %d（16 IV + 明文，CFB 不填充）", len(raw), 16+len(phone))
	}
	iv, ciphertext := raw[:16], raw[16:]
	plain, err := decryptCFB(ciphertext, iv, CredentialKey)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if plain != phone {
		t.Fatalf("往返结果 %q，期望 %q", plain, phone)
	}
}

// TestEncryptCredentialKeyLength 密钥长度必须是 16/24/32（CryptoJS 的限制）。
// 换密钥时写错长度要立刻报错，而不是发一个必然被拒的请求。
func TestEncryptCredentialKeyLength(t *testing.T) {
	for _, key := range []string{"", "short", "0123456789abcdef", "0123456789abcdef01234567", "0123456789abcdef0123456789abcdef"} {
		_, err := EncryptCredential("x", key)
		ok := len(key) == 16 || len(key) == 24 || len(key) == 32
		if ok && err != nil {
			t.Fatalf("key 长度 %d 应当可用: %v", len(key), err)
		}
		if !ok && err == nil {
			t.Fatalf("key 长度 %d 应当报错", len(key))
		}
	}
}

// TestEncryptCredentialRandomIV 同一明文两次加密必须不同（否则 IV 复用了）。
func TestEncryptCredentialRandomIV(t *testing.T) {
	first, err1 := EncryptCredentialDefault("13800000000")
	second, err2 := EncryptCredentialDefault("13800000000")
	if err1 != nil || err2 != nil {
		t.Fatalf("加密失败: %v / %v", err1, err2)
	}
	if first == second {
		t.Fatal("两次加密结果相同：IV 没有随机化")
	}
}

// TestSendSmsRequiresCaptcha 空的 captcha_param 应当本地拒绝（不发请求）。
// 上游一定拒（实测 100006），本地拦掉能省一次往返，也让错误更直白。
func TestSendSmsRequiresCaptcha(t *testing.T) {
	c := New()
	if _, err := c.SendSmsCode(context.Background(), "86", "13800000000", "  ", false); err == nil {
		t.Fatal("空 captcha_param 应当报错")
	} else if !strings.Contains(err.Error(), "人机验证") {
		t.Fatalf("错误信息应提到人机验证: %v", err)
	}
}

func TestNormalizeNationAndPhone(t *testing.T) {
	if got, err := normalizeNation("+86"); err != nil || got != "86" {
		t.Fatalf("normalizeNation(+86) = %q, %v", got, err)
	}
	if got, err := normalizeNation(""); err != nil || got != "86" {
		t.Fatalf("空国家码应默认 86，得到 %q, %v", got, err)
	}
	if _, err := normalizeNation("86abc"); err == nil {
		t.Fatal("非数字国家码应当报错")
	}
	if got := normalizePhone(" 138 0000-0000 "); got != "13800000000" {
		t.Fatalf("normalizePhone = %q", got)
	}
}

// TestSmsBusinessError 守卫业务码 → 哨兵错误的映射。
//
// 这张表是面板「一句话说清该干什么」的依据：100006 要重过人机验证，
// 100007 要重填验证码，100002/100003 要改手机号。全归成一句
// "upstream client (http 400)" 的话，用户只能猜。
func TestSmsBusinessError(t *testing.T) {
	cases := []struct {
		code  int
		want  error
		match string
	}{
		{100006, ErrCaptchaRejected, "captcha verify failed"},
		{100007, ErrSmsCodeRejected, "sms code verify failed"},
		{100002, ErrPhoneRejected, "param phone invalid"},
	}
	for _, tc := range cases {
		err := smsBusinessError(tc.code, "some_error", tc.match)
		if !errors.Is(err, tc.want) {
			t.Fatalf("code=%d 应映射到 %v，得到 %v", tc.code, tc.want, err)
		}
		if !strings.Contains(err.Error(), tc.match) {
			t.Fatalf("code=%d 错误信息丢了 details: %v", tc.code, err)
		}
	}
	// 未知码：保留原文，归 client（不是账号问题）。
	other := smsBusinessError(123456, "whatever", "")
	var apiErr *Error
	if !errors.As(other, &apiErr) || apiErr.Kind != ErrClient {
		t.Fatalf("未知码应归 ErrClient，得到 %v", other)
	}
	if !strings.Contains(other.Error(), "whatever") {
		t.Fatalf("未知码也要保留上游原文: %v", other)
	}
}

// TestSendSmsChannelSelection 守卫「发码通道可切换」。
//
// 这条不变量很关键：上游 IAM 与桌面端两条通道的验证码**互不通用**，
// 而只有桌面端认邀请人。所以「带邀请人」必须从发码起就走桌面端，
// 否则用户填对了码也会被拒（实测踩过）。
func TestSendSmsChannelSelection(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/iam/") {
			fmt.Fprint(w, `{"code":"OK","message":"success","data":{"verification_id":"v-1","expires_in":300,"retry_after":60}}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"message":"success","data":{"captcha_result":true,"sms_result":true}}`)
	}))
	defer srv.Close()

	newClient := func() *Client {
		c := New()
		c.SetAgentHosts(srv.URL, srv.URL+"/api/web/auth/v1")
		c.SetAPIHosts(srv.URL, srv.URL)
		return c
	}

	// 默认：走 IAM，拿到 verification_id。
	c := newClient()
	res, err := c.SendSmsCode(context.Background(), "86", "13800000000", "ticket", false)
	if err != nil {
		t.Fatalf("IAM 发码失败: %v", err)
	}
	if res.Via != "iam" || res.VerificationID != "v-1" {
		t.Fatalf("IAM 结果不对: %+v", res)
	}
	if len(paths) == 0 || paths[len(paths)-1] != "/api/iam/v1/sms/send" {
		t.Fatalf("没走到 IAM 端点: %v", paths)
	}

	// 带邀请人（preferLegacy）：必须走桌面端，且不能带 verification_id。
	paths = nil
	c = newClient()
	res, err = c.SendSmsCode(context.Background(), "86", "13800000000", "ticket", true)
	if err != nil {
		t.Fatalf("桌面端发码失败: %v", err)
	}
	if res.Via != "desktop" {
		t.Fatalf("应当走桌面端，得到 %+v", res)
	}
	if res.VerificationID != "" {
		t.Fatalf("桌面端不该有 verification_id: %q", res.VerificationID)
	}
	if len(paths) == 0 || paths[len(paths)-1] != "/api/web/auth/v1/send_sms" {
		t.Fatalf("没走到桌面端端点: %v", paths)
	}
}

// TestLoginWithSmsRejectsBadInviter 邀请人必须是纯数字（上游只认 uid 数字）。
func TestLoginWithSmsRejectsBadInviter(t *testing.T) {
	c := New()
	c.SetAgentHosts("http://127.0.0.1:1", "http://127.0.0.1:1/api/web/auth/v1")
	c.SetAPIHosts("http://127.0.0.1:1", "http://127.0.0.1:1")
	_, err := c.LoginWithSms(context.Background(), "86", "13800000000", "123456", "", "abc")
	if err == nil || !errors.Is(err, ErrPhoneRejected) {
		t.Fatalf("非数字邀请人应当被本地拒绝，得到 %v", err)
	}
}

// TestConfirmIAM 守住 IAM 会话确认的口径。
//
// 上游失败也放 HTTP 400 + 信封（code=200901 iam_confirm_required），
// 所以状态码和 code 必须同时判：只看状态码会把 code=0 的 4xx 当失败，
// 只看 code 会把 400 当成功。
func TestConfirmIAM(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"已确认/幂等", 200, `{"code":0,"message":"success"}`, ""},
		{"未确认", 400, `{"code":200901,"message":"iam_confirm_required","details":"IAM confirmation required"}`, "iam_confirm_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/web/auth/v1/iam/confirm" {
					t.Errorf("端点不对: %s", r.URL.Path)
				}
				if r.Method != http.MethodPost {
					t.Errorf("方法不对: %s", r.Method)
				}
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			c := New()
			c.SetAgentHosts(srv.URL, srv.URL+"/api/web/auth/v1")
			c.SetAPIHosts(srv.URL, srv.URL)

			err := c.ConfirmIAM(context.Background(), "tok-123")
			if gotAuth != "Bearer tok-123" {
				t.Fatalf("Authorization 没带上: %q", gotAuth)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应当成功: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应当报 %q，实际 %v", tc.wantErr, err)
			}
		})
	}
}

// TestLoginWithSmsConfirmsIAMBeforeProfile 守卫登录顺序：IAM 发码通道换到票后，
// 必须**先** iam/confirm 再 user_info。少了这一步，票是未确认态：user_info 也 400，
// uid 停在 token 里的 pu-… 主体上，账号进池子后每个请求都 400 iam_confirm_required
// （2026-10-29 真实号复现）。顺序反了同样白搭，所以这里连顺序一起断言。
func TestLoginWithSmsConfirmsIAMBeforeProfile(t *testing.T) {
	fakeJWT := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"sub":"pu-01a18645","exp":1795846062}`)) + ".sig"

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/iam/v1/auth/login/sms":
			fmt.Fprintf(w, `{"code":"OK","message":"success","data":{"access_token":%q}}`, fakeJWT)
		case "/api/web/auth/v1/iam/confirm":
			fmt.Fprint(w, `{"code":0,"message":"success"}`)
		case "/api/web/auth/v1/user_info":
			fmt.Fprint(w, `{"code":0,"message":"success","data":{"id":"7815106","name":"RaccoonEmily"}}`)
		default:
			t.Errorf("意外端点: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New()
	c.SetAgentHosts(srv.URL, srv.URL+"/api/web/auth/v1")
	c.SetAPIHosts(srv.URL, srv.URL)

	a, err := c.LoginWithSms(context.Background(), "86", "13800000000", "123456", "v-1", "")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if a.UID != "7815106" {
		t.Fatalf("uid 应当被 user_info 纠正成数字，实际 %q", a.UID)
	}
	idx := func(p string) int {
		for i, v := range paths {
			if v == p {
				return i
			}
		}
		return -1
	}
	confirm, profile := idx("/api/web/auth/v1/iam/confirm"), idx("/api/web/auth/v1/user_info")
	if confirm < 0 {
		t.Fatalf("没调 iam/confirm: %v", paths)
	}
	if profile < 0 || confirm > profile {
		t.Fatalf("必须先 confirm 再 user_info，实际 %v", paths)
	}
}

// TestPrepareAccountConfirmsUnconfirmedIAM 裸 token 导入时，未确认的 IAM 票
// 要先补一次 iam/confirm 再取资料——否则 uid 停在 pu-…，入池后每个请求都 400。
func TestPrepareAccountConfirmsUnconfirmedIAM(t *testing.T) {
	fakeJWT := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"sub":"pu-01a18645","exp":1795846062}`)) + ".sig"

	confirmed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/web/auth/v1/iam/confirm":
			confirmed = true
			fmt.Fprint(w, `{"code":0,"message":"success"}`)
		case "/api/web/auth/v1/user_info":
			if !confirmed {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"code":200901,"message":"iam_confirm_required","details":"IAM confirmation required"}`)
				return
			}
			fmt.Fprint(w, `{"code":0,"message":"success","data":{"id":"7815106","name":"RaccoonEmily"}}`)
		default:
			t.Errorf("意外端点: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New()
	c.SetAgentHosts(srv.URL, srv.URL+"/api/web/auth/v1")
	c.SetAPIHosts(srv.URL, srv.URL)

	a, err := c.PrepareAccount(context.Background(), map[string]any{"access_token": fakeJWT})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if a.UID != "7815106" {
		t.Fatalf("uid 应当纠正成数字，实际 %q（说明没走补确认）", a.UID)
	}
}

// decryptCFB 是测试用的反向实现（生产代码只加密不解密）。
func decryptCFB(ciphertext, iv []byte, key string) (string, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	out := make([]byte, len(ciphertext))
	cipher.NewCFBDecrypter(block, iv).XORKeyStream(out, ciphertext)
	return string(out), nil
}
