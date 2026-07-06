# Code Switch

> 用网页管理 Claude Code / Codex / Gemini CLI 的供应商、代理、MCP 和提示词。

## 概览

Code Switch 是适合 Linux 服务器运行的 Web 管理界面 + 本地代理服务。浏览器负责配置，后台进程负责转发，Claude Code / Codex / Gemini CLI 继续通过本机代理访问实际 API 供应商。

它主要解决：

- 多个 AI API 供应商统一配置和切换。
- 供应商失败时自动降级到备用供应商。
- 统计请求量、Token、成本和请求日志。
- 集中管理 MCP、CLI 配置、自定义提示词。

## 运行方式

默认会启动两个监听地址：

| 组件 | 默认地址 | 作用 |
|------|----------|------|
| Web 管理界面 | `0.0.0.0:8080` | 浏览器访问的管理后台 |
| Provider Relay | `127.0.0.1:18100` | CLI / API 客户端实际访问的本机代理 |

生产环境推荐：

- Web 管理界面只监听 `127.0.0.1:8080`。
- Provider Relay 保持 `127.0.0.1:18100`。
- Caddy / Nginx 对外提供 HTTPS，并反代到本机端口。
- 不直接把 `8080` 裸露到公网。

数据默认写入：

```text
~/.code-switch/
```

常见文件：

- `app.db`、`app.db-wal`、`app.db-shm`：SQLite 数据库和 WAL 文件。
- `users.json`：Web 管理后台用户和密码哈希。
- `claude-code.json`、`codex.json`、`openai-responses.json`、`openai-chat.json`：供应商配置。
- `codex-relay-keys.json`：Provider Relay 的访问 key。
- `provider-pools.json`：供应商池和降级配置。
- `mcp.json`、`prompts.json`：MCP 和提示词。
- `proxy-state/`：各 CLI 的代理开关状态。
- `favicons/`、`icons/`：界面图标缓存。

这个目录包含 API key、relay key、用户凭据哈希等敏感数据，应按私密数据处理。

## 生产约定

生产机通过本机 SSH 配置里的别名连接，例如：

```bash
ssh <生产机别名>
```

本文档用变量表示生产机和域名，不假设固定机器名：

```bash
PROD_SSH="<生产机 SSH 别名>"
ADMIN_DOMAIN="code.wcisman.cc"
API_DOMAIN="codeapi.wcisman.cc"
LOCAL_REPO="$(pwd)"
REMOTE_DIR="~/apps/code-switch"
DATA_DIR="~/.code-switch"
SERVICE_NAME="codeswitch.service"
```

远程本机端口：

- Web 管理界面：`127.0.0.1:8080`
- Provider Relay：`127.0.0.1:18100`

生产权限原则：

- 普通发布只上传构建产物到生产机的 `~/apps/code-switch`。
- 需要 `sudo` 的操作由服务器操作员手动执行。
- 自动化助手或发布脚本不得尝试 `sudo`、`sudo -n`、`systemctl restart`、`systemctl stop`、`systemctl start`。如果发布需要重启，只能在产物替换完成后停止操作，并把下方“操作员重启”命令发给有 sudo 权限的人执行。
- 不用 `kill` / `pkill` 绕过 systemd 管理生产服务。
- 修改 `/etc`、Caddy/Nginx、nftables、防火墙、systemd、apt 安装软件都属于系统级操作。

## 构建

已验证环境：

- Ubuntu 24.04.3 LTS
- Node.js `v24.14.0`
- npm `11.9.0`
- Go `go1.26.1`
- conda 环境 `code-switch-go-build-cgo`

准备 Go 构建环境：

```bash
conda activate code-switch-go-build-cgo
```

如果没有这个环境：

```bash
conda create -y -n code-switch-go-build-cgo -c conda-forge \
  'go=1.26.1' \
  'gcc_linux-64' \
  'gxx_linux-64'
conda activate code-switch-go-build-cgo
```

构建前端：

```bash
cd frontend
npm install
npm run build
cd ..
```

构建后端：

```bash
go test ./...
go build -o codeswitch-web .
```

本地临时运行：

```bash
CODE_SWITCH_WEB_ADDR=127.0.0.1:8080 ./codeswitch-web
```

健康检查：

```bash
curl -fsS http://127.0.0.1:8080/healthz
```

