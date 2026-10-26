package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// imageRequest 是转发一次图片任务的规范形状。
//
// 为什么要这一层：OpenAI 把 generations 与 edits 拆成两个端点，纯粹是传输
// 差异（JSON vs multipart），不是语义差异。上游更极端——/images/gen 收到
// `image` 会**静默忽略**（实测 image_tokens=0，出的是无关的新图），图生图
// 只有 /images/edits 认。所以网关必须在内部按「有没有输入图」分流，
// 而不能把客户端的 image 原样透传给 /images/gen 假装支持。
type imageRequest struct {
	prompt string
	// images 为空 = 纯文生图；非空 = 走 edits。
	images [][]byte
	mask   []byte
	// fields 是白名单参数（size/quality/n/...），原样带过去。
	fields  map[string]string
	wantURL bool
	title   string
	// model 是客户端填的模型名，只用于请求日志/面板归因。
	//
	// 上游 /images/gen 完全忽略这个字段（实测 gpt-image-2、gpt-image-2.5 和
	// 捏造名都 200，image_tokens 一模一样），所以它不参与路由；但用户排查
	// 「为什么我要 gpt-image-2 出来不像」时需要看到自己填了什么。
	model string
}

// 单图与总量的上限，与上游 gpt-image-2 的 20MB/张对齐。
const (
	maxImageBytes   = 20 << 20
	maxImagesPerReq = 16
)

// 允许透传给上游的参数（其余一律丢弃：上游对未声明字段严格校验）。
var imageFieldWhitelist = []string{"size", "quality", "n", "background", "output_format"}

// imageRateRetryDelay 是生图遇到 429 后原地重试前的等待。
//
// 实测 429 的正文是 `{"code":200103,"message":"limiter_request_error",
// "details":"request exceed max time, retry after:0s"}`，1 秒后重试必成功
// （12 并发 → 首发 10 成功 + 2 个 429 → 重试 12/12）。这里留一倍余量给
// 上游的滑动窗口，同时相对 60 秒的生成时间可以忽略。
const imageRateRetryDelay = 2 * time.Second

// imageGenerations 处理 POST /v1/images/generations。
//
// 这个端点同时承担文生图与图生图：
//   - 没有 `image` 字段 → 走上游 /images/gen（纯文生图）；
//   - 带 `image`（字符串或数组）→ 网关自己转成 multipart 打 /images/edits。
//
// `image` 支持 data URI 与裸 base64；`null` / `[]` / 空串一律视为「没有图」，
// 和 OpenAI SDK 的两种空值写法对齐。
func (h *Handler) imageGenerations(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tr := traceFrom(r.Context())
	body, err := readBody(r, h.cfg.MaxBodyBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	req := imageRequest{prompt: strings.TrimSpace(upstream.ImagePromptOf(body)), fields: map[string]string{}}

	refs, err := extractImageRefs(payload["image"])
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// mask 只在有图时有意义（无图还给 mask 是调用方搞错了）。
	if maskRef, _ := payload["mask"].(string); strings.TrimSpace(maskRef) != "" {
		if len(refs) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				"mask 需要配合 image 使用：纯文生图没有可遮罩的区域")
			return
		}
		if req.mask, err = decodeImageRef(maskRef); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "mask 解码失败: "+err.Error())
			return
		}
	}
	if req.prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt 必填（上游对空 prompt 直接 400）")
		return
	}
	for _, ref := range refs {
		blob, derr := decodeImageRef(ref)
		if derr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image 解码失败: "+derr.Error())
			return
		}
		if len(blob) > maxImageBytes {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("单张图超过 %d MB", maxImageBytes>>20))
			return
		}
		req.images = append(req.images, blob)
	}
	if len(req.images) > maxImagesPerReq {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("一次最多 %d 张输入图", maxImagesPerReq))
		return
	}

	req.wantURL, err = h.checkResponseFormat(payload["response_format"])
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req.title, _ = payload["title"].(string)
	req.model, _ = payload["model"].(string)
	req.model = strings.TrimSpace(req.model)
	for _, key := range imageFieldWhitelist {
		if v, ok := payload[key]; ok {
			if s := scalarString(v); s != "" {
				req.fields[key] = s
			}
		}
	}
	h.dispatchImage(w, r, started, tr, req)
}

