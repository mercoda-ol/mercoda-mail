package account

import (
	"context"
	"log"
	"sync"
	"time"

	"mercado-mail/internal/auth"
	"mercado-mail/internal/classify"
	"mercado-mail/internal/config"
	"mercado-mail/internal/events"
	imapclient "mercado-mail/internal/imap"
	"mercado-mail/internal/notify"
	"mercado-mail/internal/status"
	"mercado-mail/internal/store"
)

// Manager 管理所有账号的并发收信（支持动态增删）
type Manager struct {
	cfgs   []config.Account
	st     *store.Store
	cl     *classify.Classifier
	n      *notify.Notifier
	tokens *auth.TokenStore
	Status *status.Registry
	hub    *events.Hub
	spam   config.Spam
	tune   config.ImapTuning

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // key: 账号名
	dones   map[string]chan struct{}      // key: 账号名——旧 worker 退出信号（防状态竞态）
}

// New 创建账号管理器
func New(cfgs []config.Account, st *store.Store, cl *classify.Classifier, n *notify.Notifier, ts *auth.TokenStore, hub *events.Hub, spamCfg config.Spam, tuneCfg config.ImapTuning) *Manager {
	reg := status.NewRegistry()
	for _, c := range cfgs {
		reg.Register(c.Name)
	}
	return &Manager{
		cfgs: cfgs, st: st, cl: cl, n: n, tokens: ts, Status: reg, hub: hub, spam: spamCfg, tune: tuneCfg,
		cancels: map[string]context.CancelFunc{},
		dones:   map[string]chan struct{}{},
	}
}

// Start 为所有账号启动收信 goroutine
func (m *Manager) Start() {
	for _, cfg := range m.cfgs {
		m.startOne(cfg)
	}
}

// startOne 启动单个账号的收信 goroutine（同名先停旧、等旧 worker 真正退出再起新的：
// 防旧 worker 退出时的 SetOffline("已停止") 迟到覆盖新 worker 的在线状态——
// 实证过一次污染：保存账号后状态显示"已停止"但实际新连接已建立）
func (m *Manager) startOne(cfg config.Account) {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if old, ok := m.cancels[cfg.Name]; ok {
		old() // 同名旧 worker 先停
	}
	var oldDone chan struct{}
	if d, ok := m.dones[cfg.Name]; ok {
		oldDone = d
	}
	m.cancels[cfg.Name] = cancel
	m.dones[cfg.Name] = make(chan struct{})
	done := m.dones[cfg.Name]
	m.mu.Unlock()

	// 等旧 worker 退出（Run 返回时 close done），上限 5s 防卡死（正常秒退）
	if oldDone != nil {
		select {
		case <-oldDone:
		case <-time.After(5 * time.Second):
			log.Printf("[%s] 旧收信循环 5s 未退出，继续启动新循环", cfg.Name)
		}
	}

	m.Status.Register(cfg.Name)
	go func() {
		defer close(done)
		log.Printf("[%s] 启动收信 (%s)", cfg.Name, cfg.Email)
		imapclient.NewHandler(cfg, m.st, m.cl, m.n, m.tokens, m.Status, m.hub, m.spam, m.tune).Run(ctx)
	}()
}

// Add 新增/更新账号并启动（同名则替换，防重复追加膨胀）
func (m *Manager) Add(cfg config.Account) {
	m.mu.Lock()
	replaced := false
	for i, c := range m.cfgs {
		if c.Name == cfg.Name {
			m.cfgs[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		m.cfgs = append(m.cfgs, cfg)
	}
	m.mu.Unlock()
	m.startOne(cfg)
}

// Remove 停止并移除账号
func (m *Manager) Remove(name string) {
	m.mu.Lock()
	if cancel, ok := m.cancels[name]; ok {
		cancel()
		delete(m.cancels, name)
	}
	for i, c := range m.cfgs {
		if c.Name == name {
			m.cfgs = append(m.cfgs[:i], m.cfgs[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	log.Printf("[%s] 已停止收信", name)
}
