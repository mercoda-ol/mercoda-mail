package window

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

//go:embed icon.ico
var iconData []byte

// Open 打开 WebView2 桌面窗口加载管理界面（阻塞直到窗口关闭）；
// debug=true 开 DevTools（F12/右键检查），由配置 web.devtools 控制
func Open(url string, debug bool) {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     debug,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Mercado 邮件管理",
			Width:  1200,
			Height: 800,
		},
	})
	if w == nil {
		log.Printf("[window] WebView2 初始化失败（缺少 Edge WebView2 Runtime？），改用浏览器访问 %s", url)
		return
	}
	defer w.Destroy()

	setWindowIcon(w.Window())

	w.Navigate(url)
	w.Run()
}

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procSetClassLong = user32.NewProc("SetClassLongPtrW")
	procLoadImage    = user32.NewProc("LoadImageW")
	procSendMessage  = user32.NewProc("SendMessageW")
)

const (
	gclpHicon      = ^uintptr(13) // -14, GCLP_HICON
	gclpHiconSm    = ^uintptr(33) // -34, GCLP_HICONSM
	lrLoadFromFile = 0x10
	lrDefaultSize  = 0x40
	imageIcon      = 1
	wmSetIcon      = 0x0080
)

// setWindowIcon 把内嵌 ICO 落到临时文件后 LoadImage 设置窗口图标
func setWindowIcon(hwnd unsafe.Pointer) {
	if hwnd == nil || len(iconData) == 0 {
		return
	}

	icoPath := filepath.Join(os.TempDir(), "mercado-mail-icon.ico")
	if err := os.WriteFile(icoPath, iconData, 0644); err != nil {
		log.Printf("[window] 写图标临时文件失败: %v", err)
		return
	}

	pathPtr, _ := syscall.UTF16PtrFromString(icoPath)
	hIcon, _, _ := procLoadImage.Call(
		0,
		uintptr(unsafe.Pointer(pathPtr)),
		imageIcon,
		0, 0,
		lrLoadFromFile|lrDefaultSize,
	)
	if hIcon == 0 {
		log.Printf("[window] LoadIcon 失败")
		return
	}

	procSetClassLong.Call(uintptr(hwnd), gclpHicon, hIcon)
	procSetClassLong.Call(uintptr(hwnd), gclpHiconSm, hIcon)
	procSendMessage.Call(uintptr(hwnd), wmSetIcon, 1, hIcon) // ICON_BIG
	procSendMessage.Call(uintptr(hwnd), wmSetIcon, 0, hIcon) // ICON_SMALL
}
