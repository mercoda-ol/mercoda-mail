package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"mercado-mail/internal/account"
	"mercado-mail/internal/accounts"
	"mercado-mail/internal/auth"
	"mercado-mail/internal/classify"
	"mercado-mail/internal/config"
	"mercado-mail/internal/events"
	"mercado-mail/internal/notify"
	"mercado-mail/internal/prefs"
	"mercado-mail/internal/rules"
	"mercado-mail/internal/store"
	"mercado-mail/internal/web"
	"mercado-mail/internal/window"
)

// hideConsole 使本进程脱离控制台（双击启动不再有 DOS 黑框）
// Windows Terminal 托管下 ShowWindow(GetConsoleWindow()) 无效，FreeConsole 脱离后窗口/标签即关
// 日志已由 setupLog 写文件，脱离后仍可排查
func hideConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procFreeConsole := kernel32.NewProc("FreeConsole")
	procFreeConsole.Call()
}

// ===== 窗口子进程管理：托盘双击/启动共用，可重入（开窗或激活已有窗） =====
var (
	ua32                = syscall.NewLazyDLL("user32.dll")
	procFindWindow      = ua32.NewProc("FindWindowW")
	procShowWindow      = ua32.NewProc("ShowWindow")
	procSetForegroundWd = ua32.NewProc("SetForegroundWindow")

	winMu        sync.Mutex
	winCmd       *exec.Cmd
	shuttingDown bool
	devtoolsOn   bool // WebView2 DevTools 开关（server=配置 web.devtools，client=-devtools flag）
)

// winTitle 须与 window.Open 的窗口标题一致（FindWindowW 激活用）
const winTitle = "Mercado 邮件管理"

