package classify

import (
	"strings"
	"sync"

	"mercado-mail/internal/config"
)

// Result 分类结果
type Result struct {
	Category string
	RuleName string
	Alert    bool
}

// Classifier 基于关键词的分类器（支持热更新）
type Classifier struct {
	mu    sync.RWMutex
	rules []config.Rule
}

// New 创建分类器，规则按配置顺序匹配
func New(rules []config.Rule) *Classifier {
	return &Classifier{rules: rules}
}

// UpdateRules 热更新规则（Web 编辑后调用）
func (c *Classifier) UpdateRules(rules []config.Rule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rules = rules
}

// Classify 对主题做中英混合关键词匹配（不区分大小写），首个命中生效；
// 仅用标题分类（正文含 CSS/HTML 噪音，实证不可靠），未命中返回 other
func (c *Classifier) Classify(subject string) Result {
	c.mu.RLock()
	rules := c.rules
	c.mu.RUnlock()

	text := strings.ToLower(subject)
	for _, rule := range rules {
		for _, kw := range rule.Keywords {
			if kw == "" {
				continue
			}
			if strings.Contains(text, strings.ToLower(kw)) {
				return Result{
					Category: rule.Category,
					RuleName: rule.Name,
					Alert:    rule.Alert,
				}
			}
		}
	}
	return Result{Category: "other", Alert: false}
}
