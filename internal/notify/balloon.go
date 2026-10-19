package notify

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

//go:embed icon.ico
var trayIconData []byte

// 托盘气泡通知：Shell_NotifyIconW 直发（替代 powershell -Command，
// 无外部进程启动开销）；常驻托盘，单击交互走回调（双击/气泡/右键菜单
// 均由 main 注入 onOpen/onQuit，双击打开应用窗口而非浏览器）。
var (
	user32            = syscall.NewLazyDLL("user32.dll")
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	shell32           = syscall.NewLazyDLL("shell32.dll")
	winmm             = syscall.NewLazyDLL("winmm.dll")
	procRegisterCls   = user32.NewProc("RegisterClassExW")
	procCreateWin     = user32.NewProc("CreateWindowExW")
	procGetMessage    = user32.NewProc("GetMessageW")
	procTranslateMsg  = user32.NewProc("TranslateMessage")
	procDispatchMsg   = user32.NewProc("DispatchMessageW")
	procDefWndProc    = user32.NewProc("DefWindowProcW")
	procPostQuitMsg   = user32.NewProc("PostQuitMessage")
	procLoadImage     = user32.NewProc("LoadImageW")
	procShellNotify   = shell32.NewProc("Shell_NotifyIconW")
	procGetModule     = kernel32.NewProc("GetModuleHandleW")
	procPlaySound     = winmm.NewProc("PlaySoundW")
	procCreateMenu    = user32.NewProc("CreatePopupMenu")
	procAppendMenu    = user32.NewProc("AppendMenuW")
	procTrackMenu     = user32.NewProc("TrackPopupMenu")
	procGetCursorPos  = user32.NewProc("GetCursorPos")
	procSetForeground = user32.NewProc("SetForegroundWindow")
	procPostMessage   = user32.NewProc("PostMessageW")
	procDestroyWin    = user32.NewProc("DestroyWindow")
)

const (
	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10

	wmApp        = 0x8000
	wmTrayIcon   = wmApp + 1
	wmLBtnDblClk = 0x0203
	wmLBtnUp     = 0x0202
	wmRBtnUp     = 0x0205
	wmContextMenu = 0x007B // WM_CONTEXTMENU（v4 右键在 hi 字）
	wmClose      = 0x0010
	wmNull       = 0x0000
	ninSelect      = 0x0400 // NIN_SELECT（v4 单击/选中）
	ninKeySelect   = 0x0401 // NIN_KEYSELECT
	ninBalloonClick = 0x0405 // NIN_BALLOONUSERCLICK（legacy 回调）
	niifInfo     = 0x1

	wmDestroy = 0x0002

	// 右键菜单
	menuOpen   = 2001
	menuQuit   = 2002
	mfString   = 0x0000
	mfSep      = 0x0800
	tpmRightBt = 0x0002 // 鼠标右键选择菜单项
	tpmBottom  = 0x0020 // 以鼠标位置底对齐
	tpmRetCmd  = 0x0100 // TrackPopupMenu 直接返回命令 ID

	sndAsync   = 0x00000001
	sndNodeflt = 0x00000002
	sndFile    = 0x00020000

	wsOverlapped = 0x00000000

	lrLoadFromFile = 0x10
	lrDefaultSize  = 0x40
	imageIcon      = 1
)

// soundFiles 系统提示音白名单 → %WINDIR%\media 音效文件（实证本机存在）
var soundFiles = map[string]string{
	"Default":  "Windows Notify System Generic.wav",
	"IM":       "Windows Notify Messaging.wav",
	"Call":     "Windows Ringin.wav",
	"Reminder": "Windows Notify Calendar.wav",
	"SMS":      "Windows Message Nudge.wav",
	"Alarm":    "Alarm03.wav",
}

type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UNion            uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	WndProc       uintptr
	ClsExtra      int32
	WndExtra      int32
	Instance      uintptr
	Icon          uintptr
	Cursor        uintptr
	Background    uintptr
	MenuName      *uint16
	ClassName     *uint16
	IconSm        uintptr
}

type pointXY struct{ X, Y int32 }

type msgW struct {
	Hwnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       pointXY
	LPrivate uint32
}

const trayIconID = 1

