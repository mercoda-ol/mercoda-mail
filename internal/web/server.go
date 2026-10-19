package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"mercado-mail/internal/account"
	"mercado-mail/internal/auth"
	"mercado-mail/internal/classify"
	"mercado-mail/internal/config"
	"mercado-mail/internal/crypt"
	"mercado-mail/internal/events"
	imapclient "mercado-mail/internal/imap"
	"mercado-mail/internal/rules"
	"mercado-mail/internal/status"
	"mercado-mail/internal/store"
)

//go:embed static/*
var staticFS embed.FS

// Web 管理界面
type Server struct {
	store      *store.Store
	status     *status.Registry
	class      *classify.Classifier
	mgr        *account.Manager
	hub        *events.Hub
	keyPath    string
	tokenStore *auth.TokenStore
	pageSize   int           // 邮件列表每页条数（config web.email_page_size，默认 14）
	trTimeout  time.Duration // 翻译代理超时（config translate.timeout_seconds）

	// OAuth2 授权会话（按账号 ID，与 provider 无关）
	authMu   sync.Mutex
	authSess map[int64]*authSession
}

// authSession 单个账号的授权流程状态
type authSession struct {
	Status    string    `json:"status"` // pending / success / failed
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// New 创建 Web 服务器（pageSize 为邮件列表每页条数，<=0 时用默认 14；trCfg 翻译代理超时）
func New(st *store.Store, reg *status.Registry, cl *classify.Classifier, mgr *account.Manager, keyPath string, hub *events.Hub, ts *auth.TokenStore, pageSize int, trCfg config.Translate) *Server {
	if pageSize <= 0 {
		pageSize = 14
	}
	return &Server{store: st, status: reg, class: cl, mgr: mgr, keyPath: keyPath, hub: hub, tokenStore: ts, pageSize: pageSize, trTimeout: time.Duration(trCfg.TimeoutSeconds) * time.Second, authSess: map[int64]*authSession{}}
}

// Start 启动 HTTP 服务（阻塞）
func (s *Server) Start(port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /base.css", handleBaseCSS)
	mux.HandleFunc("GET /api/emails", s.handleEmails)
	mux.HandleFunc("GET /api/emails/{id}", s.handleEmailDetail)
	mux.HandleFunc("DELETE /api/emails/{id}", s.handleEmailDelete)
	mux.HandleFunc("POST /api/emails/batch_delete", s.handleEmailsBatchDelete)
	mux.HandleFunc("GET /api/unread_counts", s.handleUnreadCounts)
	mux.HandleFunc("POST /api/emails/{id}/read", s.handleMarkRead)
	mux.HandleFunc("POST /api/emails/read_all", s.handleMarkReadAll)
	mux.HandleFunc("GET /api/accounts", s.handleAccountsList)
	mux.HandleFunc("POST /api/accounts", s.handleAccountSave)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleAccountDelete)
	mux.HandleFunc("POST /api/accounts/{id}/authorize", s.handleAccountAuthorize)
	mux.HandleFunc("GET /api/accounts/{id}/auth_status", s.handleAuthStatus)
	mux.HandleFunc("GET /api/rules", s.handleRulesList)
	mux.HandleFunc("POST /api/rules", s.handleRuleSave)
	mux.HandleFunc("DELETE /api/rules/{id}", s.handleRuleDelete)
	mux.HandleFunc("POST /api/translate", s.handleTranslate)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	addr := ":" + strconv.Itoa(port)
	log.Printf("[web] 管理界面: http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("[web] HTTP 服务停止: %v", err)
	}
}

