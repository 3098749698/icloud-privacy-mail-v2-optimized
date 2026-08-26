# 部署说明

适用于 Linux amd64 Docker 部署，也可用于本地测试。生产环境建议使用独立域名和 HTTPS 反向代理。

## 准备

需要 Docker Engine、Docker Compose plugin 和 ca-certificates。创建目录并限制权限：

```bash
sudo mkdir -p /opt/icloud-privacy-mail-v2/{releases,backups}
sudo chmod 700 /opt/icloud-privacy-mail-v2
```

## 上传和配置

```bash
tar -xzf ipm2-v2.*.tar.gz -C /opt/icloud-privacy-mail-v2
cd /opt/icloud-privacy-mail-v2
cp config.example.json config.json
${EDITOR:-vi} config.json
chmod 600 config.json
```

不要把真实 `config.json`、`data/`、Apple 登录态、IMAP 授权码、OAuth token 或 API key 提交到 GitHub。

## 构建和启动

```bash
docker compose build --pull
docker compose up -d
docker compose ps
docker inspect --format='{{.State.Health.Status}}' icloud-privacy-mail-v2
curl http://127.0.0.1:8788/api/health
```

健康检查应返回 `success: true` 和当前版本。首次启动后登录管理界面，保存 IMAP 授权，再验证收件箱和垃圾箱取码。

## 反向代理

将域名反向代理到 `127.0.0.1:8788`，只开放 80/443。验证码接口必须使用 HTTPS；反代读取超时建议至少 35 秒，以覆盖等待邮件到达的请求。

## 数据备份

```bash
mkdir -p backups/$(date +%Y%m%d-%H%M%S)
docker run --rm -v icloud-privacy-mail-v2_ipm2-data:/data:ro -v "$PWD/backups":/backup alpine tar czf /backup/data-$(date +%Y%m%d-%H%M%S).tar.gz -C /data .
```

备份包含账号和授权状态，必须限制权限并存放在受控位置。迁移时同时带上状态文件和 `state.json.apple-credentials.key`。

## 升级与回滚

每次升级前保存源码包、镜像标签和数据备份：

```bash
docker compose down
docker compose build --pull
docker compose up -d
curl http://127.0.0.1:8788/api/health
```

健康检查失败时恢复上一镜像和升级前数据备份。发布包附带的 `ROLLBACK.sh` 只回退源码，生产数据回退必须使用对应备份。

## 取码性能检查

对比验证码的 `received_at`、`detected_at` 和 API 返回时间：

- `received_at -> detected_at` 长：检查 IMAP 连接、QQ Junk 分类和监听器。
- `detected_at -> API response` 长：检查反代超时、服务器负载和数据库锁。
- 只有 Junk 延迟：保留 Junk IDLE 与低频兜底，不要关闭兜底扫描。

同一 QQ 账户可以服务多个注册线程，程序会合并同步请求，不需要为每个线程重复登录 IMAP。