var (
	trayMu     sync.Mutex
	trayWnd    uintptr
	trayOnce   sync.Once
	trayReady  = make(chan struct{})
	trayWndPtr = syscall.NewCallback(trayWndProc)
	// 回调由 main 注入：双击/气泡/菜单「打开」→ onOpen；菜单「退出」→ onQuit
	trayOnOpen func()
	trayOnQuit func()
)

// trayWndProc 托盘窗口过程：双击/点气泡 → onOpen（开窗）；右键 → 菜单。
// 消息格式实证：本机 Shell 发 NOTIFYICON_VERSION_4 风格（lo=事件ID 0x402/0x404、
// hi=鼠标消息），老式 v0 则 lo=鼠标消息——两种都判，防止 case 永不命中（双击无反应）
func trayWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmTrayIcon:
		lo := uint16(lparam & 0xFFFF)
		hi := uint16(lparam >> 16)
		log.Printf("[tray] 托盘消息 lparam=0x%X lo=0x%X hi=0x%X", lparam, lo, hi)
		open := lo == wmLBtnDblClk || hi == wmLBtnDblClk ||
			lo == ninSelect || lo == ninKeySelect ||
			lo == 0x0402 // 实证 08:11 用户双击时段产生，按打开处理（持续观察日志校准）
		menu := lo == wmRBtnUp || hi == wmRBtnUp || hi == wmContextMenu
		balloon := lo == ninBalloonClick
		switch {
		case open || balloon:
			if trayOnOpen != nil {
				trayOnOpen()
			}
		case menu:
			showTrayMenu(hwnd)
		}
		return 0
	case wmDestroy:
		procPostQuitMsg.Call(0)
		return 0
	}
	ret, _, _ := procDefWndProc.Call(hwnd, msg, wparam, lparam)
	return ret
}

// showTrayMenu 右键菜单：打开界面 / 退出（TPM_RETURNCMD 直接返回命令 ID）
func showTrayMenu(hwnd uintptr) {
	hMenu, _, _ := procCreateMenu.Call()
	if hMenu == 0 {
		return
	}
	openTxt, _ := syscall.UTF16PtrFromString("打开界面")
	quitTxt, _ := syscall.UTF16PtrFromString("退出")
	procAppendMenu.Call(hMenu, mfString, menuOpen, uintptr(unsafe.Pointer(openTxt)))
	procAppendMenu.Call(hMenu, mfSep, 0, 0)
	procAppendMenu.Call(hMenu, mfString, menuQuit, uintptr(unsafe.Pointer(quitTxt)))

	// 弹出前 SetForegroundWindow（否则菜单不自动消失），弹完补 WM_NULL
	procSetForeground.Call(hwnd)
	var pt pointXY
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	cmd, _, _ := procTrackMenu.Call(
		hMenu,
		tpmRightBt|tpmBottom|tpmRetCmd,
		uintptr(pt.X), uintptr(pt.Y),
		0,
		hwnd, 0,
	)
	procPostMessage.Call(hwnd, wmNull, 0, 0)

	switch int32(cmd) {
	case menuOpen:
		if trayOnOpen != nil {
			trayOnOpen()
		}
	case menuQuit:
		if trayOnQuit != nil {
			trayOnQuit()
		}
	}
}

// InitTray 启动托盘图标（幂等）：onOpen 双击/气泡/菜单打开；onQuit 菜单退出
func InitTray(onOpen, onQuit func()) {
	trayOnce.Do(func() {
		trayOnOpen = onOpen
		trayOnQuit = onQuit
		go trayThread()
		<-trayReady
	})
}

// ShutdownTray 移除托盘图标并结束托盘消息循环（主动退出路径调用；
// 进程直接退出时系统自动回收图标）
func ShutdownTray() {
	trayMu.Lock()
	wnd := trayWnd
	trayWnd = 0
	trayMu.Unlock()
	if wnd == 0 {
		return
	}
	nid := notifyIconDataW{
		CbSize: uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:   wnd,
		UID:    trayIconID,
	}
	procShellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	// DestroyWindow 只能由窗口线程调用：PostMessage WM_CLOSE 让窗口线程自己销毁
	procPostMessage.Call(wnd, wmClose, 0, 0)
}