预期：

```json
{"ok":true}
```

## 首次部署

首次部署分两部分：

1. 本地构建并上传产物。
2. 服务器操作员配置 systemd、Caddy 和防火墙。

### 上传产物

```bash
REPO_DIR="$(pwd)"
ARTIFACT_DIR="$(pwd)/.deploy"
mkdir -p "$ARTIFACT_DIR"

cd "$REPO_DIR/frontend"
npm install
npm run build

cd "$REPO_DIR"
go test ./...
go build -o codeswitch-web .

STAMP="$(date +%Y%m%d-%H%M%S)"
PROD_SSH="<生产机 SSH 别名>"
REMOTE_DIR="~/apps/code-switch"
FRONTEND_TGZ="$ARTIFACT_DIR/codeswitch-frontend-dist.$STAMP.tgz"

tar -C frontend -czf "$FRONTEND_TGZ" dist
ssh "$PROD_SSH" "mkdir -p $REMOTE_DIR/frontend"
scp codeswitch-web "$PROD_SSH:$REMOTE_DIR/codeswitch-web.new"
scp "$FRONTEND_TGZ" "$PROD_SSH:$REMOTE_DIR/codeswitch-frontend-dist.$STAMP.tgz"

ssh "$PROD_SSH" "
  set -e
  cd $REMOTE_DIR

  rm -rf frontend/dist.new
  mkdir -p frontend/dist.new
  tar -xzf codeswitch-frontend-dist.$STAMP.tgz -C frontend/dist.new --strip-components=1

  if [ -f codeswitch-web ]; then
    cp codeswitch-web codeswitch-web.bak.$STAMP
  fi
  if [ -d frontend/dist ]; then
    rm -rf frontend/dist.bak.$STAMP
    mv frontend/dist frontend/dist.bak.$STAMP
  fi

  mv frontend/dist.new frontend/dist
  mv codeswitch-web.new codeswitch-web
  chmod +x codeswitch-web
"
```

### systemd 服务

以下命令需要服务器操作员执行：

```bash
REMOTE_DIR="$HOME/apps/code-switch"

sudo mkdir -p "$REMOTE_DIR/frontend"
sudo chown -R "$USER:$USER" "$HOME/apps"

ADMIN_DOMAIN="code.wcisman.cc"
SETUP_TOKEN="$(openssl rand -hex 32)"
echo "CODE_SWITCH_SETUP_TOKEN=$SETUP_TOKEN"

sudo tee /etc/systemd/system/codeswitch.service >/dev/null <<EOF
[Unit]
Description=Code Switch Web Service
After=network.target

[Service]
Type=simple
User=$USER
WorkingDirectory=$REMOTE_DIR
Environment=CODE_SWITCH_WEB_ADDR=127.0.0.1:8080
Environment=CODE_SWITCH_STATIC_DIR=$REMOTE_DIR/frontend/dist
Environment=CODE_SWITCH_PUBLIC_ORIGIN=https://$ADMIN_DOMAIN
Environment=CODE_SWITCH_TRUSTED_PROXIES=127.0.0.1/32,::1/128
Environment=CODE_SWITCH_SETUP_TOKEN=$SETUP_TOKEN
ExecStart=$REMOTE_DIR/codeswitch-web
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable codeswitch.service
```

保存输出的 `CODE_SWITCH_SETUP_TOKEN`。首次公网初始化管理员账号时需要它。

### Caddy 反向代理

以下命令需要服务器操作员执行：

```bash
sudo apt update
sudo apt install -y caddy

sudo cp /etc/caddy/Caddyfile /etc/caddy/Caddyfile.bak.$(date +%Y%m%d-%H%M%S) 2>/dev/null || true

ADMIN_DOMAIN="code.wcisman.cc"
API_DOMAIN="codeapi.wcisman.cc"

sudo tee /etc/caddy/Caddyfile >/dev/null <<EOF
$ADMIN_DOMAIN {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8080
}

$API_DOMAIN {
    encode zstd gzip
    reverse_proxy 127.0.0.1:18100
}
EOF

sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl enable --now caddy
sudo systemctl reload caddy
```

如果服务器使用 nftables 且默认 `policy drop`，需要放行 `80/443`。下面示例保留 SSH、HTTP、HTTPS、ICMP：

