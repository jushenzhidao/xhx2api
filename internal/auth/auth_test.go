package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeJWT 造一个只带 payload 的假 JWT（不需要合法签名）。
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return "eyJhbGciOiJub25lIn0." + enc + ".sig"
}

func TestParseVariants(t *testing.T) {
	token := makeJWT(t, map[string]any{"user_id": 4465567, "exp": 1792369369})

	cases := []struct {
		name    string
		raw     string
		wantUID string
	}{
		{"标准 JSON", `{"access_token":"` + token + `","refresh_token":"r1","uid":"4465567","nickname":"小浣熊"}`, "4465567"},
		{"驼峰别名", `{"accessToken":"` + token + `","refreshToken":"r1","userId":"u-2"}`, "u-2"},
		{"裸 token", token, "4465567"},
		{"嵌套 data", `{"data":{"access_token":"` + token + `","refresh_token":"r1","uid":"u-3"}}`, "u-3"},
		{"Bearer 前缀", `{"access_token":"Bearer ` + token + `","uid":"u-4"}`, "u-4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if a.UID != tc.wantUID {
				t.Fatalf("uid = %q, want %q", a.UID, tc.wantUID)
			}
			if strings.HasPrefix(a.AccessToken, "Bearer ") {
				t.Fatal("access token 仍带 Bearer 前缀")
			}
			if a.ExpiresAt == 0 {
				t.Fatal("expires_at 未从 JWT 解出")
			}
		})
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "不是 JSON", `{"foo":"bar"}`, `{"access_token":"短"}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("应当拒绝 %q", raw)
		}
	}
}

func TestSaveAtomicAndLoadDir(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{Data: Data{UID: "1001", Nickname: "测试", AccessToken: "tok", RefreshToken: "ref",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}
	if err := a.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 权限必须是 0600（凭证文件不能被同机其他用户读到）。
	info, err := os.Stat(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("文件权限 = %o, want 600", mode)
	}
	// 重新加载。
	auths, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(auths) != 1 || auths[0].UID != "1001" {
		t.Fatalf("加载结果不对: %+v", auths)
	}
	// 覆盖写：积分更新后落盘，重新加载应看到新值。
	a.SetCredit(1234)
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	auths, _ = LoadDir(dir)
	if auths[0].Credit != 1234 {
		t.Fatalf("credit = %d, want 1234", auths[0].Credit)
	}
	// 临时文件不应残留。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Fatalf("残留临时文件 %s", e.Name())
		}
	}
}

// TestSaveLoadPreservesState 守卫「程序写回的状态字段」：漏解析任何一个，
// 表现都是「重启后看起来正常，但某个状态悄悄丢了」（实测踩过 checked_in）。
func TestSaveLoadPreservesState(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	a := &Auth{Data: Data{
		UID: "u-1", Nickname: "号一", AccessToken: "t", RefreshToken: "r",
		Credit: 1234, CreditAt: now.Format(time.RFC3339),
		CheckedIn:  now.In(shanghai()).Format("2006-01-02"),
		ImportedAt: now.Add(-48 * time.Hour).Format(time.RFC3339),
		RegisterAt: now.Add(-720 * time.Hour).Format(time.RFC3339),
	}}
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("加载到 %d 个账号", len(loaded))
	}
	got := loaded[0].Snapshot()
	if got.Credit != 1234 {
		t.Fatalf("credit 丢了: %d", got.Credit)
	}
	if got.CreditAt == "" {
		t.Fatal("credit_at 丢了")
	}
	if got.CheckedIn != a.CheckedIn {
		t.Fatalf("checked_in 丢了: %q, want %q", got.CheckedIn, a.CheckedIn)
	}
	if got.ImportedAt == "" {
		t.Fatal("imported_at 丢了（新号静默期会失效）")
	}
	if got.RegisterAt == "" {
		t.Fatal("register_at 丢了")
	}
	// CheckedInToday 也要能直接判断。
	if !loaded[0].CheckedInToday() {
		t.Fatal("今天签到过，CheckedInToday 应为 true")
	}
}

func TestSaveStampsImportedAtOnFirstWrite(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{Data: Data{UID: "u-2", AccessToken: "t"}}
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, _ := LoadDir(dir)
	if len(loaded) != 1 {
		t.Fatal("账号没落盘")
	}
	if loaded[0].Snapshot().ImportedAt == "" {
		t.Fatal("首次 Save 应当补 imported_at（新号静默期依赖它）")
	}
	// 已有文件再次 Save：不能把进池时间刷成现在。
	first := loaded[0].Snapshot().ImportedAt
	time.Sleep(10 * time.Millisecond)
	if err := loaded[0].Save(dir); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadDir(dir)
	if again[0].Snapshot().ImportedAt != first {
		t.Fatalf("第二次 Save 改写了 imported_at: %q → %q", first, again[0].Snapshot().ImportedAt)
	}
}

func TestFileNameForSanitizes(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "./_etc_passwd.json",
		"用户:1":             "____1.json",
		"":                 "acct-",
	}
	for uid, wantPrefix := range cases {
		got := FileNameFor(uid)
		if !strings.HasSuffix(got, ".json") {
			t.Fatalf("%q → %q 没有 .json 后缀", uid, got)
		}
		if strings.Contains(got, "/") || strings.Contains(got, "..") {
			t.Fatalf("%q → %q 仍含路径字符", uid, got)
		}
		_ = wantPrefix
	}
	// 空 uid 用哈希兜底，不能所有空 uid 都撞同一个文件名。
	if FileNameFor("") == FileNameFor(" ") {
		t.Fatal("空 uid 兜底名冲突")
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{Data: Data{ExpiresAt: time.Now().Add(5 * time.Minute).UnixMilli()}}
	if !a.NeedsRefresh(10 * time.Minute) {
		t.Fatal("5 分钟后到期，10 分钟窗口应当需要刷新")
	}
	if a.NeedsRefresh(1 * time.Minute) {
		t.Fatal("1 分钟窗口不应触发")
	}
	// 没有到期时间：不主动刷（等 401）。
	unknown := &Auth{}
	if unknown.NeedsRefresh(time.Hour) {
		t.Fatal("未知到期时间不应主动刷新")
	}
}

func TestSetTokensKeepsRefreshWhenEmpty(t *testing.T) {
	a := &Auth{Data: Data{AccessToken: "old", RefreshToken: "ref"}}
	a.SetTokens("new", "", 0)
	if a.JWT() != "new" {
		t.Fatalf("access = %q", a.JWT())
	}
	if a.RefreshTokenValue() != "ref" {
		t.Fatalf("refresh 被空串覆盖了: %q", a.RefreshTokenValue())
	}
}

func TestLoadDirSkipsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	good := &Auth{Data: Data{UID: "ok", AccessToken: "t"}}
	if err := good.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	auths, err := LoadDir(dir)
	if err == nil {
		t.Fatal("应当回报坏文件错误")
	}
	if len(auths) != 1 {
		t.Fatalf("好账号应当仍被加载，got %d", len(auths))
	}
}
