// loadtest 压上游：在给定并发下打 N 个对话请求，统计成功率、延迟与 tokens。
//
// 目的不是"跑满"，而是**找拐点**：从哪个并发开始出现 429 / 排队，
// 据此给 pool.max_in_flight 与 cooldown.soft_rate 定默认值。
//
//	go run ./cmd/loadtest -uid 7774611 -c 4 -n 8 -model sn-sensenova-6-8-flash
//
// 默认用限免模型（x0），不烧积分。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "账号目录")
	uid := flag.String("uid", "", "指定账号 uid（空 = 用目录里第一个）")
	model := flag.String("model", "sn-sensenova-6-8-flash", "模型")
	concurrency := flag.Int("c", 2, "并发")
	total := flag.Int("n", 4, "总请求数")
	maxTokens := flag.Int("max-tokens", 128, "max_tokens")
	prompt := flag.String("prompt", "只回复两个字：收到", "提示词")
	timeout := flag.Duration("timeout", 180*time.Second, "单请求超时")
	flag.Parse()

	accounts, err := auth.LoadDir(*authDir)
	if err != nil || len(accounts) == 0 {
		fatal("加载账号失败: %v", err)
	}
	var account *auth.Auth
	for _, a := range accounts {
		if *uid == "" || a.UID == *uid {
			account = a
			break
		}
	}
	if account == nil {
		fatal("账号 %s 不存在（目录: %s）", *uid, *authDir)
	}

	client := upstream.New()
	client.SetPersistHook(nil)

	fmt.Printf("账号 %s（%s）  模型 %s  并发 %d  总数 %d  max_tokens=%d\n\n",
		account.UID, nickname(account), *model, *concurrency, *total, *maxTokens)

	type result struct {
		OK        bool
		Status    int
		Err       string
		Latency   time.Duration
		TTFT      time.Duration
		InTokens  int64
		OutTokens int64
	}
	results := make([]result, *total)
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < *total; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()
			body, _ := json.Marshal(map[string]any{
				"model":      *model,
				"max_tokens": *maxTokens,
				"stream":     true,
				"messages":   []any{map[string]any{"role": "user", "content": *prompt}},
			})
			t0 := time.Now()
			stream, status, rawErr, err := client.ChatStream(ctx, account, body, "loadtest")
			if err != nil {
				results[idx] = result{Err: err.Error(), Status: status, Latency: time.Since(t0)}
				return
			}
			obj, aerr := upstream.Aggregate(stream, *model)
			_ = stream.Close()
			lat := time.Since(t0)
			if aerr != nil {
				msg := aerr.Error()
				if len(rawErr) > 0 {
					msg += " | " + truncate(string(rawErr), 120)
				}
				results[idx] = result{Err: msg, Status: status, Latency: lat}
				return
			}
			in, out := tokensOf(obj)
			results[idx] = result{OK: true, Status: status, Latency: lat, InTokens: in, OutTokens: out}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	okCount := 0
	var lats []time.Duration
	var inSum, outSum int64
	errKinds := map[string]int{}
	for _, r := range results {
		if r.OK {
			okCount++
			lats = append(lats, r.Latency)
			inSum += r.InTokens
			outSum += r.OutTokens
			continue
		}
		key := r.Err
		if key == "" {
			key = fmt.Sprintf("HTTP %d", r.Status)
		}
		errKinds[shortErr(key)]++
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })

	fmt.Printf("总耗时 %.1fs   成功 %d/%d (%.0f%%)\n", elapsed.Seconds(), okCount, *total, float64(okCount)/float64(*total)*100)
	if len(lats) > 0 {
		fmt.Printf("延迟 p50=%.1fs p90=%.1fs max=%.1fs\n",
			pct(lats, 0.5).Seconds(), pct(lats, 0.9).Seconds(), lats[len(lats)-1].Seconds())
	}
	if errKinds != nil && len(errKinds) > 0 {
		fmt.Println("失败分布:")
		for k, v := range errKinds {
			fmt.Printf("  %-60s x%d\n", k, v)
		}
	}
	rpm := float64(okCount) / elapsed.Minutes()
	tpm := float64(outSum) / elapsed.Minutes()
	fmt.Printf("\n吞吐：%.1f RPM ｜ 输出 %.2f 万 tokens/分钟 ｜ 本次 in=%d out=%d\n",
		rpm, tpm/10000, inSum, outSum)
	if okCount > 0 {
		fmt.Printf("单请求均值：%.1fs，输出 %.0f tokens\n", elapsed.Seconds()/float64(okCount), float64(outSum)/float64(okCount))
	}
}

func tokensOf(obj map[string]any) (in, out int64) {
	usage, _ := obj["usage"].(map[string]any)
	if usage == nil {
		return 0, 0
	}
	return num(usage["prompt_tokens"]), num(usage["completion_tokens"])
}

func num(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

// shortErr 把长错误裁成可聚合的短标签（去掉 request-id 之类的随机后缀）。
func shortErr(s string) string {
	s = strings.SplitN(s, " | ", 2)[0]
	if i := strings.Index(s, "："); i > 0 && i < 60 {
		return s[:i]
	}
	if len(s) > 90 {
		return s[:90]
	}
	return s
}

func nickname(a *auth.Auth) string {
	if n := a.Snapshot().Nickname; n != "" {
		return n
	}
	return filepath.Base(a.FilePath)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

var _ = io.Discard
