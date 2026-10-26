// Package cosx 是腾讯云 COS 的最小上传客户端。
//
// 为什么不用官方 SDK：这个项目只有 go-qrcode 一个依赖，为了传一张图引入
// cos-go-sdk-v5 及其一串间接依赖不划算。COS 的 PUT Object 只需要一次
// HMAC-SHA1 签名，标准库就够——代价是签名细节必须严格照官方文档来，
// 所以下面的每一步都标了出处（阿里云 OSS / AWS SigV4 都是这个思路，
// COS 的差异只在 q-sign-algorithm 这套参数名）。
package cosx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Config 是 COS 连接的静态配置。
type Config struct {
	SecretID  string
	SecretKey string
	// Bucket 形如 mybucket-1250000000（含 APPID 后缀）。
	Bucket string
	// Region 形如 ap-guangzhou。
	Region string
	// Prefix 是对象键前缀（如 "xhx/"，空 = 放桶根）。
	Prefix string
	// BaseURL 是写进 url 的对外基址（CDN 域名 / 自定义域名）。
	// 空 = https://<bucket>.cos.<region>.myqcloud.com
	BaseURL string
	// PublicRead 上传时带 x-cos-acl: public-read。
	// 桶本身是公有读时可以不开，私有桶想直接给链接就得开。
	PublicRead bool
	// SignedTTL > 0 时返回带签名的临时 URL（适合私有桶 + 不想开公有读）。
	// 与 PublicRead 不冲突：开了 PublicRead 又设了 TTL，以 TTL 为准。
	SignedTTL time.Duration
}

// Client 是 COS 上传客户端。
type Client struct {
	cfg  Config
	host string
	http *http.Client
}

// New 校验配置并建客户端。
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.SecretID) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, fmt.Errorf("COS secret_id / secret_key 为空")
	}
	if strings.TrimSpace(cfg.Bucket) == "" || strings.TrimSpace(cfg.Region) == "" {
		return nil, fmt.Errorf("COS bucket / region 为空")
	}
	if p := strings.Trim(cfg.Prefix, "/"); p != "" {
		cfg.Prefix = p + "/"
	} else {
		cfg.Prefix = ""
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// 刻意不吃 HTTP(S)_PROXY。2026-10-26 实测：本地代理会把带
	// `Authorization: q-sign-algorithm=...` 的 PUT 直接 reset（GET 却能通），
	// 表现为「上传失败 + connection reset」，排查起来极费时间。
	// COS 是公网直连场景，保留环境代理只会多一层故障点。
	tr.Proxy = nil
	return &Client{
		cfg:  cfg,
		host: cfg.Bucket + ".cos." + cfg.Region + ".myqcloud.com",
		// 单张图最大 8MB 左右，60s 足够；超时由调用方的 ctx 兜底。
		http: &http.Client{Timeout: 60 * time.Second, Transport: tr},
	}, nil
}

// Put 上传对象并返回可访问的 URL。
//
// name 只放文件名（调用方已做内容寻址），Prefix 在这里拼。
func (c *Client) Put(ctx context.Context, name string, data []byte, contentType string) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("空数据")
	}
	key := c.cfg.Prefix + name
	path := "/" + key

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+c.host+path, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("构造 COS 请求失败: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// 参与签名的头必须与实际发送的一致，所以先收集再签。
	signed := map[string]string{"host": c.host}
	if c.cfg.PublicRead {
		signed["x-cos-acl"] = "public-read"
		req.Header.Set("x-cos-acl", "public-read")
	}
	req.ContentLength = int64(len(data))
	now := time.Now()
	req.Header.Set("Authorization", c.sign(http.MethodPut, path, signed, now, now.Add(time.Hour)))

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传 COS 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("COS 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return c.objectURL(key), nil
}

// objectURL 生成对外地址；配了 SignedTTL 就返回带签名的临时链接。
func (c *Client) objectURL(key string) string {
	base := strings.TrimRight(strings.TrimSpace(c.cfg.BaseURL), "/")
	if base == "" {
		base = "https://" + c.host
	}
	u := base + "/" + key
	if c.cfg.SignedTTL <= 0 {
		return u
	}
	// 签名有效期跟 SignedTTL 对齐：固定签 1 小时会出现「TTL 设 7 天、
	// 第 61 分钟就 403」的错位。
	now := time.Now()
	auth := c.sign(http.MethodGet, "/"+key, map[string]string{"host": c.host}, now, now.Add(c.cfg.SignedTTL))
	return u + "?" + auth
}

// sign 生成 COS 的 q-sign-algorithm 授权串。
//
// 流程（与官方文档一致，缺一步就会 403 SignatureDoesNotMatch）：
//  1. KeyTime  = "<start>;<end>"（Unix 秒）
//  2. SignKey  = hex(HMAC-SHA1(SecretKey, KeyTime))
//  3. HttpString = method\npath\nurlparams\nheaders\n
//  4. StringToSign = "sha1\n" + KeyTime + "\n" + hex(SHA1(HttpString)) + "\n"
//  5. Signature = hex(HMAC-SHA1(SignKey, StringToSign))
//
// 注意 SignKey 是「hex 字符串」再作为 HMAC 的 key，不是解码后的字节。
func (c *Client) sign(method, path string, headers map[string]string, start, end time.Time) string {
	keyTime := fmt.Sprintf("%d;%d", start.Unix(), end.Unix())
	signKey := hmacSHA1Hex(c.cfg.SecretKey, keyTime)
	headerList, headerStr := canonicalHeaders(headers)
	httpString := strings.ToLower(method) + "\n" + path + "\n" + "" + "\n" + headerStr + "\n"
	stringToSign := "sha1\n" + keyTime + "\n" + sha1Hex(httpString) + "\n"
	signature := hmacSHA1Hex(signKey, stringToSign)
	return fmt.Sprintf(
		"q-sign-algorithm=sha1&q-ak=%s&q-sign-time=%s&q-key-time=%s&q-header-list=%s&q-url-param-list=&q-signature=%s",
		c.cfg.SecretID, keyTime, keyTime, headerList, signature)
}

// canonicalHeaders 把参与签名的头按「小写键名 + 字典序」拼成 query 串。
func canonicalHeaders(headers map[string]string) (string, string) {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+url.QueryEscape(headers[k]))
	}
	return strings.Join(keys, ";"), strings.Join(parts, "&")
}

func hmacSHA1Hex(key, data string) string {
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func sha1Hex(data string) string {
	sum := sha1.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}