```bash
sudo cp /etc/nftables.conf /etc/nftables.conf.bak.$(date +%Y%m%d-%H%M%S)

sudo tee /etc/nftables.conf >/dev/null <<'EOF'
#!/usr/sbin/nft -f

flush ruleset

table inet filter {
        chain input {
                type filter hook input priority filter; policy drop;
                iif "lo" accept
                ct state established,related accept
                ct state invalid drop
                ip protocol icmp accept
                ip6 nexthdr ipv6-icmp accept
                tcp dport { 22, 80, 443 } accept
        }

        chain forward {
                type filter hook forward priority filter; policy drop;
        }

        chain output {
                type filter hook output priority filter; policy accept;
        }
}
EOF

sudo nft -c -f /etc/nftables.conf
sudo nft -f /etc/nftables.conf
```

启动服务：

```bash
sudo systemctl start codeswitch.service
sudo systemctl status codeswitch.service --no-pager -l
```

## 发布

后端通过 `CODE_SWITCH_STATIC_DIR` 从磁盘读取前端构建产物，不会把 `frontend/dist` 打进二进制。

发布执行边界：

- 发布者可以构建、上传、解压、备份和替换 `~/apps/code-switch` 下的产物。
- 发布者可以做不需要 sudo 的检查，例如 `curl http://127.0.0.1:8080/healthz`、`curl http://127.0.0.1:18100/v1/models`、`systemctl is-active codeswitch.service`。
- 发布者不能尝试任何 sudo 操作，也不能尝试重启 systemd 服务。即使认为当前用户可能有免密 sudo，也必须停止并通知操作员手动执行。

发布类型：

- 只改前端：上传 `frontend/dist`，通常不需要重启。
- 改了 Go 后端、路由、数据结构、relay、服务逻辑：上传 `codeswitch-web` 和 `frontend/dist`，然后由操作员重启服务。
- 只改 `scripts/manage-users` 或 `manage-users-bin`：只上传脚本和二进制，不需要重启服务。

### 纯前端发布

```bash
REPO_DIR="$(pwd)"
ARTIFACT_DIR="$(pwd)/.deploy"
mkdir -p "$ARTIFACT_DIR"

cd "$REPO_DIR/frontend"
npm install
npm run build

STAMP="$(date +%Y%m%d-%H%M%S)"
PROD_SSH="<生产机 SSH 别名>"
REMOTE_DIR="~/apps/code-switch"
FRONTEND_TGZ="$ARTIFACT_DIR/codeswitch-frontend-dist.$STAMP.tgz"

tar -C "$REPO_DIR/frontend" -czf "$FRONTEND_TGZ" dist
ssh "$PROD_SSH" "mkdir -p $REMOTE_DIR/frontend"
scp "$FRONTEND_TGZ" "$PROD_SSH:$REMOTE_DIR/codeswitch-frontend-dist.$STAMP.tgz"

ssh "$PROD_SSH" "
  set -e
  cd $REMOTE_DIR

  rm -rf frontend/dist.new
  mkdir -p frontend/dist.new
  tar -xzf codeswitch-frontend-dist.$STAMP.tgz -C frontend/dist.new --strip-components=1

  if [ -d frontend/dist ]; then
    rm -rf frontend/dist.bak.$STAMP
    mv frontend/dist frontend/dist.bak.$STAMP
  fi

  mv frontend/dist.new frontend/dist
"
```

### 后端或全量发布

```bash
REPO_DIR="$(pwd)"
ARTIFACT_DIR="$(pwd)/.deploy"
mkdir -p "$ARTIFACT_DIR"

cd "$REPO_DIR/frontend"
npm install
npm run build

cd "$REPO_DIR"
go test ./...
go build -o codeswitch-web .

STAMP="$(date +%Y%m%d-%H%M%S)"
PROD_SSH="<生产机 SSH 别名>"
REMOTE_DIR="~/apps/code-switch"
FRONTEND_TGZ="$ARTIFACT_DIR/codeswitch-frontend-dist.$STAMP.tgz"

tar -C frontend -czf "$FRONTEND_TGZ" dist
ssh "$PROD_SSH" "mkdir -p $REMOTE_DIR/frontend"
scp codeswitch-web "$PROD_SSH:$REMOTE_DIR/codeswitch-web.new"
scp "$FRONTEND_TGZ" "$PROD_SSH:$REMOTE_DIR/codeswitch-frontend-dist.$STAMP.tgz"

ssh "$PROD_SSH" "
  set -e
  cd $REMOTE_DIR

  rm -rf frontend/dist.new
  mkdir -p frontend/dist.new
  tar -xzf codeswitch-frontend-dist.$STAMP.tgz -C frontend/dist.new --strip-components=1

  if [ -f codeswitch-web ]; then
    cp codeswitch-web codeswitch-web.bak.$STAMP
  fi
  if [ -d frontend/dist ]; then
    rm -rf frontend/dist.bak.$STAMP
    mv frontend/dist frontend/dist.bak.$STAMP
  fi

  mv frontend/dist.new frontend/dist
  mv codeswitch-web.new codeswitch-web
  chmod +x codeswitch-web
"
```