// openWindow 拉起/激活 WebView2 窗口子进程：有活窗则激活，无则重开；
// 正常关窗(exit 0)主进程驻留托盘不退出，panic(code≠0)降级系统浏览器
func openWindow(url string) {
	log.Println("[window] 开窗请求（托盘双击/菜单/启动）")
	winMu.Lock()
	if winCmd != nil && winCmd.Process != nil {
		if activateWindowLocked() {
			winMu.Unlock()
			return
		}
		winCmd.Process.Kill() // 找不到窗口（僵死）→杀掉重开
		winCmd = nil
	}
	exe, err := os.Executable()
	if err != nil {
		winMu.Unlock()
		openBrowser(url)
		return
	}
	args := []string{"-window-child", url}
	if devtoolsOn {
		args = append(args, "-devtools")
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := cmd.Start(); err != nil {
		winMu.Unlock()
		log.Printf("[window] 窗口子进程启动失败: %v，降级为浏览器", err)
		openBrowser(url)
		return
	}
	winCmd = cmd
	winMu.Unlock()

	go func() {
		err := cmd.Wait()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		winMu.Lock()
		replaced := winCmd != cmd
		if !replaced {
			winCmd = nil
		}
		sd := shuttingDown
		winMu.Unlock()
		if replaced || sd {
			return // 已被重开替换或主动关停：静默，不降级
		}
		if code != 0 {
			log.Printf("[window] 窗口异常退出(code=%d)，降级为系统浏览器访问 %s", code, url)
			openBrowser(url)
			return
		}
		log.Println("窗口已关闭，驻留托盘（双击托盘图标重新打开）")
	}()
}

// activateWindowLocked 按标题激活已有窗口（调用方须持有 winMu）
func activateWindowLocked() bool {
	t, _ := syscall.UTF16PtrFromString(winTitle)
	hwnd, _, _ := procFindWindow.Call(0, uintptr(unsafe.Pointer(t)))
	if hwnd == 0 {
		return false
	}
	procShowWindow.Call(hwnd, 9) // SW_RESTORE
	procSetForegroundWd.Call(hwnd)
	return true
}

// shutdownWin 主动关停窗口子进程（Wait 由开窗 goroutine 处理，sd 分支静默）
func shutdownWin() {
	winMu.Lock()
	shuttingDown = true
	if winCmd != nil && winCmd.Process != nil {
		winCmd.Process.Kill()
	}
	winMu.Unlock()
}

// rotateWriter 按大小轮转的日志 writer：单文件超过 limit 即滚动为 .1~.keep，
// 磁盘占用封顶 ≈ limit×(keep+1)，防止日志风暴打爆磁盘（曾 26GB 事故）
type rotateWriter struct {
	mu    *sync.Mutex
	f     *os.File
	path  string
	n     int64
	limit int64
	keep  int
}

var logW *rotateWriter

// setupLog 日志写文件（控制台已脱离，stderr 不可用；auth 子命令不经过这里）
// 先用默认上限开写，config.Load 成功后由 setLogLimit 更新为配置值
func setupLog() {
	w, err := newRotateWriter("mercado-mail.log", 20<<20, 3)
	if err != nil {
		return
	}
	logW = w
	log.SetOutput(w)
}

func newRotateWriter(path string, limit int64, keep int) (*rotateWriter, error) {
	w := &rotateWriter{mu: &sync.Mutex{}, path: path, limit: limit, keep: keep}
	if fi, err := os.Stat(path); err == nil && fi.Size() > limit {
		w.rotateLocked() // 启动时已超限：先滚动
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	w.f = f
	return w, nil
}

// setLogLimit 用配置值更新上限（config.Load 成功后调用一次）
func setLogLimit(mb, keep int) {
	if logW == nil || mb <= 0 {
		return
	}
	logW.mu.Lock()
	logW.limit = int64(mb) << 20
	if keep > 0 {
		logW.keep = keep
	}
	logW.mu.Unlock()
}

func (w *rotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n > 0 && w.n+int64(len(p)) > w.limit {
		w.rotateLocked()
	}
	if w.f == nil {
		return 0, os.ErrClosed
	}
	n, err := w.f.Write(p)
	w.n += int64(n)
	return n, err
}

// rotateLocked 关闭当前文件、按 keep 滚动历史（.keep 删除、.i→.i+1、log→.1）后重建
func (w *rotateWriter) rotateLocked() {
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	for i := w.keep - 1; i >= 1; i-- {
		os.Rename(w.path+"."+itoa(i), w.path+"."+itoa(i+1))
	}
	os.Rename(w.path, w.path+".1")
	if f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		w.f = f
	}
	w.n = 0
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

func main() {
	// 控制台已脱离，panic 打印无处可去，捕获后写日志
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[panic] %v\n%s", r, debug.Stack())
		}
	}()

	// 子命令: mercado-mail auth -account <账号名>  （OAuth2 首次授权）
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		authCmd(os.Args[2:])
		return
	}

	// 桌面/服务模式：隐藏 DOS 黑框 + 日志写文件（auth 子命令保持控制台可见）
	setupLog()
	hideConsole()

	// 内部子进程: 独立窗口模式（隔离 WebView2 panic）
	if len(os.Args) > 1 && os.Args[1] == "-window-child" {
		// 第三个参数 -devtools 由主进程按配置 web.devtools 附加
		debug := len(os.Args) > 3 && os.Args[3] == "-devtools"
		window.Open(os.Args[2], debug)
		return
	}

	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	noWindow := flag.Bool("no-window", false, "不打开桌面窗口，纯后台运行")
	mode := flag.String("mode", "", "启动模式: server（后台服务+窗口）/ client（仅窗口）")
	serverURL := flag.String("server", "", "client 模式连接的服务地址，如 http://192.168.1.100:8080")
	forceSetup := flag.Bool("setup", false, "强制显示启动模式选择")
	devtoolsFlag := flag.Bool("devtools", false, "WebView2 开 DevTools（client 模式用；server 模式由配置 web.devtools 控制）")
	flag.Parse()

	// client 模式无配置文件，DevTools 走命令行；server 模式在 runServer 内用配置覆盖
	devtoolsOn = *devtoolsFlag

	// 读取偏好（记忆上次选择）
	prefsPath := "prefs.json"
	p, _ := prefs.Load(prefsPath)

	// 模式优先级：命令行 > -setup 强制选择 > 偏好 > 首次选择
	if *forceSetup || (*mode == "" && *serverURL == "" && p.Mode == "") {
		res, err := web.RunSetupWithURL(openSetupWindow)
		closeSetupWindow() // 用户已提交，关掉选择窗口
		if err != nil {
			log.Fatalf("启动模式选择失败: %v", err)
		}
		p = prefs.Prefs{Mode: res.Mode, ServerURL: res.ServerURL}
		if err := prefs.Save(prefsPath, p); err != nil {
			log.Printf("[setup] 保存偏好失败: %v", err)
		}
	}
	if *mode != "" {
		p.Mode = *mode
	}
	if *serverURL != "" {
		p.ServerURL = *serverURL
		if p.Mode == "" {
			p.Mode = "client"
		}
	}

	if p.Mode == "client" {
		runClient(p.ServerURL)
		return
	}
	runServer(cfgPath, *noWindow)
}

// openSetupWindow 用子进程打开 setup 窗口
var setupWinCmd *exec.Cmd

func openSetupWindow(url string) {
	exe, err := os.Executable()
	if err == nil {
		setupWinCmd = exec.Command(exe, "-window-child", url)
		setupWinCmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
		if err := setupWinCmd.Start(); err != nil {
			log.Printf("[setup] 打开选择窗口失败: %v，可改用浏览器 %s", err, url)
			openBrowser(url)
			return
		}
		return
	}
	openBrowser(url)
}

// closeSetupWindow 关闭 setup 窗口
func closeSetupWindow() {
	if setupWinCmd != nil && setupWinCmd.Process != nil {
		setupWinCmd.Process.Kill()
		setupWinCmd.Wait()
		setupWinCmd = nil
	}
}

// runClient 客户端模式：仅打开窗口连接服务地址，不收信不起服务
func runClient(serverURL string) {
	if serverURL == "" {
		fmt.Fprintln(os.Stderr, "client 模式需要服务地址: mercado-mail.exe -mode client -server http://192.168.1.100:8080")
		os.Exit(1)
	}
	log.Printf("[client] 仅窗口模式，连接服务: %s", serverURL)
	window.Open(serverURL, devtoolsOn)
	log.Println("窗口已关闭，程序退出")
}