// handleTranslate 邮件翻译代理：本地 Edge Translator 不可用时前端走此端点，
// 后端转发 Google 免费端点（client=gtx，免 key），响应 {"text":"译文"}
func (s *Server) handleTranslate(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(code int, v interface{}) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}

	var req struct {
		Text   string `json:"text"`
		Source string `json:"source"` // 源语言（es/en/pt…）
		Target string `json:"target"` // 目标语言，默认 zh-CN
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeJSON(http.StatusBadRequest, map[string]string{"error": "文本为空"})
		return
	}
	if len(req.Text) > 20000 {
		writeJSON(http.StatusBadRequest, map[string]string{"error": "单段文本过长"})
		return
	}
	if req.Source == "" {
		req.Source = "es"
	}
	if req.Target == "" {
		req.Target = "zh-CN"
	}

	gu := "https://translate.googleapis.com/translate_a/single?client=gtx" +
		"&sl=" + url.QueryEscape(req.Source) +
		"&tl=" + url.QueryEscape(req.Target) +
		"&dt=t&q=" + url.QueryEscape(req.Text)

	client := &http.Client{Timeout: s.trTimeout}
	resp, err := client.Get(gu)
	if err != nil {
		writeJSON(http.StatusBadGateway, map[string]string{"error": "翻译服务不可达: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeJSON(http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("翻译服务返回 %d", resp.StatusCode)})
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		writeJSON(http.StatusBadGateway, map[string]string{"error": "读取翻译结果失败"})
		return
	}

	// Google 返回结构: [[["译文","原文",...],...], null, "源语言", ...]
	var outer []json.RawMessage
	if err := json.Unmarshal(body, &outer); err != nil || len(outer) == 0 {
		writeJSON(http.StatusBadGateway, map[string]string{"error": "翻译结果解析失败"})
		return
	}
	var segs []json.RawMessage
	if err := json.Unmarshal(outer[0], &segs); err != nil {
		writeJSON(http.StatusBadGateway, map[string]string{"error": "翻译结果解析失败"})
		return
	}
	var sb strings.Builder
	for _, raw := range segs {
		var item []interface{}
		if json.Unmarshal(raw, &item) == nil && len(item) > 0 {
			if t, ok := item[0].(string); ok {
				sb.WriteString(t)
			}
		}
	}
	writeJSON(http.StatusOK, map[string]string{"text": sb.String()})
}

// handleEvents SSE 实时事件流（新邮件/告警秒级推送）
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持 SSE", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	// 首帧连接确认 + 心跳（防中间设备断开）
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}

// indexPage 首页静态内容启动时加载一次（embed FS 每请求 ReadFile 会重复分配 54KB）
var indexPage = func() []byte {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		panic("static/index.html 加载失败: " + err.Error())
	}
	return b
}()

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexPage)
}

// baseCSS 公共样式（第21轮抽离）启动时加载一次
var baseCSS = func() []byte {
	b, err := staticFS.ReadFile("static/base.css")
	if err != nil {
		panic("static/base.css 加载失败: " + err.Error())
	}
	return b
}()

func handleBaseCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Write(baseCSS)
}

func (s *Server) handleEmails(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize <= 0 {
		pageSize = s.pageSize
	}
	list, total, err := s.store.ListEmails(store.EmailFilter{
		Account:  q.Get("account"),
		Category: q.Get("category"),
		Query:    q.Get("q"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": list, "total": total, "page_size": pageSize})
}

func (s *Server) handleEmailDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效 ID"})
		return
	}
	e, err := s.store.GetEmail(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if e == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "邮件不存在"})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleUnreadCounts(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.UnreadCounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效 ID"})
		return
	}
	if err := s.store.MarkRead(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleMarkReadAll(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	if err := s.store.MarkReadAll(category); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleEmailDelete 删除邮件：先删远端（IMAP MOVE/EXPUNGE），成功后删本地
func (s *Server) handleEmailDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效 ID"})
		return
	}
	e, err := s.store.GetEmail(id)
	if err != nil || e == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "邮件不存在"})
		return
	}

	acc, err := s.store.GetAccount(e.Account)
	if err != nil || acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在: " + e.Account})
		return
	}

	// 构造 IMAP 连接配置（app_password 需解密）
	cfg, err := s.buildMailConfig(acc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := imapclient.DeleteRemoteEmail(cfg, s.tokenStore, e.Folder, e.UID); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "服务器删除失败: " + err.Error()})
		return
	}

	if err := s.store.DeleteEmail(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "本地清理失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// buildMailConfig 构造 IMAP 连接配置（app_password 密码需解密）
func (s *Server) buildMailConfig(acc *store.Account) (config.Account, error) {
	cfg := config.Account{
		Name:     acc.Name,
		Email:    acc.Email,
		Provider: acc.Provider,
		IMAP:     config.IMAP{Host: acc.IMAPHost, Port: acc.IMAPPort},
		Auth:     config.Auth{Type: acc.AuthType, Username: acc.AuthUsername},
	}
	if acc.AuthType == "app_password" && acc.AuthPasswordEnc != "" {
		plain, err := crypt.DecryptString(s.keyPath, acc.AuthPasswordEnc)
		if err != nil {
			return cfg, fmt.Errorf("解密密码失败: %w", err)
		}
		cfg.Auth.Password = plain
	}
	return cfg, nil
}

type batchDeleteFail struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
}

