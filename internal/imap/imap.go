package imapclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"mercado-mail/internal/auth"
	"mercado-mail/internal/classify"
	"mercado-mail/internal/config"
	"mercado-mail/internal/events"
	"mercado-mail/internal/notify"
	"mercado-mail/internal/status"
	"mercado-mail/internal/store"
)

// Handler 单账号收信处理器
type Handler struct {
	cfg      config.Account
	store    *store.Store
	class    *classify.Classifier
	notifier *notify.Notifier
	tokens   *auth.TokenStore
	status   *status.Registry
	hub      *events.Hub
	wake     chan struct{} // 服务端推送时唤醒 IDLE 循环

	// 垃圾箱轮询
	spamCfg      config.Spam
	spamFolder   string // 探测到的垃圾箱文件夹，空则跳过
	lastSpamPoll time.Time

	// IMAP 运行参数（config.yaml imap 段）
	tune config.ImapTuning

	// 本次同步累积统计（syncFolder 开始置位，结束 flushSync 清空）：告警聚合避免逐封弹提示
	accum *syncAccum

	// 本次 runOnce 是否曾成功连接：连上过则断线退避重置（治"成功进 IDLE 后再断退避不归零锁死 5 分钟"）
	everConnected bool
}

// pendingAlert 聚合缓冲条目
type pendingAlert struct {
	category string
	subject  string
	from     string
}

// syncAccum 单次同步的新增统计
type syncAccum struct {
	newEmails int
	alerts    []pendingAlert
	first     *pendingAlert // 首封（含 from，单封事件用）
}

// NewHandler 创建账号处理器
func NewHandler(cfg config.Account, st *store.Store, cl *classify.Classifier, n *notify.Notifier, ts *auth.TokenStore, reg *status.Registry, hub *events.Hub, spamCfg config.Spam, tuneCfg config.ImapTuning) *Handler {
	return &Handler{cfg: cfg, store: st, class: cl, notifier: n, tokens: ts, status: reg, hub: hub, wake: make(chan struct{}, 1), spamCfg: spamCfg, tune: tuneCfg}
}

// publishStatus 广播账号在线状态变更（前端实时刷新统计/状态点）
func (h *Handler) publishStatus(online bool, errMsg string) {
	if h.hub == nil {
		return
	}
	h.hub.Publish(events.Event{Type: "account_status", Data: map[string]interface{}{
		"account": h.cfg.Name,
		"online":  online,
		"error":   errMsg,
	}})
}

