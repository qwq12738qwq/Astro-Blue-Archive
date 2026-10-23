# Astro-Blue-Archive

一个文件优先的个人博客 CMS：文章和页面是 `content/` 里的纯 Markdown，是唯一的事实来源；数据库只保存运行时状态；Astro 渲染每一个页面，Go 后端只在回环上提供 JSON API。

> **开发中。** 接口与存储结构仍可能变动。已经定下来的部分记在 [`ARCHITECTURE.md`](ARCHITECTURE.md) 里。

## 部署

生产部署用容器：

```bash
cp container/.env.example container/.env && $EDITOR container/.env
docker compose -f container/docker-compose.yml up -d --build
```

一个服务装两个进程，站点发布在 `PUBLISH_ADDRESS:PUBLISH_PORT`（默认 `127.0.0.1:4321`，两个值都在 `container/.env` 里改）。三个数据目录——`content/`（文章与展示层）、`media/`（上传的原图）、`data/`（数据库、会话密钥、备份）——以**相对路径**挂载，所以它们留在宿主机上：改完 Markdown 刷新即可，不用重新构建。

镜像由 GitHub Actions 构建并发布到 GHCR：推 `main` 或 `v*` tag 时发布，拉取请求只构建不推送。

**反向代理和 TLS 不在项目的部署范围内**，由你自己在前面架。`PUBLIC_ORIGIN` 要填浏览器实际访问的地址——Origin 检查拿它和访客的 origin 比对。

## 开发

```bash
cd astro && npm install && npm run dev        # 站点
cd backend && go run ./cmd/server            # API
```

需要 **Go 1.25+**、**Node 22+**、**npm**。内容改动在两种模式下都是即时的：内容用 live collection 读，不在构建期快照。

门禁：`make verify`（全部）、`make test-all`（四套测试）、`make arch`（架构不变量）。

## 许可

MIT，见 [LICENSE](LICENSE)。