// trayThread 创建隐藏窗口 + 注册托盘 + 消息循环（窗口与循环必须同线程）；
// LockOSThread 固定线程防 goroutine 迁移（受控实验未复现迁移，属防御性加固：
// 消息入队到建窗线程，循环跑在别的线程会取消息不到）
func trayThread() {
	runtime.LockOSThread()
	className, _ := syscall.UTF16PtrFromString("MercadoMailTrayWnd")
	hInst, _, _ := procGetModule.Call(0)
	hIcon := loadTrayIcon()

	wc := wndClassExW{
		CbSize:    uint32(unsafe.Sizeof(wndClassExW{})),
		WndProc:   trayWndPtr,
		Instance:  hInst,
		Icon:      hIcon,
		ClassName: className,
	}
	if r, _, _ := procRegisterCls.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		log.Printf("[notify] 托盘窗口类注册失败")
		close(trayReady)
		return
	}

	hwnd, _, _ := procCreateWin.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0, // 空标题
		wsOverlapped,
		0, 0, 0, 0,
		0,    // 普通隐藏顶层窗口（不 Show 即不可见）
		0, hInst, 0,
	)
	if hwnd == 0 {
		log.Printf("[notify] 托盘窗口创建失败")
		close(trayReady)
		return
	}

	nid := notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:             hwnd,
		UID:              trayIconID,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTrayIcon,
		HIcon:            hIcon,
	}
	copyUTF16(nid.SzTip[:], "Mercado 邮件管理")

	if r, _, err := procShellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); r == 0 {
		log.Printf("[notify] 托盘图标添加失败: %v", err)
		close(trayReady) // 不关会导致 InitTray 永久阻塞
		return
	}

	trayMu.Lock()
	trayWnd = hwnd
	trayMu.Unlock()
	close(trayReady)

	// 消息循环（阻塞直到窗口销毁；进程退出时窗口销毁，系统回收托盘图标）
	var m msgW
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMsg.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMsg.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// loadTrayIcon 内嵌 ICO 落临时文件后 LoadImage
func loadTrayIcon() uintptr {
	if len(trayIconData) == 0 {
		return 0
	}
	icoPath := filepath.Join(os.TempDir(), "mercado-mail-tray.ico")
	if err := os.WriteFile(icoPath, trayIconData, 0644); err != nil {
		log.Printf("[notify] 写托盘图标失败: %v", err)
		return 0
	}
	p, _ := syscall.UTF16PtrFromString(icoPath)
	h, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(p)), imageIcon, 0, 0, lrLoadFromFile|lrDefaultSize)
	return h
}

// playSound 播系统音效文件（白名单映射，异步不阻塞）
func playSound(sound string) {
	f, ok := soundFiles[sound]
	if !ok {
		f = soundFiles["Default"]
	}
	windir := os.Getenv("WINDIR")
	if windir == "" {
		windir = `C:\Windows`
	}
	path, _ := syscall.UTF16PtrFromString(filepath.Join(windir, "media", f))
	procPlaySound.Call(uintptr(unsafe.Pointer(path)), 0, sndFile|sndAsync|sndNodeflt)
}

// copyUTF16 复制到 UTF16 缓冲（超长截断并保证结尾 NUL）
func copyUTF16(dst []uint16, s string) {
	b, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	n := copy(dst, b)
	if n == len(dst) {
		dst[len(dst)-1] = 0
	}
}

// showToast 发气泡通知 + 播提示音（title 截 63 字符、body 截 255 字符防溢出）
func showToast(title, body, sound string) error {
	if rs := []rune(title); len(rs) > 60 {
		title = string(rs[:60]) + "…"
	}
	if rs := []rune(body); len(rs) > 250 {
		body = string(rs[:250]) + "…"
	}
	playSound(sound)

	trayMu.Lock()
	defer trayMu.Unlock()
	if trayWnd == 0 {
		return fmt.Errorf("托盘未初始化")
	}
	nid := notifyIconDataW{
		CbSize:      uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:        trayWnd,
		UID:         trayIconID,
		UFlags:      nifInfo,
		DwInfoFlags: niifInfo,
	}
	copyUTF16(nid.SzInfoTitle[:], title)
	copyUTF16(nid.SzInfo[:], strings.TrimSpace(body))
	if r, _, err := procShellNotify.Call(nimModify, uintptr(unsafe.Pointer(&nid))); r == 0 {
		return fmt.Errorf("Shell_NotifyIcon 失败: %v", err)
	}
	log.Printf("[notify] 气泡已弹出: title=%q body=%q", title, body)
	return nil
}