// runServer 服务端模式：收信 + HTTP + 本机窗口
func runServer(cfgPath *string, noWindow bool) {
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	setLogLimit(cfg.Log.MaxSizeMB, cfg.Log.Keep)
	devtoolsOn = cfg.Web.Devtools

	st, err := store.New(cfg.Database.Path)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer st.Close()

	cl := classify.New(cfg.Rules)
	n := notify.New(cfg.Notify.Enabled, cfg.Notify.Sound, cfg.Notify.MinInterval, cfg.Notify.CategorySounds, cfg.Notify.OnlyCategories)
	ts := auth.NewTokenStore("secrets.enc", "secrets.key")

	// 规则：DB 有则用 DB，否则从 config.yaml 导入；支持 Web 在线编辑热更新
	if err := rules.Load(st, cl, cfg.Rules); err != nil {
		log.Fatalf("加载规则失败: %v", err)
	}

	// 账号：DB 有则用 DB，否则从 config.yaml 导入（密码加密存储）
	loadedAccounts, err := accounts.Load(st, cfg.Accounts, "secrets.key")
	if err != nil {
		log.Fatalf("加载账号失败: %v", err)
	}

	log.Printf("Mercado 邮件分类工具启动，账号数: %d", len(loadedAccounts))

	// SSE 事件广播中心：收信后秒级推送给前端窗口
	hub := events.NewHub()
	mgr := account.New(loadedAccounts, st, cl, n, ts, hub, cfg.Spam, cfg.Imap)

	// 退出信号：托盘菜单「退出」与系统信号共用同一收尾路径
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	if cfg.Web.Enabled {
		// 托盘常驻：双击/气泡/菜单「打开」→ 开窗或激活（不再弹浏览器）；
		// 菜单「退出」→ 杀窗口+移除图标+统一收尾
		notify.InitTray(
			func() { openWindow(fmt.Sprintf("http://localhost:%d", cfg.Web.Port)) },
			func() {
				go func() {
					log.Println("托盘退出请求，正在关停...")
					shutdownWin()
					notify.ShutdownTray()
					quit <- syscall.SIGTERM
				}()
			},
		)
		go web.New(st, mgr.Status, cl, mgr, "secrets.key", hub, ts, cfg.Web.EmailPageSize, cfg.Translate).Start(cfg.Web.Port)
	}

	mgr.Start()

	if cfg.Web.Enabled && !noWindow {
		// 桌面窗口跑在独立子进程：WebView2 偶发 panic 时不影响收信主进程；
		// 正常关窗主进程驻留托盘（openWindow 内处理），panic 降级系统浏览器
		go func() {
			time.Sleep(800 * time.Millisecond) // 等 HTTP 服务就绪
			openWindow(fmt.Sprintf("http://localhost:%d", cfg.Web.Port))
		}()
	}

	// 阻塞等待退出信号（账号 goroutine 均为后台运行）
	<-quit
	log.Println("收到退出信号，正在停止...")
	shutdownWin()
	notify.ShutdownTray()
}

// openBrowser 用系统默认浏览器打开 URL（降级方案）
// 不能用 cmd /c start：URL 含 & 时会被 cmd 当命令分隔符截断，rundll32 不经 cmd 解析
func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	if err := cmd.Start(); err != nil {
		log.Printf("[window] 打开浏览器失败: %v，请手动访问 %s", err, url)
	}
}

// authCmd OAuth2 首次授权子命令（账号从 DB 读取，支持 Web 新增的账号）
func authCmd(args []string) {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "配置文件路径")
	accountName := fs.String("account", "", "账号名（账号管理中的名称）")
	fs.Parse(args)

	if *accountName == "" {
		fmt.Fprintln(os.Stderr, "用法: mercado-mail auth -account <账号名>")
		os.Exit(1)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	st, err := store.New(cfg.Database.Path)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer st.Close()

	// 兜底：DB 无账号时先从 config 导入
	if n, _ := st.CountAccounts(); n == 0 {
		if _, err := accounts.Load(st, cfg.Accounts, "secrets.key"); err != nil {
			log.Fatalf("导入账号失败: %v", err)
		}
	}

	acc, err := st.GetAccount(*accountName)
	if err != nil {
		log.Fatalf("未找到账号: %s（请检查账号管理中的名称）", *accountName)
	}

	target := config.Account{
		Name: acc.Name, Email: acc.Email, Provider: acc.Provider,
		IMAP: config.IMAP{Host: acc.IMAPHost, Port: acc.IMAPPort},
		Auth: config.Auth{Type: acc.AuthType, Username: acc.AuthUsername},
	}

	ts := auth.NewTokenStore("secrets.enc", "secrets.key")
	if err := auth.Authorize(target, ts); err != nil {
		log.Fatalf("授权失败: %v", err)
	}
	fmt.Printf("账号 %s 授权成功，refresh token 已加密保存。\n", target.Name)
}
