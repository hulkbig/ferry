# Ferry 官网

官网使用 VitePress，包含中英文首页、安装、Hermes 接入、FAQ、排障、贡献和安全文档。构建后是静态文件，运行时不需要 Node.js、数据库或后端服务。

## 构建

需要 Python 3 和 Node.js 22.12 或更新的兼容版本。在仓库根目录执行：

```sh
npm ci --prefix website
python3 scripts/build_website.py /tmp/ferry-website
```

输出目录必须位于仓库之外。GitHub Pages 构建使用 `--base /`；内网预览可用默认的相对路径，或传入实际部署路径。向用户交付预览时使用已验证的内网 HTTPS 入口。

`scripts/prepare_website.py` 从仓库 Markdown 生成文档页面，避免维护两份正文。已纳入站点的文档链接指向站内页面，其他详细参考链接指向 GitHub。生成目录、依赖和构建缓存不提交。

依赖固定为 VitePress `2.0.0-alpha.21`、Vue `3.5.44`，以锁文件为准。采用此预发布版本是为了使用已修复已知依赖漏洞的 Vite 工具链；升级前重跑构建、依赖审计、链接及浏览器检查。当前版本不作为生产 HTTP 服务运行。

## 六张动画

```sh
python3 scripts/build_gateway_diagrams.py
```

生成六个场景的中英文 SVG：请求发出、流式返回、备用接力、排队限流、逻辑模型切换（Hermes 为案例）、AutoReset 临期重置券兑换。右侧节点按场景分别绘制，中心仅保留 Ferry 图标与字样。README 引用 SVG 图片，官网内联同一份资产并提供独立暂停与重播。没有 GIF、视频或外部图片请求。

动画使用原生 SVG 时间轴。官网离开视口或后台标签页时暂停；减少动态效果模式默认静止，也允许用户主动播放。README 的动画支持取决于阅读器，静态节点保留含义；GitHub 实际渲染在公开版本上线后核对。

配图是流程示意，不是真实流量或渠道认证。品牌来源见 `docs/asset-provenance.md`，许可证随 `licenses.txt` 发布。

构建还会生成 `assets/third-party-licenses.json`，收录实际打包的 VitePress、Vue 等浏览器依赖的版本和许可原文。该文件与静态站点一起分发。

## 独立文档源码包

在产品仓库根目录运行 `python3 -B scripts/export_docs.py ../ferry-doc`，可导出独立的 VitePress 源码包。目标必须是尚不存在的目录；重复导出应使用新目录，审阅差异后再替换旧包，避免覆盖已有修改。

进入导出目录后执行 `npm ci`、`FERRY_SITE_BASE=/ferry/ npm run build` 即可构建，不依赖相邻产品源码。`content/` 和 `public/` 在此包中是需要保留的源码，而不是可以忽略的生成缓存。默认产物为 `.vitepress/dist/`；部署根路径或其他子路径时调整 `FERRY_SITE_BASE`。本命令不初始化 Git、不创建远端、不发布站点。

## 发布与检查

公开地址为 `https://ferry.bighulk.dev/`，以 Pages 部署回执为准。首次发行后，将公开仓库 Pages 来源设为 GitHub Actions，再从准确 `main` 运行 `Website`。工作流限定公有 `hulkbig/ferry`，部署前检查公开时间窗口，并核对 `v0.1.0` 的安装附件。

修改后检查中英文、明暗主题、320px 窄屏、独立动画控制、搜索、导航与子页面直接刷新。链接验收须检查 HTTP 200 和实际内容，不能将 404 页渲染成功视为通过。复制按钮需在 HTTPS 或本机可信上下文中测试。

独立域名使用根路径 `/` 构建，生成文件包含 `CNAME: ferry.bighulk.dev`。发布时先在新产品仓库 Pages 中绑定该域名，再在 DNS 配置 `ferry` 的 CNAME 指向 `hulkbig.github.io`，完成证书签发及 HTTPS 验证。Actions 部署以仓库 Pages 设置为准，产物中的 CNAME 不能替代域名绑定。不要修改账号博客仓库的 CNAME。文档源码准备不表示 DNS、证书或 Pages 已开通。