到这里自动发布必须停止。发布者需要把本次 `STAMP` 一并发给服务器操作员；下面命令由操作员在生产机上手动执行重启和验证：

```bash
sudo systemctl restart codeswitch.service
sudo systemctl status codeswitch.service --no-pager -l
curl -fsS http://127.0.0.1:8080/healthz
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18100/v1/models
```

预期：

- `healthz` 返回 `{"ok":true}`。
- 未带 relay key 请求 `127.0.0.1:18100` 返回 `401`。

如果重启后异常，由操作员执行回滚：

```bash
STAMP="<本次发布时间戳，例如 20260706-205801>"
cd ~/apps/code-switch
cp codeswitch-web.bak.$STAMP codeswitch-web
rm -rf frontend/dist
mv frontend/dist.bak.$STAMP frontend/dist
chmod +x codeswitch-web
sudo systemctl restart codeswitch.service
sudo systemctl status codeswitch.service --no-pager -l
```

### 发布管理用户脚本

```bash
REPO_DIR="$(pwd)"
ARTIFACT_DIR="$(pwd)/.deploy"
mkdir -p "$ARTIFACT_DIR"

STAMP="$(date +%Y%m%d-%H%M%S)"
PROD_SSH="<生产机 SSH 别名>"
REMOTE_DIR="~/apps/code-switch"
MANAGE_USERS_BIN="$ARTIFACT_DIR/manage-users-bin.$STAMP"

go build -o "$MANAGE_USERS_BIN" ./cmd/manage-users
ssh "$PROD_SSH" "mkdir -p $REMOTE_DIR/scripts"
scp scripts/manage-users "$PROD_SSH:$REMOTE_DIR/scripts/manage-users.new"
scp "$MANAGE_USERS_BIN" "$PROD_SSH:$REMOTE_DIR/scripts/manage-users-bin.new"

ssh "$PROD_SSH" "
  set -e
  cd $REMOTE_DIR

  if [ -f scripts/manage-users ]; then
    cp scripts/manage-users scripts/manage-users.bak.$STAMP
  fi
  if [ -f scripts/manage-users-bin ]; then
    cp scripts/manage-users-bin scripts/manage-users-bin.bak.$STAMP
  fi

  mv scripts/manage-users.new scripts/manage-users
  mv scripts/manage-users-bin.new scripts/manage-users-bin
  chmod +x scripts/manage-users scripts/manage-users-bin
"
```

使用：

```bash
cd ~/apps/code-switch
scripts/manage-users list
scripts/manage-users add --username <name>
scripts/manage-users reset-password --username <name>
scripts/manage-users disable --username <name>
scripts/manage-users enable --username <name>
```

## 验证

在服务器本机验证：

```bash
ss -ltnp | grep -E ':(8080|18100) '
curl -fsS http://127.0.0.1:8080/healthz
curl -i -sS -X POST http://127.0.0.1:18100/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{}'
curl -i -sS -X POST http://127.0.0.1:18100/responses \
  -H 'Content-Type: application/json' \
  -d '{}'
```

预期：

- `8080` 和 `18100` 都有监听。
- `healthz` 返回 `{"ok":true}`。
- 未带 relay key 的 API 请求返回 `401`。
- `/chat/completions` 和 `/responses` 不应返回 `404`。

公网验证：

```bash
ADMIN_DOMAIN="code.wcisman.cc"
API_DOMAIN="codeapi.wcisman.cc"

curl -fsS "https://$ADMIN_DOMAIN/healthz"
curl -i -sS -X POST "https://$API_DOMAIN/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{}'
```


