package notify

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// soundWhitelist 配置允许的提示音（防注入，对应 balloon.go soundFiles）
var soundWhitelist = map[string]bool{
	"Default": true, "IM": true, "Call": true,
	"Reminder": true, "SMS": true, "Alarm": true,
}

// Notifier 托盘气泡通知 + 内置声音（shell32 气泡 + winmm 音效，见 balloon.go）
type Notifier struct {
	enabled        bool
	sound          string
	categorySounds map[string]string // 按分类覆盖提示音（如 order→Call），未配置用 sound
	onlyCats       []string          // 仅这些分类发系统通知（网页提示不受限）
	minInterval    time.Duration
	mu             sync.Mutex // lastFired 多账号 goroutine 并发访问需加锁
	lastFired      time.Time
}

// New 创建通知器；sound 取值: Default, IM, Call, Reminder, SMS, Alarm（映射系统音效文件）
func New(enabled bool, sound string, minIntervalSec int, categorySounds map[string]string, onlyCats []string) *Notifier {
	if sound == "" {
		sound = "Reminder"
	}
	if minIntervalSec <= 0 {
		minIntervalSec = 5
	}
	return &Notifier{
		enabled:        enabled,
		sound:          sound,
		categorySounds: categorySounds,
		onlyCats:       onlyCats,
		minInterval:    time.Duration(minIntervalSec) * time.Second,
	}
}

// soundFor 按分类取提示音（配置优先，回退默认）
func (n *Notifier) soundFor(category string) string {
	if s, ok := n.categorySounds[category]; ok && soundWhitelist[s] {
		return s
	}
	return n.sound
}

// tryFire 限流检查（拿锁更新 lastFired，返回 false 表示被限流）
func (n *Notifier) tryFire() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if time.Since(n.lastFired) < n.minInterval {
		return false
	}
	n.lastFired = time.Now()
	return true
}

// notifyCategory 该分类是否发系统通知（空配置视为不过滤兜底）
func (n *Notifier) notifyCategory(cat string) bool {
	if len(n.onlyCats) == 0 {
		return true
	}
	for _, c := range n.onlyCats {
		if c == cat {
			return true
		}
	}
	return false
}

// Alert 弹出气泡通知（含提示音，按分类选音），带最小间隔限流；仅 onlyCats 分类发系统通知
func (n *Notifier) Alert(account, category, subject string) {
	if !n.enabled {
		return
	}
	// 白名单过滤在限流之前：非目标分类不占用通知配额
	if !n.notifyCategory(category) {
		return
	}

	if !n.tryFire() {
		log.Printf("[notify] 限流跳过: %s - %s", account, subject)
		return
	}

	title := fmt.Sprintf("Mercado 告警 [%s] %s", account, categoryName(category))
	// 截断过长主题（按 rune 防中文乱码）
	if rs := []rune(subject); len(rs) > 120 {
		subject = string(rs[:120]) + "..."
	}

	if err := showToast(title, subject, n.soundFor(category)); err != nil {
		log.Printf("[notify] Toast 推送失败: %v", err)
	}
}

// AlertSummary 多封告警聚合为一条汇总气泡（categories 分类计数，subjects 样本主题最多取3条）；
// 仅统计 onlyCats 分类，其它类不发系统通知（网页提示不受限）
func (n *Notifier) AlertSummary(account string, count int, categories map[string]int, subjects []string) {
	if !n.enabled || count <= 0 {
		return
	}

	// 只统计要系统通知的分类
	filtered := make(map[string]int)
	total := 0
	for cat, c := range categories {
		if c > 0 && n.notifyCategory(cat) {
			filtered[cat] = c
			total += c
		}
	}
	if total <= 0 {
		return
	}

	if !n.tryFire() {
		log.Printf("[notify] 限流跳过(汇总): %s - %d 封", account, total)
		return
	}

	var parts []string
	for _, cat := range n.onlyCats {
		if c := filtered[cat]; c > 0 {
			parts = append(parts, fmt.Sprintf("%s%d", categoryName(cat), c))
		}
	}
	nSubj := len(subjects)
	if nSubj > 3 {
		nSubj = 3
	}
	sample := strings.Join(subjects[:nSubj], "；")
	rs := []rune(sample)
	if len(rs) > 80 {
		sample = string(rs[:80]) + "…"
	}

	title := fmt.Sprintf("Mercado 告警 [%s] 收到 %d 封", account, total)
	body := strings.Join(parts, " · ")
	if sample != "" {
		body += " ｜ " + sample
	}

	if err := showToast(title, body, n.sound); err != nil {
		log.Printf("[notify] Toast 汇总推送失败: %v", err)
	}
}

func categoryName(cat string) string {
	switch cat {
	case "order":
		return "订单"
	case "abnormal_order":
		return "异常订单"
	case "account_security":
		return "账号安全"
	case "customer_support":
		return "客服支持"
	case "listing_optimization":
		return "商品优化"
	default:
		return cat
	}
}
