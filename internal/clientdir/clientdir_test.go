package clientdir

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeJWT 造一个只有 payload 有意义的 JWT（exp/sub 够 auth 包解析就行）。
func fakeJWT(t *testing.T, sub string, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"sub": sub, "exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestScanOfficialClientFile 守卫官方客户端的落盘形状：
// {"access_token":"…","office_identity":"personal"}，没有 refresh_token。
func TestScanOfficialClientFile(t *testing.T) {
	dir := t.TempDir()
	token := fakeJWT(t, "pu-01a175df-cf74-7cd1-affc-7ead195a7362", time.Now().Add(30*24*time.Hour))
	path := writeTemp(t, dir, "auth.json", `{"access_token":"`+token+`","office_identity":"personal"}`)

	cands := Scan([]string{path})
	if len(cands) != 1 {
		t.Fatalf("扫到 %d 个候选", len(cands))
	}
	cand := cands[0]
	if cand.Err != "" {
		t.Fatalf("解析失败: %s", cand.Err)
	}
	if cand.Token != token {
		t.Fatal("token 没有原样读出")
	}
	if cand.RefreshToken {
		t.Fatal("官方客户端文件里没有 refresh_token，不该标成有")
	}
	if cand.ExpiresAt == 0 {
		t.Fatal("没有从 JWT exp 推出到期时刻")
	}
	if cand.TokenMasked == "" || strings.Contains(cand.TokenMasked, token[10:30]) {
		t.Fatalf("掩码没遮住: %q", cand.TokenMasked)
	}
	if cand.Note == "" {
		t.Fatal("无 refresh_token 的 IAM 票应当有说明（到期需重新导入）")
	}
}

// TestScanGatewayAccountFile 兼容网关自己的账号文件（带 uid/nickname/refresh）。
func TestScanGatewayAccountFile(t *testing.T) {
	dir := t.TempDir()
	token := fakeJWT(t, "7772869", time.Now().Add(24*time.Hour))
	path := writeTemp(t, dir, "7772869.json",
		`{"uid":"7772869","nickname":"RaccoonAva","access_token":"`+token+`","refresh_token":"r1"}`)

	cands := Scan([]string{path})
	if len(cands) != 1 || cands[0].Err != "" {
		t.Fatalf("解析失败: %+v", cands)
	}
	if cands[0].UID != "7772869" || cands[0].Nickname != "RaccoonAva" {
		t.Fatalf("uid/nickname 没读出来: %+v", cands[0])
	}
	if !cands[0].RefreshToken {
		t.Fatal("带 refresh_token 的文件应当标记")
	}
}

func TestScanBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"not-json.json":     `not json at all`,
		"empty.json":        `{}`,
		"only-refresh.json": `{"refresh_token":"r"}`,
	}
	paths := make([]string, 0, len(cases))
	for name, content := range cases {
		paths = append(paths, writeTemp(t, dir, name, content))
	}
	cands := Scan(paths)
	if len(cands) != len(cases) {
		t.Fatalf("扫到 %d 个候选，期望 %d", len(cands), len(cases))
	}
	for _, cand := range cands {
		if cand.Err == "" {
			t.Fatalf("%s 应当带错误而不是静默成功", cand.Path)
		}
	}
}

// TestScanSkipsMissingAndDirs 不存在的路径与目录都要跳过（目录由 Locations 展开）。
func TestScanSkipsMissingAndDirs(t *testing.T) {
	dir := t.TempDir()
	cands := Scan([]string{filepath.Join(dir, "nope.json"), dir})
	if len(cands) != 0 {
		t.Fatalf("不该产出候选: %+v", cands)
	}
}

// TestLocationsIncludesPlatformDefault 平台默认位置必须在列表里，
// 否则「一键扫描」在 macOS 上根本找不到客户端的 auth.json。
func TestLocationsIncludesPlatformDefault(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无 home 目录")
	}
	locs := Locations(nil)
	var want string
	switch runtime.GOOS {
	case "darwin":
		want = filepath.Join(home, "Library", "Application Support", "office-raccoon", "agent", "config", "auth.json")
	case "windows":
		want = filepath.Join(os.Getenv("APPDATA"), "office-raccoon", "agent", "config", "auth.json")
	default:
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		want = filepath.Join(base, "office-raccoon", "agent", "config", "auth.json")
	}
	for _, loc := range locs {
		if loc.Path == want {
			return
		}
	}
	t.Fatalf("默认位置列表里没有 %s: %+v", want, locs)
}

// TestLocationsExpandsCustomDir 用户配了目录时，要展开成下面几个常见相对位置。
func TestLocationsExpandsCustomDir(t *testing.T) {
	dir := t.TempDir()
	locs := Locations([]string{dir})
	want := filepath.Join(dir, "agent", "config", "auth.json")
	for _, loc := range locs {
		if loc.Path == want {
			return
		}
	}
	t.Fatalf("目录没有展开出 %s: %+v", want, locs)
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	if home != "" {
		if got := ExpandPath("~/Library/x"); got != filepath.Join(home, "Library", "x") {
			t.Fatalf("ExpandPath(~/Library/x) = %q", got)
		}
	}
	if got := ExpandPath("relative/path"); !filepath.IsAbs(got) {
		t.Fatalf("相对路径没有转绝对: %q", got)
	}
	if got := ExpandPath("   "); got != "" {
		t.Fatalf("空白应当返回空串，得到 %q", got)
	}
}