## 备份和恢复

生产备份方案：

```text
~/.code-switch
  -> 停 codeswitch.service 后打 tgz 冷备份
  -> restic 加密、去重、保留策略
  -> 腾讯云 COS 私有 Bucket 的 code-switch/prod 仓库
```

前提：

- 腾讯云 COS Bucket 已创建，访问权限为私有读写。
- COS 访问白名单已放行服务器出口 IP。
- CAM 密钥只授予这个 Bucket 的数据读写权限。
- 服务器上 `codeswitch.service` 已由 systemd 管理。

日常备份和跨服务器迁移使用同一个逻辑仓库：

```text
code-switch/prod
```

### 配置 COS 仓库

以下命令需要服务器操作员执行。替换 `SecretId`、`SecretKey`、地域和 Bucket 名：

```bash
sudo apt update
sudo apt install -y restic

sudo install -d -m 700 /etc/code-switch-backup

sudo tee /etc/code-switch-backup/cos.env >/dev/null <<'EOF'
AWS_ACCESS_KEY_ID='替换成腾讯云 SecretId'
AWS_SECRET_ACCESS_KEY='替换成腾讯云 SecretKey'
AWS_REGION='ap-tokyo'
AWS_DEFAULT_REGION='ap-tokyo'
RESTIC_REPOSITORY='s3:https://cos.ap-tokyo.myqcloud.com/code-switch-backups-1309705566/code-switch/prod'
RESTIC_PASSWORD_FILE='/etc/code-switch-backup/restic-password'
EOF

openssl rand -base64 48 | sudo tee /etc/code-switch-backup/restic-password >/dev/null

sudo chmod 600 /etc/code-switch-backup/cos.env
sudo chmod 600 /etc/code-switch-backup/restic-password
sudo cat /etc/code-switch-backup/restic-password
```

保存 `restic-password`。这个密码丢失后，云端备份无法解密恢复。

腾讯云 COS 2024-01-01 后创建的 Bucket 不支持 path-style 访问。所有 `restic` 命令都必须带：

```bash
-o s3.bucket-lookup=dns -o "s3.region=$AWS_REGION"
```

初始化：

```bash
sudo bash -c '
set -a
. /etc/code-switch-backup/cos.env
set +a
restic -o s3.bucket-lookup=dns -o "s3.region=$AWS_REGION" init
'
```

如果出现 `client.BucketExists: Access Denied`，检查：

- Bucket 地域和 endpoint 是否一致。
- COS 白名单是否包含服务器实际出口 IP。
- CAM 权限是否包含 Bucket list/head 和 object put/get/delete。
- 命令是否漏了 `-o s3.bucket-lookup=dns`。

### 备份脚本

```bash
sudo tee /usr/local/sbin/code-switch-backup-cos >/dev/null <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

CODE_SWITCH_USER="${CODE_SWITCH_USER:-chh}"
CODE_SWITCH_HOME="$(getent passwd "$CODE_SWITCH_USER" | cut -d: -f6)"
BACKUP_DIR="$CODE_SWITCH_HOME/backups/code-switch"
STAMP="$(date +%Y%m%d-%H%M%S)"
ARCHIVE="$BACKUP_DIR/code-switch-data.$STAMP.tgz"
ENV_FILE="/etc/code-switch-backup/cos.env"

if [ -z "$CODE_SWITCH_HOME" ]; then
  echo "cannot resolve home for user: $CODE_SWITCH_USER" >&2
  exit 1
fi

mkdir -p "$BACKUP_DIR"

systemctl stop codeswitch.service
trap 'systemctl start codeswitch.service' EXIT

tar -C "$CODE_SWITCH_HOME" -czf "$ARCHIVE" .code-switch
sha256sum "$ARCHIVE" > "$ARCHIVE.sha256"

systemctl start codeswitch.service
trap - EXIT

set -a
. "$ENV_FILE"
set +a

RESTIC_S3_OPTS=(-o "s3.bucket-lookup=dns" -o "s3.region=$AWS_REGION")

restic "${RESTIC_S3_OPTS[@]}" backup "$ARCHIVE" "$ARCHIVE.sha256"
restic "${RESTIC_S3_OPTS[@]}" forget \
  --keep-daily 7 \
  --keep-weekly 4 \
  --keep-monthly 6 \
  --prune

find "$BACKUP_DIR" \
  -type f \
  \( -name 'code-switch-data.*.tgz' -o -name 'code-switch-data.*.tgz.sha256' \) \
  -mtime +30 \
  -delete
EOF

sudo chmod 700 /usr/local/sbin/code-switch-backup-cos
```