// imageEdits 处理 POST /v1/images/edits（multipart 门面）。
//
// 它与 generations 汇入同一个 dispatchImage：这里只负责把 multipart 拆成
// 规范形状。多图两种写法都收——重复的 `image` 字段（HTML 表单/curl 常用）
// 和 `image[]`（SDK 常用），上游两种都认（实测各 200）。
func (h *Handler) imageEdits(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tr := traceFrom(r.Context())

	const maxEditBytes = 64 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxEditBytes)
	if err := r.ParseMultipartForm(maxEditBytes); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"需要 multipart/form-data（image=图片文件、prompt=文本）："+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	req := imageRequest{prompt: strings.TrimSpace(r.FormValue("prompt")), fields: map[string]string{}}
	if req.prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt 必填")
		return
	}
	var err error
	req.wantURL, err = h.checkResponseFormat(r.FormValue("response_format"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 两种字段名都收，保持与客户端习惯一致。
	for _, key := range []string{"image", "image[]"} {
		for _, fh := range r.MultipartForm.File[key] {
			// 空 part 表示「没附文件」（浏览器未选文件、或读取失败），
			// 与 JSON 侧的 null 同义，不能因此报错。
			if fh.Size == 0 {
				continue
			}
			if fh.Size > maxImageBytes {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
					fmt.Sprintf("%s 超过 %d MB", fh.Filename, maxImageBytes>>20))
				return
			}
			f, oerr := fh.Open()
			if oerr != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "读取上传图片失败: "+oerr.Error())
				return
			}
			blob, rerr := io.ReadAll(io.LimitReader(f, maxImageBytes))
			_ = f.Close()
			if rerr != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "读取上传图片失败: "+rerr.Error())
				return
			}
			req.images = append(req.images, blob)
		}
	}
	if len(req.images) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"image 必填（multipart 文件字段，字段名 image 或 image[]）")
		return
	}
	if len(req.images) > maxImagesPerReq {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("一次最多 %d 张输入图", maxImagesPerReq))
		return
	}
	if mc, maskHeader, merr := r.FormFile("mask"); merr == nil {
		defer mc.Close()
		if maskHeader.Size > 0 {
			if req.mask, err = io.ReadAll(io.LimitReader(mc, maxImageBytes)); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "读取 mask 失败: "+err.Error())
				return
			}
		}
	}
	req.title = strings.TrimSpace(r.FormValue("title"))
	req.model = strings.TrimSpace(r.FormValue("model"))
	for _, key := range imageFieldWhitelist {
		if v := strings.TrimSpace(r.FormValue(key)); v != "" {
			req.fields[key] = v
		}
	}
	h.dispatchImage(w, r, started, tr, req)
}

// dispatchImage 按「有没有输入图」选上游端点。
func (h *Handler) dispatchImage(w http.ResponseWriter, r *http.Request, started time.Time,
	tr *requestTrace, req imageRequest) {
	if tr != nil {
		tr.model = req.model
		if tr.model == "" {
			tr.model = upstream.ImageModelID
		}
		tr.stream = false
	}
	// 上游有两条尺寸约束（都会让用户白等或拿到小图）：
	//   - 最长边 ≤ 3840：超了先跑 106 秒再回 400；
	//   - 总像素 ≤ 3840×2160：超了不报错，静默降级成更小的图（实测 3840x3840 → 2880x2880）。
	// 所以网关在转发前统一等比压缩，并在响应头标注实际提交的尺寸。
	if raw := req.fields["size"]; raw != "" {
		if fixed, changed := upstream.ClampImageSize(raw); changed {
			req.fields["size"] = fixed
			log.Printf("INFO: [images] size %q 超上游约束（最长边 ≤ %d、像素 ≤ %d），已等比压到 %q",
				raw, upstream.MaxImageEdge, upstream.MaxImagePixels, fixed)
			w.Header().Set("X-XHX-Image-Size", fixed)
		}
	}

	if len(req.images) == 0 {
		// 文生图：保持 JSON 直通 /images/gen。
		payload := map[string]any{"prompt": req.prompt}
		for k, v := range req.fields {
			payload[k] = v
		}
		out, err := json.Marshal(payload)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "rewrite body: "+err.Error())
			return
		}
		h.serveImage(w, r, started, tr, req.wantURL, func(ctx context.Context, a *auth.Auth) (*upstream.ImageReply, error) {
			return h.cfg.Upstream.GenerateImage(ctx, a, out, req.title)
		})
		return
	}

	// 图生图：上游只认 multipart，网关在这里转。
	body, contentType, err := buildEditMultipart(req)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "构造上游请求失败: "+err.Error())
		return
	}
	h.serveImage(w, r, started, tr, req.wantURL, func(ctx context.Context, a *auth.Auth) (*upstream.ImageReply, error) {
		return h.cfg.Upstream.EditImage(ctx, a, body, contentType, req.title)
	})
}