// Run 启动账号监听循环：连接 → 增量同步 → IDLE → 断线重连（指数退避，成功后重置）；ctx 取消时退出
func (h *Handler) Run(ctx context.Context) {
	backoff := time.Duration(h.tune.ReconnectInterval) * time.Second
	maxBackoff := time.Duration(h.tune.ReconnectMaxBackoff) * time.Second
	for {
		select {
		case <-ctx.Done():
			h.status.SetOffline(h.cfg.Name, "已停止")
			h.publishStatus(false, "已停止")
			return
		default:
		}

		h.everConnected = false
		if err := h.runOnce(ctx); err != nil {
			h.status.SetOffline(h.cfg.Name, err.Error())
			h.publishStatus(false, err.Error())
			log.Printf("[%s] 连接异常: %v，%v 后重连", h.cfg.Name, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if h.everConnected {
				// 曾成功连接（如睡醒后死连接、服务器踢登录）：退避归零，10s 级快速恢复
				backoff = time.Duration(h.tune.ReconnectInterval) * time.Second
			} else {
				// 从未连上（如未授权）：指数退避，防高频空连
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		} else {
			// 连接成功过（runOnce 正常退出通常是 IDLE 被打断），重置退避
			backoff = time.Duration(h.tune.ReconnectInterval) * time.Second
		}
	}
}

func (h *Handler) runOnce(ctx context.Context) error {
	c, err := h.connect()
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	h.everConnected = true
	h.status.SetOnline(h.cfg.Name)
	h.publishStatus(true, "")
	defer c.Logout()

	// 选择 INBOX（syncFolder 会切换邮箱，idleLoop 前需确保选中 INBOX）
	if _, err := c.Select("INBOX", false); err != nil {
		return fmt.Errorf("选择 INBOX 失败: %w", err)
	}

	// 增量同步 INBOX
	if err := h.syncFolder(c, "INBOX"); err != nil {
		log.Printf("[%s] INBOX 同步失败: %v", h.cfg.Name, err)
	}

	// IDLE 监听新邮件
	log.Printf("[%s] 进入 IDLE 监听", h.cfg.Name)
	return h.idleLoop(ctx, c)
}

func (h *Handler) connect() (*client.Client, error) {
	addr := fmt.Sprintf("%s:%d", h.cfg.IMAP.Host, h.cfg.IMAP.Port)

	c, err := client.DialTLS(addr, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, err
	}

	switch h.cfg.Auth.Type {
	case "app_password":
		if err := c.Login(h.cfg.Auth.Username, h.cfg.Auth.Password); err != nil {
			c.Logout()
			return nil, fmt.Errorf("登录失败: %w", err)
		}
	case "oauth2":
		token, err := auth.AccessToken(h.cfg, h.tokens)
		if err != nil {
			c.Logout()
			return nil, fmt.Errorf("获取 access token 失败: %w", err)
		}
		if err := c.Authenticate(auth.NewXOAuth2SASL(h.cfg.Auth.Username, token)); err != nil {
			c.Logout()
			return nil, fmt.Errorf("XOAUTH2 登录失败: %w", err)
		}
	default:
		c.Logout()
		return nil, fmt.Errorf("不支持的认证类型: %s", h.cfg.Auth.Type)
	}

	return c, nil
}

// idleLoop IDLE 监听：服务端推送新邮件（EXISTS 等）时立即结束 IDLE 并增量同步；
// 无推送时每 idle_timeout 秒兜底重启一次 IDLE（应对部分服务器不推送/断连检测）；
// IDLE 期间不能执行其它命令，利用兜底重启间隙轮询垃圾箱；
// 连续失败 idle_fail_threshold 次返回错误 → Run 层指数退避重连（防半死连接风暴）
func (h *Handler) idleLoop(ctx context.Context, c *client.Client) error {
	updates := make(chan client.Update, 16)
	c.Updates = updates
	go func() {
		// ctx 退出：断线重连时旧 channel 不会 close，不加退出条件会泄漏 goroutine
		for {
			select {
			case upd, ok := <-updates:
				if !ok {
					return
				}
				switch upd.(type) {
				case *client.MailboxUpdate, *client.StatusUpdate, *client.MessageUpdate:
					select {
					case h.wake <- struct{}{}:
					default:
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// 连续失败计数：达到 idle_fail_threshold 即退出回 Run 层指数退避重连；
	// 失败日志仅首次详细打（防半死连接紧密循环日志风暴——曾致单日 26GB）
	idleFails := 0
	var lastFailErr error

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// NOOP 心跳：进 IDLE 前探活（实验：验证 Exchange 是否仅认 NOOP 才重置
		// auto-logout 计时器——微软文档称 Outlook 无论 IDLE 与否均发 NOOP 重置；
		// 若 65 分钟被踢消失=缺心跳实锤，照踢=连接年龄硬时限）
		noopErr := c.Noop()

		stop := make(chan struct{})
		stopped := make(chan struct{})
		// stop 只能关闭一次：goroutine 收到超时/唤醒/取消时关闭 → Idle 内部监听 stop 发
		// DONE 中断返回；主线程 Idle 返回后再兜底关（原实现 goroutine 仅退出自己不关
		// stop，已进 IDLE 的连接对 cancel/wake/timeout 全不响应，半死卡住）
		stopOnce := sync.Once{}
		stopFn := func() { stopOnce.Do(func() { close(stop) }) }
		go func() {
			defer close(stopped)
			t := time.NewTimer(time.Duration(h.tune.IdleTimeout) * time.Second)
			defer t.Stop()
			select {
			case <-t.C:
			case <-h.wake:
			case <-ctx.Done():
			case <-stop:
			}
			stopFn()
		}()

		err := c.Idle(stop, nil)
		stopFn()
		<-stopped

		if ctx.Err() != nil {
			return nil
		}

		failed := false
		if noopErr != nil {
			failed = true
			lastFailErr = noopErr
		}
		if err != nil && !strings.Contains(err.Error(), "imap: connection closed") {
			failed = true
			lastFailErr = err
		}

		// 同步 INBOX
		if serr := h.syncFolder(c, "INBOX"); serr != nil {
			failed = true
			lastFailErr = serr
		}

		// 轮询垃圾箱（间隔内直接跳过）
		h.pollSpamIfNeeded(c)

		// 确保选回 INBOX 以便下次 IDLE
		if _, serr := c.Select("INBOX", false); serr != nil {
			failed = true
			lastFailErr = serr
		}

		if !failed {
			idleFails = 0
			continue
		}
		if idleFails == 0 {
			log.Printf("[%s] IDLE 循环出错（连续失败开始，后续同类错误静默）: %v", h.cfg.Name, lastFailErr)
		}
		idleFails++
		if idleFails >= h.tune.IdleFailThreshold {
			return fmt.Errorf("IDLE 循环连续失败 %d 次，最后错误: %w", idleFails, lastFailErr)
		}
	}
}

// process 处理单封邮件：分类 → 入库 → 告警；folder 为来源文件夹（INBOX/垃圾箱）

// extractBody 从消息中提取正文，返回（纯文本, 净化后HTML）；
// 有 text/plain 用之，有 text/html 则同时提取净化后 HTML 供前端渲染；
// maxBytes 为正文截断上限（config imap.max_body_bytes）
func extractBody(msg *imap.Message, maxBytes int) (plain, html string) {
	if msg == nil {
		return "", ""
	}
	section := &imap.BodySectionName{}
	lr := msg.GetBody(section)
	if lr == nil {
		return "", ""
	}
	raw, err := io.ReadAll(io.LimitReader(lr, int64(maxBytes)+1))
	if err != nil {
		return "", ""
	}
	if len(raw) > maxBytes {
		raw = raw[:maxBytes]
	}
	plain, htmlRaw := extractMIMEBody(string(raw), maxBytes)
	if htmlRaw != "" {
		html = sanitizeHTML(htmlRaw)
	}
	return plain, html
}

// extractMIMEBody 解析原始邮件，返回（纯文本, 原始HTML）；
// 递归处理嵌套 multipart，text/plain 优先作为纯文本；仅有 HTML 时用标签剥离生成纯文本
func extractMIMEBody(raw string, maxBytes int) (plain, html string) {
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return htmlStripTags(raw), ""
	}
	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return readDecoded(msg.Body, msg.Header.Get("Content-Transfer-Encoding")), ""
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return readDecoded(msg.Body, msg.Header.Get("Content-Transfer-Encoding")), ""
		}
		mr := multipart.NewReader(msg.Body, boundary)
		var plainOut, htmlOut string
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			pct := part.Header.Get("Content-Type")
			if pct == "" {
				pct = "text/plain"
			}
			pmt, _, err := mime.ParseMediaType(pct)
			if err != nil {
				continue
			}
			// 递归嵌套 multipart（如 multipart/related 内套 multipart/alternative）
			if strings.HasPrefix(pmt, "multipart/") {
				partData, _ := io.ReadAll(io.LimitReader(part, int64(maxBytes)))
				np, nh := extractMIMEBody(string(partData), maxBytes)
				if plainOut == "" && np != "" {
					plainOut = np
				}
				if htmlOut == "" && nh != "" {
					htmlOut = nh
				}
				continue
			}
			body := readDecoded(part, part.Header.Get("Content-Transfer-Encoding"))
			if strings.HasPrefix(pmt, "text/plain") && plainOut == "" && strings.TrimSpace(body) != "" {
				plainOut = strings.TrimSpace(body)
			}
			if strings.HasPrefix(pmt, "text/html") && htmlOut == "" && strings.TrimSpace(body) != "" {
				htmlOut = strings.TrimSpace(body)
			}
		}
		// 无纯文本时用 HTML 剥标签兜底
		if plainOut == "" && htmlOut != "" {
			plainOut = htmlStripTags(htmlOut)
		}
		return plainOut, htmlOut
	}

	body := readDecoded(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
	if strings.HasPrefix(mediaType, "text/html") {
		return htmlStripTags(body), strings.TrimSpace(body)
	}
	return strings.TrimSpace(body), ""
}

// sanitizeHTML 净化邮件 HTML：移除脚本/样式/事件属性/javascript 协议，保留排版标签
func sanitizeHTML(s string) string {
	// 移除 script/style 整块
	for _, tag := range []string{"script", "style", "iframe", "object", "embed", "link", "meta"} {
		s = removeHTMLBlock(s, tag)
	}
	// 移除所有事件属性 on*=
	s = stripAttr(s, `on`)
	// 移除 javascript: 协议
	s = strings.NewReplacer(
		"javascript:", "",
		"JavaScript:", "",
		"JAVASCRIPT:", "",
	).Replace(s)
	return s
}

// removeHTMLBlock 移除 <tag ...>...</tag> 整块（含自闭合）
func removeHTMLBlock(s, tag string) string {
	lower := strings.ToLower(s)
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		// 找 <tag（不区分大小写）
		idx := strings.Index(lower[i:], "<"+tag)
		if idx < 0 {
			out.WriteString(s[i:])
			break
		}
		idx += i
		// 确认后随空格或 >
		after := idx + 1 + len(tag)
		if after < len(s) && s[after] != '>' && s[after] != ' ' && s[after] != '/' && s[after] != '\t' && s[after] != '\n' {
			out.WriteString(s[i : idx+1])
			i = idx + 1
			continue
		}
		out.WriteString(s[i:idx])
		// 找 >
		gt := strings.Index(s[idx:], ">")
		if gt < 0 {
			break
		}
		openEnd := idx + gt + 1
		// 自闭合检查
		if gt >= 1 && s[idx+gt-1] == '/' {
			i = openEnd
			continue
		}
		// 找 </tag>（复用 lower，避免每次对剩余部分全量 ToLower 造成 O(n²) 分配）
		closeTag := "</" + tag + ">"
		ci := strings.Index(lower[openEnd:], closeTag)
		if ci < 0 {
			i = openEnd
			continue
		}
		i = openEnd + ci + len(closeTag)
	}
	return out.String()
}

// stripAttr 移除 HTML 标签内以 prefix 开头的属性（如 on* 事件属性）
func stripAttr(s, prefix string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		lt := strings.Index(s[i:], "<")
		if lt < 0 {
			out.WriteString(s[i:])
			break
		}
		lt += i
		gt := strings.Index(s[lt:], ">")
		if gt < 0 {
			out.WriteString(s[i:])
			break
		}
		gt += lt
		tag := s[lt : gt+1]
		out.WriteString(s[i:lt])
		out.WriteString(sanitizeTagAttrs(tag, prefix))
		i = gt + 1
	}
	return out.String()
}

// sanitizeTagAttrs 处理单个标签字符串，移除指定前缀的属性（按属性名判断）
func sanitizeTagAttrs(tag, prefix string) string {
	lt := strings.Index(tag, "<")
	gt := strings.LastIndex(tag, ">")
	if lt < 0 || gt <= lt {
		return tag
	}
	name := tag[lt+1 : gt]
	selfClose := ""
	if strings.HasSuffix(name, "/") {
		name = strings.TrimSuffix(name, "/")
		selfClose = "/"
	}
	// 拆分：标签名 + 属性
	sp := strings.IndexAny(name, " \t\n")
	if sp < 0 {
		return tag
	}
	tagName, attrs := name[:sp], name[sp:]
	fields := splitAttrs(attrs)
	var kept []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		// 属性名 = 第一个 = 之前的部分（无 = 则整段）
		attrName := f
		if eq := strings.Index(f, "="); eq >= 0 {
			attrName = f[:eq]
		}
		if strings.HasPrefix(strings.ToLower(attrName), strings.ToLower(prefix)) {
			continue
		}
		kept = append(kept, f)
	}
	result := "<" + tagName
	if len(kept) > 0 {
		result += " " + strings.Join(kept, " ")
	}
	return result + selfClose + ">"
}