// handleEmailsBatchDelete 批量删除：按(账号,文件夹)分组复用连接远端删除，
// 远端成功才删本地；返回 {ok, failed:[{id,error}]}
func (s *Server) handleEmailsBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效 ids"})
		return
	}
	if len(req.IDs) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "单次最多 500 封"})
		return
	}

	failed := make([]batchDeleteFail, 0)
	type item struct {
		id  int64
		uid uint32
	}
	groups := make(map[string][]item) // key: account\x00folder

	for _, id := range req.IDs {
		e, err := s.store.GetEmail(id)
		if err != nil || e == nil {
			failed = append(failed, batchDeleteFail{ID: id, Error: "邮件不存在"})
			continue
		}
		key := e.Account + "\x00" + e.Folder
		groups[key] = append(groups[key], item{id: id, uid: e.UID})
	}

	var localDelete []int64
	for key, items := range groups {
		parts := strings.SplitN(key, "\x00", 2)
		accountName, folder := parts[0], parts[1]

		acc, err := s.store.GetAccount(accountName)
		if err != nil || acc == nil {
			for _, it := range items {
				failed = append(failed, batchDeleteFail{ID: it.id, Error: "账号不存在: " + accountName})
			}
			continue
		}
		cfg, err := s.buildMailConfig(acc)
		if err != nil {
			for _, it := range items {
				failed = append(failed, batchDeleteFail{ID: it.id, Error: err.Error()})
			}
			continue
		}

		uids := make([]uint32, 0, len(items))
		for _, it := range items {
			uids = append(uids, it.uid)
		}
		deleted, uidFailed := imapclient.DeleteRemoteEmails(cfg, s.tokenStore, folder, uids)

		uid2id := make(map[uint32]int64, len(items))
		for _, it := range items {
			uid2id[it.uid] = it.id
		}
		for _, uid := range deleted {
			localDelete = append(localDelete, uid2id[uid])
		}
		for uid, e := range uidFailed {
			failed = append(failed, batchDeleteFail{ID: uid2id[uid], Error: e.Error()})
		}
	}

	if len(localDelete) > 0 {
		if err := s.store.DeleteEmails(localDelete); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "本地清理失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": len(localDelete), "failed": failed})
}

// accountVO 账号视图对象（密码脱敏）
type accountVO struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Provider     string `json:"provider"`
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	AuthType     string `json:"auth_type"`
	AuthUsername string `json:"auth_username"`
	Online       bool   `json:"online"`
	HasPassword  bool   `json:"has_password"`
	LastError    string `json:"last_error"`
	LastSync     string `json:"last_sync"`
}

func (s *Server) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 合并在线状态
	statusMap := map[string]status.Status{}
	for _, st := range s.status.Snapshot() {
		statusMap[st.Name] = st
	}

	items := make([]accountVO, 0, len(accounts))
	for _, a := range accounts {
		vo := accountVO{
			ID: a.ID, Name: a.Name, Email: a.Email, Provider: a.Provider,
			IMAPHost: a.IMAPHost, IMAPPort: a.IMAPPort,
			AuthType: a.AuthType, AuthUsername: a.AuthUsername,
			HasPassword: a.AuthPasswordEnc != "",
		}
		if st, ok := statusMap[a.Name]; ok {
			vo.Online = st.Online
			vo.LastError = st.LastError
			if !st.LastSync.IsZero() {
				vo.LastSync = st.LastSync.Format("2006-01-02T15:04:05Z07:00")
			}
		}
		items = append(items, vo)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

// accountReq 保存账号的请求体
type accountReq struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Provider     string `json:"provider"`
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	AuthType     string `json:"auth_type"`
	AuthUsername string `json:"auth_username"`
	AuthPassword string `json:"auth_password"` // 明文提交，入库前加密
}

