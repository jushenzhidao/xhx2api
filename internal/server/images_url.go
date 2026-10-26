package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 图片 URL 支持：上游只回 base64，而一张 4K 图的 base64 有 8MB+，
// 塞给前端、工作流或消息队列都很难用。开启 images.return_url 后，
// 网关把图落盘并提供 /v1/images/<id> 直接访问。

// ImageUploader 把图片传到外部存储并返回可访问 URL。
//
// 由 cmd/server 按配置注入（目前是 cosx.Client）；server 包只依赖这个接口，
// 不 import 任何对象存储实现——换 S3/MinIO 时只需要再写一个实现。
type ImageUploader interface {
	Put(ctx context.Context, name string, data []byte, contentType string) (string, error)
}

// imageContentType 按扩展名给出上传用的 Content-Type。
func imageContentType(ext string) string {
	switch ext {
	case ".jpg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

// imageExtByFormat 把上游的 output_format 映射成扩展名。
func imageExtByFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return ".jpg"
	case "webp":
		return ".webp"
	default:
		return ".png"
	}
}

// imageID 是内容寻址的图片 id：sha256 前 16 字节。
//
// 用内容哈希而不是自增/随机名有两个好处：同一张图重复生成不会产生新文件，
// 且文件名不可枚举（免鉴权的静态路由必须防遍历）。
func imageID(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:16])
}

// attachImageURLs 把响应里每张图落盘，并补上可访问的 url。
//
// onlyURL=true 时删掉 b64_json：客户端显式要 response_format=url，通常就是
// 为了省掉那几 MB 的 base64。
func (h *Handler) attachImageURLs(r *http.Request, body []byte, onlyURL bool) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, fmt.Errorf("响应不是 JSON: %w", err)
	}
	items, _ := payload["data"].([]any)
	if len(items) == 0 {
		return body, nil
	}
	ext := imageExtByFormat(fmt.Sprint(payload["output_format"]))
	base := h.imagePublicBase(r)
	if err := os.MkdirAll(h.cfg.ImageDir, 0o700); err != nil {
		return body, fmt.Errorf("建图片目录失败: %w", err)
	}
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		b64, _ := obj["b64_json"].(string)
		if b64 == "" {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			log.Printf("WARN: [images] base64 解码失败: %v", err)
			continue
		}
		name := imageID(blob) + ext
		// 优先对象存储：多副本/容器部署下本地盘不共享，外链也更好用。
		link := ""
		if h.cfg.ImageUploader != nil {
			u, uerr := h.cfg.ImageUploader.Put(r.Context(), name, blob, imageContentType(ext))
			if uerr != nil {
				log.Printf("WARN: [images] 上传对象存储失败，回落本地: %v", uerr)
			} else {
				link = u
			}
		}
		if link == "" && h.cfg.ImageDir != "" {
			path := filepath.Join(h.cfg.ImageDir, name)
			if _, statErr := os.Stat(path); statErr != nil {
				if werr := os.WriteFile(path, blob, 0o600); werr != nil {
					log.Printf("WARN: [images] 写图失败 %s: %v", path, werr)
					continue
				}
			}
			link = base + "/v1/images/" + name
		}
		if link == "" {
			// 既没上传成功也没有本地目录：不编造 url，保留 b64 让客户端自取。
			continue
		}
		obj["url"] = link
		if onlyURL {
			delete(obj, "b64_json")
		}
	}
	return json.Marshal(payload)
}

// imagePublicBase 决定写进 url 的对外基址。
//
// 优先用配置的 public_base：反向代理后面 r.Host 是内网名，直接把内网地址
// 写进 url，客户端拿到也访问不了。没配就按请求头推导，够本机直连用。
func (h *Handler) imagePublicBase(r *http.Request) string {
	if v := strings.TrimRight(strings.TrimSpace(h.cfg.ImagePublicBase), "/"); v != "" {
		return v
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); v != "" {
		scheme = v
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// imageFile 提供落盘图片的访问（GET /v1/images/{name}）。
//
// 不带鉴权：浏览器 <img src>、Markdown 预览、第三方下载器都发不了
// Authorization 头。安全性靠文件名——内容 sha256 的前 16 字节，不可枚举。
func (h *Handler) imageFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.cfg.ImageDir == "" || !imageNameOK(name) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(h.cfg.ImageDir, name)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	// 内容寻址 => 文件永远不会变，可以永久缓存。
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

// imageNameOK 校验文件名形如 <32 位小写 hex>.<png|jpg|webp>。
func imageNameOK(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return false
	}
	id, ext := name[:dot], name[dot:]
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	switch ext {
	case ".png", ".jpg", ".webp":
		return true
	}
	return false
}
