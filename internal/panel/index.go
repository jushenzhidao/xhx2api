// index.go 内嵌前端资源。
//
// go:embed 让二进制自带面板，部署时不需要额外挂载静态目录——这是「单二进制」
// 承诺的一部分（也是为什么前端坚持不用构建工具）。
package panel

import (
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

//go:embed index.html app.js style.css
var panelFS embed.FS

// decodeJSON 解析请求体 JSON（限制 2MiB）。
func decodeJSON(r *http.Request, v any) error {
	body, err := readAll(r, 2<<20)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// readAll 读请求体（带上限）。
func readAll(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errTooLarge
	}
	return raw, nil
}

// errTooLarge 是请求体超限错误。
var errTooLarge = &httpError{Status: http.StatusRequestEntityTooLarge, Msg: "请求体过大"}

// httpError 是带状态码的错误（面板统一映射）。
type httpError struct {
	Status int
	Msg    string
}

func (e *httpError) Error() string { return e.Msg }

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func writeFile(path string, raw []byte) error {
	return os.WriteFile(path, raw, 0o600)
}

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

func baseName(path string) string { return filepath.Base(path) }
