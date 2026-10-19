package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// SetupResult setup 页提交的选择
type SetupResult struct {
	Mode      string `json:"mode"`
	ServerURL string `json:"server_url"`
}

// RunSetupWithURL 启动本地回环 setup 服务，通过 openWindow 回调打开选择页；
// 阻塞直到用户提交选择
func RunSetupWithURL(openWindow func(url string)) (SetupResult, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return SetupResult{}, fmt.Errorf("setup 端口监听失败: %w", err)
	}
	defer ln.Close()

	resultCh := make(chan SetupResult, 1)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile("static/setup.html")
		if err != nil {
			http.Error(w, "setup 页加载失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	mux.HandleFunc("POST /setup/choose", func(w http.ResponseWriter, r *http.Request) {
		var res SetupResult
		if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
			json.NewEncoder(w).Encode(map[string]string{"error": "参数解析失败"})
			return
		}
		if res.Mode != "server" && res.Mode != "client" {
			json.NewEncoder(w).Encode(map[string]string{"error": "无效模式"})
			return
		}
		if res.Mode == "client" && res.ServerURL == "" {
			json.NewEncoder(w).Encode(map[string]string{"error": "请填写服务地址"})
			return
		}
		// 先写响应，前端收到后再由主程序关窗
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
		select {
		case resultCh <- res:
		default:
		}
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	setupURL := fmt.Sprintf("http://%s/setup", ln.Addr().String())
	openWindow(setupURL)

	res := <-resultCh
	// 留出响应写出时间
	time.Sleep(300 * time.Millisecond)
	return res, nil
}