手动执行：

```bash
sudo /usr/local/sbin/code-switch-backup-cos
```

查看快照：

```bash
sudo bash -c '
set -a
. /etc/code-switch-backup/cos.env
set +a
restic -o s3.bucket-lookup=dns -o "s3.region=$AWS_REGION" snapshots
'
```

### 定时备份

```bash
sudo mkdir -p /var/cache/code-switch-restic
sudo chown root:root /var/cache/code-switch-restic
sudo chmod 700 /var/cache/code-switch-restic

sudo tee /etc/systemd/system/code-switch-backup-cos.service >/dev/null <<'EOF'
[Unit]
Description=Back up Code Switch data to Tencent COS
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
Environment=XDG_CACHE_HOME=/var/cache/code-switch-restic
ExecStart=/usr/local/sbin/code-switch-backup-cos
EOF

sudo tee /etc/systemd/system/code-switch-backup-cos.timer >/dev/null <<'EOF'
[Unit]
Description=Daily Code Switch backup to Tencent COS

[Timer]
OnCalendar=*-*-* 04:10:00
Persistent=true
RandomizedDelaySec=10m

[Install]
WantedBy=timers.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now code-switch-backup-cos.timer
systemctl list-timers --all | grep code-switch
```

查看日志并确认备份成功：

```bash
sudo journalctl -u code-switch-backup-cos.service -n 120 --no-pager
```

正常日志应包含：

- `snapshot <id> saved`
- `code-switch-backup-cos.service: Deactivated successfully`
- `Finished code-switch-backup-cos.service`

如果出现 `unable to open cache: unable to locate cache directory`，说明 service 没有正确设置 `XDG_CACHE_HOME`，按上面的 service 文件重新写入并执行：

```bash
sudo systemctl daemon-reload
sudo systemctl restart code-switch-backup-cos.timer
```

### 恢复演练

定期把最新备份恢复到 `/tmp` 并校验哈希：

```bash
sudo rm -rf /tmp/code-switch-restore-test
sudo mkdir -p /tmp/code-switch-restore-test

sudo bash -c '
set -euo pipefail
set -a
. /etc/code-switch-backup/cos.env
set +a

RESTIC_S3_OPTS=(-o "s3.bucket-lookup=dns" -o "s3.region=$AWS_REGION")
restic "${RESTIC_S3_OPTS[@]}" restore latest --target /tmp/code-switch-restore-test

RESTORED_DIR="$(find /tmp/code-switch-restore-test -type d -path '*/backups/code-switch' | sort | tail -n 1)"
ls -lh "$RESTORED_DIR"
cd "$RESTORED_DIR"
sha256sum -c ./*.tgz.sha256
'
```

`sha256sum` 输出 `OK` 才说明备份文件完整。

### 正式恢复

正式恢复会替换 `~/.code-switch`。恢复前先给当前数据做本机备份：