// splitAttrs 按空白切属性（保留引号内空白）
func splitAttrs(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			cur.WriteByte(c)
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			cur.WriteByte(c)
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// readDecoded 读取并按 Content-Transfer-Encoding 解码
func readDecoded(r io.Reader, encoding string) string {
	data, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(string(data))
		if err != nil {
			// 容错：剔除空白后重试
			cleaned := strings.Map(func(r rune) rune {
				if r == '\r' || r == '\n' || r == ' ' {
					return -1
				}
				return r
			}, string(data))
			if decoded, err = base64.StdEncoding.DecodeString(cleaned); err != nil {
				return string(data)
			}
		}
		return string(decoded)
	case "quoted-printable":
		// 注意：r 已被上方 ReadAll 耗尽，必须用 data 解码
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err != nil {
			return string(data)
		}
		return string(decoded)
	default:
		return string(data)
	}
}

// htmlStripTags 极简 HTML 转纯文本（去标签、解实体、块级标签转行）
func htmlStripTags(s string) string {
	// 先删 script/style/head 等整块（CSS/脚本内容不是正文）
	for _, tag := range []string{"script", "style", "head", "noscript", "iframe", "object", "embed"} {
		s = removeHTMLBlock(s, tag)
	}
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '<' {
			inTag = true
			continue
		}
		if c == '>' {
			inTag = false
			continue
		}
		if inTag {
			continue
		}
		b.WriteByte(c)
	}
	text := b.String()
	// 常见 HTML 实体
	text = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&apos;", "'",
	).Replace(text)
	// 块级标签换行补偿：由于去标签丢了结构，对 br/div/p 残留位置补不了，按空行合并
	lines := strings.Split(text, "\n")
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func (h *Handler) process(msg *imap.Message, folder string) {
	env := msg.Envelope
	from := ""
	if env != nil && len(env.From) > 0 {
		from = env.From[0].Address()
	}
	subject := ""
	if env != nil {
		subject = env.Subject
	}

	plain, html := extractBody(msg, h.tune.MaxBodyBytes)
	result := h.class.Classify(subject)

	e := &store.Email{
		Account:   h.cfg.Name,
		UID:       msg.Uid,
		From:      from,
		Subject:   subject,
		Category:  result.Category,
		RuleName:  result.RuleName,
		Folder:    folder,
		Body:      plain,
		HTMLBody:  html,
		Timestamp: env.Date,
	}

	_, isNew, err := h.store.SaveEmail(e)
	if err != nil {
		log.Printf("[%s] 入库失败: %v", h.cfg.Name, err)
		return
	}

	if !isNew {
		return
	}

	log.Printf("[%s] 新邮件 [%s] %s", h.cfg.Name, result.Category, subject)

	if h.accum == nil {
		// 兜底：非同步流程（理论上不会发生）直接单发
		if h.hub != nil {
			h.hub.Publish(events.Event{Type: "new_email", Data: map[string]interface{}{
				"account": h.cfg.Name, "category": result.Category,
				"subject": subject, "from": from,
			}})
		}
		if result.Alert {
			h.notifier.Alert(h.cfg.Name, result.Category, subject)
		}
		return
	}

	h.accum.newEmails++
	if h.accum.first == nil {
		h.accum.first = &pendingAlert{category: result.Category, subject: subject, from: from}
	}
	if result.Alert {
		h.accum.alerts = append(h.accum.alerts, pendingAlert{category: result.Category, subject: subject})
	}
}

