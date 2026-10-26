# xhx2api 反代（回环；鉴权由**服务侧 api_key** 承担，nginx 层不做认证）
proxy_pass http://127.0.0.1:39026;
proxy_http_version 1.1;
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
# ⚠️ 不设 Upgrade/$connection_upgrade：SSE 是普通 HTTP 长连接，**不需要** WebSocket 升级；
#    而 $connection_upgrade 需在 http 块 map 定义，宝塔主配置未必有 ⇒ 会让 nginx -t 失败。

# 🔴 SSE 流式：/v1/chat/completions 的 stream=true 是长连接事件流。
#    一旦开缓冲，nginx 会攒够才吐 ⇒ 客户端表现为「一直等、最后一次性吐完」。
proxy_buffering off;
proxy_request_buffering off;
proxy_cache off;

# 🔴 超时按最慢的链路设，不是按对话设：
#    - 非流式对话实测 12-137s（同一模型波动极大）
#    - 生图 4K 档 136-148s，quality=high 13342 image_tokens
#    - 图生图上传 + 生成可到 200s+
#    默认 60s 会把上面全部掐断，表现为「网关卡死」。
proxy_connect_timeout 10s;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;

# 图生图走 base64 内联图片，4K PNG base64 约 2-3MB；给 64m 覆盖多图输入。
client_max_body_size 64m;
client_body_timeout 300s;
