// Package clientdir 在本机找「商汤小浣熊」桌面客户端留下的登录凭证。
//
// 客户端（Electron + 内置 box-agent）把票明文写在：
//
//	macOS    ~/Library/Application Support/office-raccoon/agent/config/auth.json
//	Windows  %APPDATA%\office-raccoon\agent\config\auth.json
//	         %LOCALAPPDATA%\office-raccoon\agent\config\auth.json
//	Linux    $XDG_CONFIG_HOME/office-raccoon/agent/config/auth.json
//	         或 ~/.config/office-raccoon/agent/config/auth.json
//
// 内容是 {"access_token":"eyJ…","office_identity":"personal"}，只有 access_token，
// **没有 refresh_token**：这是 IAM 票（iss=xiaohuanxiong.com/iam，30 天），
// 官方客户端自己也不续期，到期就重新登录。所以扫出来的号能用一个月，
// 到期必须重新扫一次——面板会把到期日显示出来，别当成 bug。
//
// 这个包只做**读盘与解析**，不联网。是否真的可用由面板拿着 token 打一次
// user_info 判断（顺便把 pu-… 的 IAM 主体换成 office 域的数字 uid）。
package clientdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/rsfree/xhx2api/internal/auth"
)

// Source 标识候选路径来自哪个平台/来源。
type Source string

const (
	SourceMacOS   Source = "macos"
	SourceWindows Source = "windows"
	SourceLinux   Source = "linux"
	SourceLegacy  Source = "legacy"
	SourceCustom  Source = "custom"
)

// Location 是一个待扫描的位置。
type Location struct {
	Path   string `json:"path"`
	Source Source `json:"source"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size,omitempty"`
}

// Candidate 是从一个文件里读出来的凭证。
type Candidate struct {
	Path string `json:"path"`
	// Source 同 Location.Source。
	Source Source `json:"source"`
	// Token 是原始 access_token（json:"-"：面板不回传，避免票在浏览器里多处留痕）。
	Token string `json:"-"`
	// TokenMasked 是给人看的掩码。
	TokenMasked string `json:"token_masked"`
	// UID/Nickname/Phone 由面板联网补齐（user_info），扫描阶段为空。
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Phone    string `json:"phone,omitempty"`
	// ExpiresAt 是 token 的到期时刻（unix 毫秒，来自 JWT exp）。
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// RefreshToken 为真说明这个文件里还带了 refresh_token（老版本客户端/我们自己导出的）。
	RefreshToken bool `json:"has_refresh"`
	// Note 是给人看的说明（例如「IAM 票，不续期」）。
	Note string `json:"note,omitempty"`
	// Err 非空表示这个文件读/解析失败。
	Err string `json:"error,omitempty"`
}

// Locations 返回本机所有已知的凭证位置（不判断文件是否存在）。
//
// extra 是用户自己配的路径（可以是文件，也可以是目录；目录会展开成下面几个
// 常见相对位置）。服务器部署时本机没有客户端，这个列表自然是空的——那是正常的。
func Locations(extra []string) []Location {
	var out []Location
	home, _ := os.UserHomeDir()
	add := func(path string, source Source) {
		if path == "" {
			return
		}
		path = filepath.Clean(path)
		for _, existing := range out {
			if existing.Path == path {
				return
			}
		}
		loc := Location{Path: path, Source: source}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			loc.Exists = true
			loc.Size = info.Size()
		}
		out = append(out, loc)
	}

	switch runtime.GOOS {
	case "darwin":
		if home != "" {
			add(filepath.Join(home, "Library", "Application Support", "office-raccoon", "agent", "config", "auth.json"), SourceMacOS)
		}
	case "windows":
		for _, env := range []string{"APPDATA", "LOCALAPPDATA"} {
			if base := os.Getenv(env); base != "" {
				add(filepath.Join(base, "office-raccoon", "agent", "config", "auth.json"), SourceWindows)
			}
		}
	default:
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" && home != "" {
			base = filepath.Join(home, ".config")
		}
		if base != "" {
			add(filepath.Join(base, "office-raccoon", "agent", "config", "auth.json"), SourceLinux)
		}
	}
	// 更早的独立 agent 目录（有些版本会把 auth.json 放这里）。
	if home != "" {
		add(filepath.Join(home, ".box-agent", "config", "auth.json"), SourceLegacy)
		add(filepath.Join(home, ".box-agent", "auth.json"), SourceLegacy)
	}
	for _, raw := range extra {
		raw = ExpandPath(raw)
		if raw == "" {
			continue
		}
		info, err := os.Stat(raw)
		if err != nil {
			add(raw, SourceCustom)
			continue
		}
		if info.IsDir() {
			for _, sub := range []string{
				filepath.Join("agent", "config", "auth.json"),
				filepath.Join("config", "auth.json"),
				"auth.json",
			} {
				add(filepath.Join(raw, sub), SourceCustom)
			}
			continue
		}
		add(raw, SourceCustom)
	}
	return out
}

// Scan 读取给定的文件路径，返回解析出来的候选（读不出来的也会返回，带 Err）。
//
// 只处理存在的普通文件；目录会被忽略（由 Locations 负责展开）。
func Scan(paths []string) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		cand := scanFile(path)
		out = append(out, cand)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func scanFile(path string) Candidate {
	cand := Candidate{Path: path, Source: SourceCustom}
	raw, err := os.ReadFile(path)
	if err != nil {
		cand.Err = "读取失败: " + err.Error()
		return cand
	}
	token, refresh, uid, nick, err := parseCredential(raw)
	if err != nil {
		cand.Err = err.Error()
		return cand
	}
	cand.Token = token
	cand.TokenMasked = auth.MaskToken(token)
	cand.UID = uid
	cand.Nickname = nick
	cand.RefreshToken = refresh != ""
	cand.ExpiresAt = auth.JWTExpiry(token)
	if cand.ExpiresAt > 0 && !cand.RefreshToken {
		cand.Note = "IAM 票（无 refresh_token），到期需重新导入"
	}
	return cand
}

// parseCredential 兼容三种落盘形状：
//
//	{"access_token":…,"office_identity":…}   官方客户端
//	{"uid":…,"nickname":…,"access_token":…}  网关自己的账号文件
//	{"accessToken":…}                        驼峰变体
func parseCredential(raw []byte) (token, refresh, uid, nick string, err error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", "", "", errors.New("不是 JSON 对象：" + err.Error())
	}
	pick := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := payload[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	token = pick("access_token", "accessToken", "token", "accessTokenValue")
	refresh = pick("refresh_token", "refreshToken")
	uid = pick("uid", "user_id", "userId", "id")
	nick = pick("nickname", "name")
	if token == "" && refresh == "" {
		return "", "", "", "", errors.New("文件里没有 access_token / refresh_token")
	}
	if token == "" {
		return "", "", "", "", errors.New("只有 refresh_token，无法离线验证；请改用粘贴导入")
	}
	if uid == "" {
		uid = auth.JWTUserID(token)
	}
	return token, refresh, uid, nick, nil
}

// ExpandPath 展开 ~ 与相对路径（相对当前工作目录）。
func ExpandPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`) {
		home, err := os.UserHomeDir()
		if err == nil {
			raw = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), string(os.PathSeparator)))
		}
	}
	if abs, err := filepath.Abs(raw); err == nil {
		return abs
	}
	return raw
}
