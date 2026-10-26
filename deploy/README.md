# 线上部署（1task.cn）

目标形态：`https://xhx2api.1task.cn` → nginx（TLS）→ `127.0.0.1:7867` 容器。

## 1. 配置

```bash
cp config.example.json deploy/config.json
```

改三处：

```jsonc
{
  "listen": "127.0.0.1:7867",              // 只让本机 nginx 访问
  "api_key": "sk-xhx-<自己生成>",           // 公网必须设！空 = 谁都能用你的账号
  "login": {
    "callback_base": "https://xhx2api.1task.cn"   // 浏览器授权的回跳地址
  }
}
```

`callback_base` 是**线上和本机唯一需要区别对待**的配置：

| 场景 | 取值 | 说明 |
|---|---|---|
| 本机自用 | 留空 | 自动推导 `http://127.0.0.1:<端口>` |
| 线上域名 | `https://xhx2api.1task.cn` | 授权完成后浏览器回跳到这个域名，网关收 code 换票 |

上游 authorize 页接收域名 redirect（2026-10-26 实测：页面原样透传，不回退
`office-raccoon://`）。**但即使它某天开始拒绝**，也不影响可用性：
用户把跳转后地址栏里的整条链接粘到面板「粘贴凭证」即可（`code` 就在里面）。

## 2. 起容器

```bash
mkdir -p auths data
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml logs -f
```

看到 `已启动：监听 127.0.0.1:7867` 即可。

> 挂载目录属主不对会报 `permission denied`（写 `auths/*.json.tmp` 失败）：
> `PUID=$(id -u) PGID=$(id -g) docker compose -f deploy/docker-compose.yml up -d`
> 或 `sudo chown -R 10001:10001 auths data deploy/config.json`。

## 3. nginx + 证书

```bash
cp deploy/nginx.xhx2api.1task.cn.conf /www/server/panel/vhost/nginx/
# 证书（DNS-01，不占用 80 端口）
acme.sh --issue --dns dns_ali -d xhx2api.1task.cn
nginx -t && nginx -s reload
```

`proxy_buffering off` 和 `proxy_read_timeout 3600s` 是**必须**的：
思考型模型单次生成 1-3 分钟，缓冲/短超时会让客户端看起来卡死。

## 4. 验收

```bash
# 只读项（不耗积分）
go run ./cmd/livecheck -base https://xhx2api.1task.cn -key <api_key> -skip-chat
# 全量（会消耗几次对话的积分）
go run ./cmd/livecheck -base https://xhx2api.1task.cn -key <api_key>
```

全绿就可以把客户端指过去了：

```bash
export ANTHROPIC_BASE_URL=https://xhx2api.1task.cn
export ANTHROPIC_AUTH_TOKEN=<api_key>
```

## 5. 安全底线

- **`api_key` 必须设**：为空等于把账号借给全网。
- 面板与 `/v1/*` 同一把钥匙；面板支持 `?key=` 直达，分享链接时注意别外传。
- 建议在 nginx 层再加一道 IP 白名单或 `limit_req`（本仓未内置限流）。
- `auths/` 里是明文 refresh token：不要进备份/同步盘，不要提交到仓库。
