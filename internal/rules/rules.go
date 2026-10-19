package rules

import (
	"log"

	"mercado-mail/internal/classify"
	"mercado-mail/internal/config"
	"mercado-mail/internal/store"
)

// Load 加载规则：DB 有则用 DB，否则从 config 导入后加载
func Load(st *store.Store, cl *classify.Classifier, cfgRules []config.Rule) error {
	n, err := st.CountRules()
	if err != nil {
		return err
	}

	if n == 0 {
		log.Printf("[rules] DB 无规则，从 config.yaml 导入 %d 条", len(cfgRules))
		for i, r := range cfgRules {
			id, err := st.InsertRule(&store.Rule{
				Name:     r.Name,
				Category: r.Category,
				Alert:    r.Alert,
				Keywords: r.Keywords,
				Priority: i,
			})
			if err != nil {
				return err
			}
			_ = id
		}
	}

	if err := reload(st, cl); err != nil {
		return err
	}
	// 启动时幂等重分类，保证历史邮件与当前规则一致
	reclassify(st, cl)
	return nil
}

// reload 从 DB 读取规则并热更新到分类器
func reload(st *store.Store, cl *classify.Classifier) error {
	dbRules, err := st.ListRules()
	if err != nil {
		return err
	}
	cfgRules := make([]config.Rule, 0, len(dbRules))
	for _, r := range dbRules {
		cfgRules = append(cfgRules, config.Rule{
			Name:     r.Name,
			Category: r.Category,
			Alert:    r.Alert,
			Keywords: r.Keywords,
		})
	}
	cl.UpdateRules(cfgRules)
	return nil
}

// Save 保存规则到 DB 并热更新分类器，随后重分类全部已有邮件
func Save(st *store.Store, cl *classify.Classifier, r *store.Rule) (int64, error) {
	var id int64
	var err error
	if r.ID == 0 {
		id, err = st.InsertRule(r)
	} else {
		err = st.UpdateRule(r)
		id = r.ID
	}
	if err != nil {
		return 0, err
	}
	if err := reload(st, cl); err != nil {
		return id, err
	}
	reclassify(st, cl)
	return id, nil
}

// Delete 删除规则并热更新分类器，随后重分类全部已有邮件
func Delete(st *store.Store, cl *classify.Classifier, id int64) error {
	if err := st.DeleteRule(id); err != nil {
		return err
	}
	if err := reload(st, cl); err != nil {
		return err
	}
	reclassify(st, cl)
	return nil
}

// reclassify 用当前规则重分类全部已有邮件（规则变更后历史邮件立即归位）
func reclassify(st *store.Store, cl *classify.Classifier) {
	n, err := st.ReclassifyAll(func(subject string) (string, string) {
		res := cl.Classify(subject)
		return res.Category, res.RuleName
	})
	if err != nil {
		log.Printf("[rules] 重分类失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("[rules] 规则变更，重分类 %d 封邮件", n)
	}
}