// buildEditMultipart 把规范请求拼成上游 /images/edits 要的 multipart。
//
// 文件名按魔数给扩展名：上游会看扩展名判断图片类型，全叫 .png 会让 jpeg
// 输入走错解码分支。
func buildEditMultipart(req imageRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("prompt", req.prompt); err != nil {
		return nil, "", err
	}
	for _, key := range imageFieldWhitelist {
		if v := req.fields[key]; v != "" {
			if err := mw.WriteField(key, v); err != nil {
				return nil, "", err
			}
		}
	}
	for i, blob := range req.images {
		fw, err := mw.CreateFormFile("image", fmt.Sprintf("image-%d%s", i, sniffImageExt(blob)))
		if err != nil {
			return nil, "", err
		}
		if _, err := fw.Write(blob); err != nil {
			return nil, "", err
		}
	}
	if len(req.mask) > 0 {
		fw, err := mw.CreateFormFile("mask", "mask"+sniffImageExt(req.mask))
		if err != nil {
			return nil, "", err
		}
		if _, err := fw.Write(req.mask); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// checkResponseFormat 统一处理 response_format：url 需要配置开启。
func (h *Handler) checkResponseFormat(v any) (bool, error) {
	format := strings.TrimSpace(scalarString(v))
	if !strings.EqualFold(format, "url") {
		return false, nil
	}
	if !h.cfg.ImageReturnURL {
		return false, fmt.Errorf("上游只返回 base64，没有可访问的图片 URL；要 url 请先开启 images.return_url（网关会落盘或传对象存储并提供链接）")
	}
	return true, nil
}

// extractImageRefs 接受 null / 字符串 / 字符串数组，空值表示没有图。
func extractImageRefs(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("image 数组里只能是字符串（data URI 或 base64）")
			}
			if strings.TrimSpace(s) == "" {
				continue
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("image 必须是字符串或字符串数组")
}

// decodeImageRef 把 data URI 或裸 base64 解成字节。
//
// 不支持 http(s) URL：那需要网关替调用方去抓取，会引入 SSRF 面（内网地址、
// 元数据服务）。要这种形态就让客户端自己下载后转 base64。
func decodeImageRef(ref string) ([]byte, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("空图片")
	}
	if strings.HasPrefix(ref, "data:") {
		comma := strings.Index(ref, ",")
		if comma < 0 {
			return nil, fmt.Errorf("data URI 缺少逗号")
		}
		if !strings.Contains(ref[:comma], ";base64") {
			return nil, fmt.Errorf("只支持 base64 编码的 data URI")
		}
		ref = ref[comma+1:]
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return nil, fmt.Errorf("暂不支持 URL 形态：请由客户端下载后转成 base64 或 data URI")
	}
	blob, err := base64.StdEncoding.DecodeString(ref)
	if err != nil {
		// 兼容 URL-safe 与缺省 padding 的写法。
		if alt, altErr := base64.RawURLEncoding.DecodeString(strings.TrimRight(ref, "=")); altErr == nil {
			return alt, nil
		}
		return nil, fmt.Errorf("不是合法 base64: %w", err)
	}
	return blob, nil
}

// sniffImageExt 按魔数给扩展名。
func sniffImageExt(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return ".png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return ".jpg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return ".gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp"
	}
	return ".png"
}

// scalarString 把 JSON 标量渲染成字符串（n 可能是数字或字符串）。
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// imageSender 发一次图片请求；由调用方决定打哪个上游端点。
type imageSender func(ctx context.Context, a *auth.Auth) (*upstream.ImageReply, error)