// flushSync 同步收尾：汇总本批新增，发一条聚合通知（单封实时保持逐条体验）
func (h *Handler) flushSync() {
	acc := h.accum
	h.accum = nil
	if acc == nil || acc.newEmails == 0 {
		return
	}

	alertCount := len(acc.alerts)

	// Windows Toast：0 告警不弹；1 封逐条；多封汇总一条
	if alertCount == 1 {
		a := acc.alerts[0]
		h.notifier.Alert(h.cfg.Name, a.category, a.subject)
	} else if alertCount > 1 {
		cats := map[string]int{}
		subjects := make([]string, 0, alertCount)
		for _, a := range acc.alerts {
			cats[a.category]++
			subjects = append(subjects, a.subject)
		}
		h.notifier.AlertSummary(h.cfg.Name, alertCount, cats, subjects)
	}

	if h.hub == nil {
		return
	}
	if acc.newEmails == 1 && acc.first != nil {
		if alertCount == 1 {
			// 单封告警：只发 new_alert（前端 toast + 横幅一次到位，避免 new_email+new_alert 双弹）
			h.hub.Publish(events.Event{Type: "new_alert", Data: map[string]interface{}{
				"account": h.cfg.Name, "category": acc.alerts[0].category,
				"subject": acc.alerts[0].subject, "from": acc.first.from,
			}})
		} else {
			// 单封非告警：new_email 逐条事件
			h.hub.Publish(events.Event{Type: "new_email", Data: map[string]interface{}{
				"account": h.cfg.Name, "category": acc.first.category,
				"subject": acc.first.subject, "from": acc.first.from,
			}})
		}
		return
	}

	// 多封：一条聚合事件（前端一条 toast + 统一刷新）
	cats := map[string]int{}
	subjects := make([]string, 0, alertCount)
	for _, a := range acc.alerts {
		cats[a.category]++
		if len(subjects) < 3 {
			subjects = append(subjects, a.subject)
		}
	}
	h.hub.Publish(events.Event{Type: "sync_summary", Data: map[string]interface{}{
		"account":     h.cfg.Name,
		"count":       acc.newEmails,
		"alert_count": alertCount,
		"categories":  cats,
		"subjects":    subjects,
	}})
}
