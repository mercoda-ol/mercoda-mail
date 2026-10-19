# Mercado Mail — MercadoLibre 店铺邮件接收分类工具

面向 MercadoLibre 多店铺运营的邮件接收与分类工具：一个店铺一个邮箱账号（Gmail / Outlook），
实时收信、按主题关键词自动分类入库、重要类别弹 Windows 系统通知，附带 WebView2 桌面管理界面。

## 目录

- [一、来源](#一来源)
- [二、项目优势](#二项目优势)
- [三、系统要求](#三系统要求)
- [四、使用方法](#四使用方法)
- [五、编译方法](#五编译方法)
- [六、配置说明](#六配置说明)
- [七、目录结构](#七目录结构)
- [八、文档索引](#八文档索引)
- [九、安全须知](#九安全须知)
- [十、许可证](#十许可证)

## 一、来源

- **本地自研项目**，非 fork、无开源上游；git 仓库当前无提交历史、无远端。
- 起因：MercadoLibre 店铺运营邮件量大（10+ 账号、一店铺一账号），需实时收信并按主题
  （中英葡混合关键词）分类，及时捕捉订单/异常订单/账号安全/客服类重要邮件。
- 实施历程与现状、协作规则均记录在内部文档（`docs/` 与 `AGENTS.md`，**内部使用、不随仓库分发**）。

## 二、项目优势

| 优势 | 说明 |
|---|---|
| 秒级实时收信 | IMAP IDLE 事件驱动 + UID 增量，不轮询、不重复入库 |
| 低资源占用 | Go 单二进制、SQLite(WAL) 单连接池、正文 256KB 截断入库 |
| 多账号隔离 | 每账号独立 goroutine + 独立连接，单账号异常不影响其它账号 |
| 断线自愈 | 指数退避重连（10s→300s，成功归零）+ IDLE 连续失败阈值断开重连，防半死连接日志风暴 |
| 六类自动分类 | 异常订单/订单/账号安全/客服支持/商品优化/其他，按 priority 首个命中；规则存 DB，Web 在线编辑即热更新 + 全量重分类 |
| 分级通知 | 网页内全部提示；Windows 系统通知（托盘气泡+提示音）仅订单/异常订单/账号安全/客服支持四类，且有频率限流 |
| 桌面体验 | WebView2 独立子进程开窗（崩溃不影响收信主进程）、系统托盘常驻、亮/暗双主题、邮件 HTML 详情 + 一键翻译 |
| 可运维性 | 日志按大小轮转（默认 20MB×3，磁盘占用封顶）、SSE 秒级推送前端、批量删除远端优先 |
| 零外部进程 | 通知用 Shell_NotifyIconW + PlaySound，翻译优先本地 Edge Translator，不依赖 PowerShell 脚本 |

## 三、系统要求

- **运行**：Windows 10 及以上（manifest 已标 win10）；Edge WebView2 Runtime（Win10 通常自带，
  缺失时自动降级为系统浏览器访问 `http://localhost:8080`）
- **构建**：Go 1.26.8+（`go.mod` 声明 go 1.26.8），Windows 环境（含 Win32 API / WebView2 依赖）
- **网络**：可访问 Gmail / Outlook IMAP（OAuth2 需可回调 `http://localhost`）

## 四、使用方法

### 4.1 首次启动

双击 `mercado-mail.exe`（工作目录=项目根，需 `config.yaml` 在旁）：
首次弹出启动模式选择窗口（`-setup` 可强制重选），选择后写入 `prefs.json` 记忆。

| 模式 | 行为 |
|---|---|
| `server` | 收信主进程 + HTTP 服务 + 本机桌面窗口（默认） |
| `client` | 仅开窗连接远程服务地址，不收信不起服务：`mercado-mail.exe -mode client -server http://192.168.1.100:8080` |

**凭据文件无需手工创建**：clone 后首次运行自动生成 `secrets.key`（32 字节随机密钥），
首次授权/保存账号凭据时自动创建 `secrets.enc`（AES-GCM 加密的 refresh token 与密码）；
两者已在 `.gitignore` 中，不会被提交。旧机器迁移时须**同时**拷贝 `secrets.enc` + `secrets.key`
（缺 key 则密文无法解密）。

### 4.2 运行行为

- **启动即开窗**（WebView2 子进程，标题「Mercado 邮件管理」）；Web 地址 `http://localhost:<web.port>`（默认 8080）
- **关窗不退出**：驻留系统托盘；托盘**双击/菜单「打开」**重新开窗，**右键菜单「退出」**才真退出
- 无 DOS 黑框（`FreeConsole`），日志写 `mercado-mail.log`（按大小轮转为 `.1/.2`）
- 异常退出降级：窗口子进程 panic 时主进程驻留托盘并回退系统浏览器

### 4.3 Web 管理界面

- **邮件**：多选/翻页/搜索/分类筛选、详情抽屉（HTML iframe 渲染 + 翻译按钮）、单删与批量删除（远端优先）、已读/未读统计
- **账号管理**：新增账号、OAuth 授权按钮（授权成功立即以新 token 重启该账号收信）、在线状态实时（SSE）
- **规则管理**：分类规则增删改，保存即热更新并全量重分类
- **通知**：仅 `notify.only_categories` 四类弹系统通知；网页内 toast 不受限制
- **主题**：顶栏 ☀/🌙 切换，`localStorage` 持久化

### 4.4 账号认证

| 提供商 | 方式 |
|---|---|
| Gmail | OAuth2 或应用专用密码（`auth.type: app_password`） |
| Outlook | 仅 OAuth2（微软已关闭 Basic Auth） |

- refresh token 以 AES-GCM 加密存于 `secrets.enc`（密钥 `secrets.key`），静默续期
- **Outlook 无需自己注册应用**：使用内置 Azure 公共应用 ClientID + PKCE（源码不含也不需要
  ClientSecret，桌面应用标准做法）；授权行为记在该共享应用名下，token 只存使用者本机
- Gmail 的内置 Google OAuth ClientID 尚未配置（代码 TODO），Gmail 请用应用专用密码
- 首次授权两条路径：Web 界面「授权」按钮，或命令行子命令
  `mercado-mail.exe auth -account <账号名>`（账号需已在 DB/config 中存在）

### 4.5 命令行参数

```text
-config <path>   配置文件路径（默认 config.yaml）
-no-window       不打开桌面窗口，纯后台运行
-mode <s>        启动模式 server / client
-server <url>    client 模式连接的服务地址
-setup           强制显示启动模式选择
-devtools        WebView2 开 DevTools（client 模式；server 模式由 web.devtools 配置控制）
auth -account    OAuth2 首次授权子命令
```

## 五、编译方法

### 5.1 验证三件套（改码后必跑）

```powershell
go build ./...
go vet ./...
go test ./...
```

### 5.2 构建 exe

```powershell
# 先停旧进程，避免文件占用
Stop-Process -Name mercado-mail -Force -ErrorAction SilentlyContinue
go build -o mercado-mail.exe ./cmd
# 工作目录=项目根启动
Start-Process .\mercado-mail.exe
```

### 5.3 图标/版本资源（可选）

图标源与资源描述在 `cmd/winres/`（`winres.json` + 多尺寸 PNG），
`rsrc_windows_*.syso` 已随 `cmd/` 打包进 exe；换图标时**必须在 `cmd/` 目录下执行**：

```powershell
Set-Location cmd
go-winres make      # 重新生成 syso（需 go install github.com/tc-hib/go-winres@latest）
Set-Location ..
go build -o mercado-mail.exe ./cmd
```

> 文件管理器小图标显示旧图时为系统 iconcache 缓存：删除 `iconcache_*.db`（先结束 explorer.exe 再重启）。

### 5.4 交叉/32 位

`cmd/` 下已有 amd64 与 386 两份 syso，按目标架构编译即可，无需额外步骤。

## 六、配置说明

**配置铁律**：所有业务参数统一在 `config.yaml`，业务代码不读环境变量、不私自增配项。
配置段概览：

| 段 | 关键项 | 说明 |
|---|---|---|
| `database` | `path` | SQLite 文件（WAL） |
| `notify` | `enabled` / `sound` / `min_interval_seconds` / `category_sounds` / `only_categories` | 系统通知开关、限流、分类提示音、系统通知白名单 |
| `web` | `enabled` / `port` / `email_page_size` / `devtools` | HTTP 服务与界面 |
| `imap` | `reconnect_interval` / `reconnect_max_backoff` / `idle_timeout` / `idle_fail_threshold` / `batch_size` / `max_body_bytes` | 重连退避与 IDLE 调优 |
| `log` | `max_size_mb` / `keep` | 日志轮转（磁盘封顶 ≈ max×(keep+1)） |
| `translate` | `timeout_seconds`（上游地址 `translate.upstream` 可配） | 后端翻译代理 |
| `spam` | `enabled` / `poll_seconds` / `folder` | 垃圾箱轮询收取 |
| `rules` | `name` / `category` / `alert` / `keywords` | 分类规则（**仅 DB 为空时导入**；实际生效以 SQLite `rules` 表为准，在 Web 界面改） |
| `accounts` | `name` / `email` / `provider` / `imap` / `auth` | 邮箱账号（**仅 DB 为空时导入**，密码加密入库） |

改规则请走 Web 界面或 `rules` 链路（保存即全量重分类）；直接改 yaml 不会覆盖已有 DB 数据。
`config.yaml` 中的规则词表需与 `internal/classify/classify_test.go` 的 `testRules` 保持同步。

## 七、目录结构

```text
mercado-mail/
├── cmd/main.go              入口（FreeConsole 隐藏黑框、文件日志轮转、defer recover、auth 子命令、窗口子进程管理）
│   ├── cmd/winres/          图标源 PNG + winres.json（图标更换需在 cmd/ 下 go-winres make）
├── internal/
│   ├── config/              YAML 加载 + 全部默认值兜底
│   ├── account/             多账号 goroutine 管理 + 连接状态 + 同名替换去重
│   ├── accounts/            账号从 config 导入 DB（密码加密）
│   ├── auth/                OAuth2 授权 + refresh token 加密 + XOAUTH2
│   ├── imap/                IDLE 收信、UID 增量、指数退避、spam 轮询、单删/批量删远端
│   ├── classify/            标题关键词匹配（首个命中）+ 单测
│   ├── store/               SQLite（emails/rules/accounts/sync_state）
│   ├── notify/              托盘气泡 + 声音 + 分类白名单 + 限流
│   ├── window/              WebView2 桌面窗（jchv/go-webview2）
│   ├── web/                 HTTP 路由 + 内嵌单页（static/index.html + base.css 双主题）
│   └── events/ rules/ status/ crypt/ prefs/   SSE、规则热更新、连接状态、加解密、启动偏好
├── config.yaml              全部配置（含规则源头文档、only_categories、imap 调优）
└── secrets.enc / secrets.key  OAuth refresh token 加密存储（勿外泄，不入库）
```

## 八、文档索引

> 本仓库不含内部文档：`AGENTS.md`（协作规则）与 `docs/`（实施历史、轮次计划）
> 均**不入库、不随仓库分发**，仅在内部环境保留。

## 九、安全须知

- `secrets.enc` / `secrets.key` 为凭据加密文件与密钥，`mercado-mail.db` 含账号与邮件数据，
  **均不得提交仓库或外发**；`config.yaml` 内账号密码请用占位符，真实凭据走加密入库
- 本 README 与入库文档不记录任何 OAuth Client Secret / token；共享 Client 配置记录于内部文档（不随仓库分发）
- OAuth 回调仅 `http://localhost`，请勿将 Web 服务端口暴露公网（无鉴权）

## 十、许可证

[MIT](LICENSE) © 2026 kerbs