```bash
sudo bash -c '
set -euo pipefail

RESTORE_ROOT="/tmp/code-switch-restore"
CODE_SWITCH_USER="${CODE_SWITCH_USER:-chh}"
CODE_SWITCH_HOME="$(getent passwd "$CODE_SWITCH_USER" | cut -d: -f6)"
CURRENT_BACKUP_DIR="$CODE_SWITCH_HOME/backups/code-switch"
STAMP="$(date +%Y%m%d-%H%M%S)"

if [ -z "$CODE_SWITCH_HOME" ]; then
  echo "cannot resolve home for user: $CODE_SWITCH_USER" >&2
  exit 1
fi

rm -rf "$RESTORE_ROOT"
mkdir -p "$RESTORE_ROOT" "$CURRENT_BACKUP_DIR"

set -a
. /etc/code-switch-backup/cos.env
set +a

RESTIC_S3_OPTS=(-o "s3.bucket-lookup=dns" -o "s3.region=$AWS_REGION")
restic "${RESTIC_S3_OPTS[@]}" restore latest --target "$RESTORE_ROOT"

RESTORED_BACKUP="$(find "$RESTORE_ROOT" -type f -path "*/backups/code-switch/code-switch-data.*.tgz" | sort | tail -n 1)"
RESTORED_SHA="$RESTORED_BACKUP.sha256"
cd "$(dirname "$RESTORED_BACKUP")"
sha256sum -c "$(basename "$RESTORED_SHA")"

systemctl stop codeswitch.service
trap "systemctl start codeswitch.service" EXIT

if [ -d "$CODE_SWITCH_HOME/.code-switch" ]; then
  tar -C "$CODE_SWITCH_HOME" -czf "$CURRENT_BACKUP_DIR/before-restore.$STAMP.tgz" .code-switch
fi

rm -rf "$CODE_SWITCH_HOME/.code-switch"
tar -C "$CODE_SWITCH_HOME" -xzf "$RESTORED_BACKUP"
chown -R "$CODE_SWITCH_USER:$CODE_SWITCH_USER" "$CODE_SWITCH_HOME/.code-switch"
chmod -R u+rwX,go-rwx "$CODE_SWITCH_HOME/.code-switch"

systemctl start codeswitch.service
trap - EXIT
systemctl status codeswitch.service --no-pager -l
'
```

恢复后执行“验证”里的本机和公网检查。

一次成功恢复应满足：

- restic 输出 `repository ... opened ... password is correct`。
- `sha256sum` 输出 `OK`。
- `codeswitch.service` 恢复为 `active (running)`。
- `curl http://127.0.0.1:8080/healthz` 返回 `{"ok":true}`。
- 未带 relay key 请求 `127.0.0.1:18100` 返回 `401`。

## 使用

首次打开后台：

1. 浏览器打开生产环境 Web 管理域名。
2. 创建管理员账号和密码。
3. 如果是首次公网初始化，输入启动日志或 systemd 配置里的 setup token。
4. 登录后配置供应商。

配置供应商：

1. 点击右上角 `+`。
2. 填写供应商名称、API URL、API Key。
3. 按需配置模型支持、模型映射和供应商池。
4. 保存。

建议至少配置两个供应商，这样自动降级才有意义。

CLI 代理：

- 在主界面为 Claude Code、Codex、Gemini CLI 或自定义 CLI 打开代理。
- 程序会把对应 CLI 配置指向 `127.0.0.1:18100`。
- 关闭代理会恢复原始直连配置。

生成 relay key：

1. 打开 `设置`。
2. 进入 `安全设置`。
3. 点击 `生成 key`。
4. 复制生成出来的 `csk_...` key。

调用示例：

```bash
curl http://127.0.0.1:18100/responses \
  -H "Authorization: Bearer csk_xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-codex","input":"hello"}'
```

## 故障排查

服务状态：

```bash
sudo systemctl status codeswitch.service --no-pager -l
sudo journalctl -u codeswitch.service -n 160 --no-pager
```

端口监听：

```bash
ss -ltnp | grep -E ':(8080|18100|80|443) '
```

Caddy 状态：

```bash
sudo systemctl status caddy --no-pager -l
sudo journalctl -u caddy -n 120 --no-pager
```

HTTPS 证书申请失败时，优先检查：

- 域名是否解析到当前服务器公网 IP。
- 服务器防火墙是否放行 `80/443`。
- 云厂商安全组或网络 ACL 是否放行 `80/443`。
- Caddyfile 是否包含正确域名。

如果公网 `80/443` 超时而 SSH 正常，常见原因是 nftables 或云防火墙未放行端口。

## 开发

当前开发方式不再是 `wails3 task dev`。

前端：

```bash
cd frontend
npm install
npm run build
```

后端：

```bash
conda activate code-switch-go-build-cgo
go run .
```

常用检查：

```bash
go build ./...
go test ./...
cd frontend && npm run build
```

## 技术栈

| 层级 | 技术 |
|------|------|
| 后端 | Go + Gin + SQLite |
| 前端 | Vue 3 + TypeScript + Vite |
| 通信 | HTTP RPC + SSE |
| 数据目录 | `~/.code-switch/` |

## 开源协议

MIT License

---

问题反馈：<https://github.com/Rogers-F/code-switch-R/issues>
