package classify

import (
	"testing"

	"mercado-mail/internal/config"
)

// testRules 与 config.yaml 规则保持一致（优先级: 异常订单 → 订单 → 账号安全 → 客服支持 → 商品优化）
func testRules() []config.Rule {
	return []config.Rule{
		{Name: "异常订单", Category: "abnormal_order", Alert: true,
			Keywords: []string{"order cancelled", "cancelled order", "order canceled", "canceled order", "cancellation request", "order delay", "delayed shipment", "shipping delay", "late shipment", "late delivery", "shipping problem", "delivery problem", "not delivered", "delay", "delayed", "延迟", "物流", "物流延迟", "物流异常", "延迟发货", "取消订单", "订单取消", "订单延迟"}},
		{Name: "订单", Category: "order", Alert: true,
			Keywords: []string{"you sold", "sold", "order", "pedido", "compra", "purchase", "订单", "tracking", "shipment", "退款", "refund"}},
		{Name: "账号安全", Category: "account_security", Alert: true,
			Keywords: []string{"security alert", "new browser", "异常登录", "unusual", "suspicious", "password", "密码", "verify", "验证", "suspended", "suspension", "封号", "停用", "停权", "侵权", "infringement", "dmca", "投诉", "complaint", "下架", "政策违规", "policy violation", "hazardous"}},
		{Name: "客服支持", Category: "customer_support", Alert: true,
			Keywords: []string{"customer support", "serviço ao cliente", "[case:"}},
		{Name: "商品优化", Category: "listing_optimization", Alert: false,
			Keywords: []string{"listing", "listing optimiz", "improve your listing", "boost your listing", "product listing", "listing quality", "listing ideas", "paused", "reactivate", "canceled your", "cancelled your", "deactivate", "was cancelled"}},
	}
}

func TestClassifySubject(t *testing.T) {
	c := New(testRules())

	cases := []struct {
		subject  string
		category string
		alert    bool
	}{
		// 异常订单（最优先，词组更具体）
		{"Delayed shipment: your order will arrive late", "abnormal_order", true},
		{"您的延迟发货报告已准备好", "abnormal_order", true},
		{"Order cancelled by the buyer", "abnormal_order", true},
		// 真实成交 → 订单；买家取消请求 → 异常订单（第21轮裁定二改：取消类归异常，原漏 other）
		{"You sold in Mexico with Mercado Envíos Partnered Carrier!", "order", true},
		{"Cancellation request of Bx. Videogame Gamer Stick M15", "abnormal_order", true},
		// paused/reactivate/canceled your → 商品优化（第20轮裁定：listing 统一进商品优化）
		{"Some of your listings were paused", "listing_optimization", false},
		{"Fix your photos to reactivate your listings", "listing_optimization", false},
		{"We canceled your Ws. Vasos listing", "listing_optimization", false},
		{"We have deactivate your listing with no sales and visits", "listing_optimization", false},
		// 账号安全（侵权/危险品/告警/投诉等严重事项）
		{"We paused your listing for containing hazardous materials", "account_security", true},
		{"Security alert: login from a new  browser ", "account_security", true},
		{"Check your listings to avoid the suspension of your account", "account_security", true},
		{"您的账号已被封号", "account_security", true},
		{"You solved the complaint for Wss. Porta-fotocards Para Kpop", "account_security", true},
		// 客服支持（第21轮新分类，弹系统通知）
		{"Customer support [Case: 489530140]", "customer_support", true},
		{"Serviço ao Cliente [Case: 488134301]", "customer_support", true},
		// 纯 listing（无账号安全词）→ 商品优化（非告警）
		{"Your listing was cancelled", "listing_optimization", false},
		{"10 ideas to improve your product listing", "listing_optimization", false},
		// 中文订单
		{"订单超时未发货提醒", "order", true},
		// 不该命中 → other
		{"恭喜突破10单！你的店铺权益已解锁！", "other", false},
		{"您的独家店铺现已在您销售的市场/国家上线！🚀", "other", false},
		{"连接到 Microsoft 帐户的新应用", "other", false},
		{"Here is your verification code", "other", false},
	}

	for _, tc := range cases {
		got := c.Classify(tc.subject)
		if got.Category != tc.category {
			t.Errorf("Classify(%q) = %q, 期望 %q", tc.subject, got.Category, tc.category)
		}
		if got.Alert != tc.alert {
			t.Errorf("Classify(%q).Alert = %v, 期望 %v", tc.subject, got.Alert, tc.alert)
		}
	}
}

func TestClassifyCaseInsensitive(t *testing.T) {
	c := New(testRules())
	if got := c.Classify("REFUND request"); got.Category != "order" {
		t.Errorf("英文大小写不敏感匹配失败: %q", got.Category)
	}
}

// TestClassifySubjectOnly 分类仅用标题：标题无关键词 → other（正文不参与）
func TestClassifySubjectOnly(t *testing.T) {
	c := New(testRules())
	if got := c.Classify("Fwd: some message"); got.Category != "other" {
		t.Errorf("仅标题分类失败: got %q, 期望 other", got.Category)
	}
}