func (s *Server) handleAccountSave(w http.ResponseWriter, r *http.Request) {
	var req accountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if req.Name == "" || req.Email == "" || req.Provider == "" || req.IMAPHost == "" || req.AuthType == "" || req.AuthUsername == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "名称/邮箱/服务商/IMAP/认证类型/用户名均不能为空"})
		return
	}

	acc := &store.Account{
		ID: req.ID, Name: req.Name, Email: req.Email, Provider: req.Provider,
		IMAPHost: req.IMAPHost, IMAPPort: req.IMAPPort,
		AuthType: req.AuthType, AuthUsername: req.AuthUsername,
	}

	// 密码：提交了新密码则加密；编辑时留空则保留原密码
	if req.AuthPassword != "" {
		enc, err := crypt.EncryptString(s.keyPath, req.AuthPassword)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "密码加密失败: " + err.Error()})
			return
		}
		acc.AuthPasswordEnc = enc
	} else if req.ID > 0 {
		old, err := s.store.GetAccount(req.Name)
		if err == nil {
			acc.AuthPasswordEnc = old.AuthPasswordEnc
		}
	}

	id, err := s.store.InsertAccount(acc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 动态生效：解密密码转 config.Account 并启动/替换 worker
	cfgAcc, err := s.toConfigAccount(acc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "账号已保存但启动失败: " + err.Error()})
		return
	}
	s.mgr.Add(cfgAcc)

	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效 ID"})
		return
	}
	acc, err := s.store.GetAccountByID(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.DeleteAccount(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.mgr.Remove(acc.Name)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleAccountAuthorize 发起 OAuth2 首次授权（通用：按账号 provider 自动分流 outlook/gmail）
func (s *Server) handleAccountAuthorize(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效 ID"})
		return
	}
	acc, err := s.store.GetAccountByID(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if acc.AuthType != "oauth2" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "该账号不是 OAuth2 类型，无需授权"})
		return
	}

	// 防并发：同账号已有进行中的授权则直接返回
	s.authMu.Lock()
	if sess, ok := s.authSess[id]; ok && sess.Status == "pending" {
		s.authMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	}
	s.authSess[id] = &authSession{Status: "pending", UpdatedAt: time.Now()}
	s.authMu.Unlock()

	// 起 goroutine 跑通用授权流程（auth.Authorize 内部按 provider 选端点/Scope并自动开浏览器）
	cfgAcc, err := s.toConfigAccount(acc)
	if err != nil {
		s.setAuthResult(id, "failed", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取账号失败: " + err.Error()})
		return
	}
	go func() {
		log.Printf("[auth] 账号 %s 发起 OAuth2 授权（provider=%s）", acc.Name, acc.Provider)
		if err := auth.Authorize(cfgAcc, s.tokenStore); err != nil {
			log.Printf("[auth] 账号 %s 授权失败: %v", acc.Name, err)
			s.setAuthResult(id, "failed", err.Error())
			return
		}
		log.Printf("[auth] 账号 %s 授权成功", acc.Name)
		s.setAuthResult(id, "success", "")
		// 立即重启该账号收信：拿到新 token 直连（否则等退避最长数分钟），同时自愈旧的状态残留
		s.mgr.Add(cfgAcc)
		log.Printf("[auth] 账号 %s 已触发立即重连", acc.Name)
	}()

	writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
}

// handleAuthStatus 查询授权状态
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效 ID"})
		return
	}
	s.authMu.Lock()
	sess, ok := s.authSess[id]
	if !ok {
		s.authMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "idle"})
		return
	}
	out := *sess
	s.authMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) setAuthResult(id int64, status, errMsg string) {
	s.authMu.Lock()
	s.authSess[id] = &authSession{Status: status, Error: errMsg, UpdatedAt: time.Now()}
	s.authMu.Unlock()
}

// toConfigAccount 把持久化账号转为运行时账号（解密密码）
func (s *Server) toConfigAccount(a *store.Account) (config.Account, error) {
	cfg := config.Account{
		Name: a.Name, Email: a.Email, Provider: a.Provider,
		IMAP: config.IMAP{Host: a.IMAPHost, Port: a.IMAPPort},
		Auth: config.Auth{Type: a.AuthType, Username: a.AuthUsername},
	}
	if a.AuthPasswordEnc != "" {
		plain, err := crypt.DecryptString(s.keyPath, a.AuthPasswordEnc)
		if err != nil {
			return cfg, err
		}
		cfg.Auth.Password = plain
	}
	return cfg, nil
}

func (s *Server) handleRulesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListRules()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": list})
}

func (s *Server) handleRuleSave(w http.ResponseWriter, r *http.Request) {
	var rule store.Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if rule.Name == "" || rule.Category == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "名称和分类不能为空"})
		return
	}
	id, err := rules.Save(s.store, s.class, &rule)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id})
}

func (s *Server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效 ID"})
		return
	}
	if err := rules.Delete(s.store, s.class, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
