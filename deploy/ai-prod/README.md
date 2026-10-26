# xhx2api · ai-prod 运维说明

公网入口：**https://xhx2api.1task.cn** 　面板：https://xhx2api.1task.cn/panel/
本机回环：`127.0.0.1:39026` → 容器 `7867` 　源：github.com/rsfree/xhx2api

## 目录

```
/opt/xhx2api/
├── docker-compose.yml     # 镜像部署声明（端口只绑回环）
├── .env                   # XHX2API_IMAGE_TAG=sha-<提交>
├── config.json            # 生产配置（含 api_key，600，属主 10001）
├── auths/                 # 账号凭证（JWT/refresh，700，属主 10001）
├── data/                  # 池状态 + 用量 + 请求日志 + 模型目录缓存
│   └── images/            # 生成的图（默认返回 url 后每张都落盘，600/属主 10001）
└── nginx/                 # 站点配置与签发脚本（真源在仓库 deploy/）
```

## 日常操作

```bash
cd /opt/xhx2api

docker compose ps                    # 状态（应 Up (healthy)）
docker logs --tail 50 xhx2api        # 日志
curl -s http://127.0.0.1:39026/healthz   # 存活（恒 200）

docker compose restart xhx2api       # 重启（会先落盘池状态）
docker compose pull && docker compose up -d   # 拉新镜像并换
```

## 升级（CI 出镜像后）

镜像由仓库的 `docker` workflow 在 push main 时构建，tag 为
`main` / `latest` / `sha-<7位>`。**回滚靠 sha tag，不靠 latest**：

```bash
cd /opt/xhx2api
# 1) 看有哪些版本
T=$(curl -s "https://ghcr.io/token?scope=repository:rsfree/xhx2api:pull&service=ghcr.io" \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")
curl -s -H "Authorization: Bearer $T" https://ghcr.io/v2/rsfree/xhx2api/tags/list

# 2) 钉住某个提交（把 .env 里那行改掉）
sed -i 's/^XHX2API_IMAGE_TAG=.*/XHX2API_IMAGE_TAG=sha-02d21d1/' .env
docker compose pull && docker compose up -d
```

版本号会体现在面板顶部（镜像构建时注入 `VERSION`，非 tag 构建为 `dev-<sha>`）。

## 证书

```bash
bash /opt/xhx2api/nginx/apply-https.sh    # 幂等：已有证书则只做安装/续期配置
```

acme.sh 自动续期，`reloadcmd` 已配。到期前 30 天会自动续，无需人工干预。

## 备份

`auths` 与 `data` 是**有状态**的，升级/迁移前先备份。注意 `data/images` 可能有几个 GB
（默认上限 2GB），只想留状态就别把图打包进去：

```bash
tar czf /root/xhx2api-backup-$(date +%F).tar.gz -C /opt/xhx2api auths config.json \
  --exclude='data/images' data
```

## 图片目录（默认开）

`images.return_url` 默认 **true**：每张生成的图都会落盘到 `data/images/`，并提供
`https://xhx2api.1task.cn/v1/images/<sha256前16位>.<ext>`。要点：

- 文件名是**内容哈希**，同一张图不会重复落盘；
- 容量上限 `images.max_total_mb`（默认 2048，`0` = 不限）：超了按 mtime 删最旧的到 90%，
  **旧链接会 404**，别当永久存储；
- 想彻底不落盘：`images.return_url=false` + `images.response_format=b64_json`
  （改了要重启，重启后用 `docker exec xhx2api sh -c 'touch /app/data/images/.w'` 验可写）；
- 目录属主必须 10001，否则容器写不进去 → 响应会回落成 base64（`X-XHX-Image-URL: unavailable`）：

```bash
chown -R 10001:10001 /opt/xhx2api/data && chmod 755 /opt/xhx2api/data/images
```

## 注意

- **同一账号不要同时在两处跑**：本机和线上若挂同一批凭证，refresh token 轮换会互相顶掉，
  表现为随机掉登录。迁移时先把旧实例停掉。
- `auths/*.json` 等同明文密码，不要进 git、不要贴聊天记录。
- nginx 的 `proxy_read_timeout 3600s` 是给生图和慢对话留的：上游实测同一模型
  12s→137s 波动，默认 60s 会掐断，表现为「网关卡死」。