// serveImage 是生图与改图共用的执行段：选号 → 调上游 → 计量 → 回写。
//
// 抽出来是为了让两条路由共享同一套换号与错误分类：图生图的失败模式和生图
// 完全一致（超时不换号、账号级错误才换号），复制一份迟早会分叉。
func (h *Handler) serveImage(w http.ResponseWriter, r *http.Request, started time.Time,
	tr *requestTrace, wantURL bool, send imageSender) {
	var lastErr error
	var lastKind pool.ErrKind
	excluded := map[string]bool{}
	for attempt := 0; attempt < h.cfg.MaxRotate; attempt++ {
		if tr != nil {
			tr.attempts = attempt + 1
		}
		lease, perr := h.pickForRequest(r.Context(), "", excluded)
		if perr != nil {
			if lastErr == nil {
				lastErr = perr
			}
			break
		}
		a := lease.Auth()
		if a == nil {
			lease.ReleaseSuccess(0)
			lastErr = pool.ErrNoAccount
			break
		}
		if tr != nil {
			tr.uid = a.UID
			tr.account = a.Snapshot().Nickname
		}
		if _, rerr := h.cfg.Upstream.RefreshTokenIfNeeded(r.Context(), a, h.cfg.RefreshSkew); rerr != nil {
			log.Printf("WARN: [refresh] %s: %v", a.UID, rerr)
		}

		timeout := h.cfg.ImageTimeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		reply, gerr := send(ctx, a)
		elapsed := time.Since(started)
		if gerr != nil {
			cancel()
			kind := classifyErr(gerr, 0, gerr.Error())
			lastKind, lastErr = kind, gerr
			if tr != nil {
				tr.errKind, tr.errMsg = kind, gerr.Error()
			}
			log.Printf("WARN: [images] uid=%s attempt=%d kind=%s: %v", a.UID, attempt+1, kind, gerr)

			// 429 是瞬时过载，不是账号故障：实测 12 并发生图首发 10 成功、
			// 2 个 429（立即返回、retry after: 0s），等 1 秒重试**全部成功**。
			// 换号帮不上忙（瓶颈是单账号并发配额），罚号更没道理（会把好号
			// 打进软冷却，连带影响同一账号的对话请求）。所以原地等一小会再来。
			if kind == upstream.ErrSoftRate.String() && attempt+1 < h.cfg.MaxRotate {
				lease.ReleaseSuccess(elapsed) // 不计失败、不触发冷却
				if tr != nil {
					tr.rotations++
				}
				log.Printf("INFO: [images] uid=%s 429 瞬时过载，%s 后原地重试", a.UID, imageRateRetryDelay)
				select {
				case <-time.After(imageRateRetryDelay):
				case <-r.Context().Done():
					writeOpenAIError(w, http.StatusRequestTimeout, "client_closed", "客户端已断开")
					return
				}
				continue
			}

			lease.Release(&kind, gerr.Error(), elapsed)
			// 超时不换号：等满一个窗口已经 100s 了，再换一个号继续等没有意义。
			if kind != "client" && kind != "session_dead" && kind != "server" && kind != "gateway" {
				break
			}
			excluded[a.UID] = true
			if tr != nil {
				tr.rotations++
			}
			continue
		}
		cancel()
		lease.ReleaseSuccess(elapsed)
		promptTokens, completionTokens, total := tokenCounts(reply.Usage())
		if tr != nil {
			tr.prompt, tr.completion, tr.total = promptTokens, completionTokens, total
		}
		h.recordUsage(tr, upstream.ImageModelID, false, promptTokens, completionTokens, total, elapsed)
		body := reply.Body
		if h.cfg.ImageReturnURL && (h.cfg.ImageDir != "" || h.cfg.ImageUploader != nil) {
			if out, uerr := h.attachImageURLs(r, body, wantURL); uerr == nil {
				body = out
			} else {
				// 落盘失败不该让整次生成白费：图还是给客户端，只是没有 url。
				log.Printf("WARN: [images] 生成 url 失败: %v", uerr)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	status, code, msg := errorResponse(lastErr, lastKind, h.cfg.Pool.Counts())
	writeOpenAIError(w, status, code, msg)
}
