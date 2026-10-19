package imapclient

import (
	"log"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"mercado-mail/internal/events"
)

// spamNameCandidates 垃圾箱文件夹名猜测候选（小写比较）
var spamNameCandidates = []string{
	"junk",
	"junk email",
	"junk e-mail",
	"spam",
	"垃圾邮件",
	"[gmail]/垃圾邮件",
	"[gmail]/spam",
}

// detectSpamFolder 探测垃圾箱文件夹：
// 优先 SPECIAL-USE \Junk 标志，其次名字猜测；配置指定则直接用
func (h *Handler) detectSpamFolder(c *client.Client) string {
	// 配置手动指定优先
	if h.spamCfg.Folder != "" {
		return h.spamCfg.Folder
	}

	mailboxes := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", "*", mailboxes)
	}()

	var names []string
	var junkBySpecialUse string
	for m := range mailboxes {
		names = append(names, m.Name)
		for _, attr := range m.Attributes {
			if strings.EqualFold(attr, `\Junk`) && junkBySpecialUse == "" {
				junkBySpecialUse = m.Name
			}
		}
	}
	if err := <-done; err != nil {
		log.Printf("[%s] 探测垃圾箱失败: %v", h.cfg.Name, err)
		return ""
	}

	if junkBySpecialUse != "" {
		log.Printf("[%s] 垃圾箱(SPECIAL-USE): %s", h.cfg.Name, junkBySpecialUse)
		return junkBySpecialUse
	}

	// 名字猜测：末段匹配
	for _, cand := range spamNameCandidates {
		for _, n := range names {
			lower := strings.ToLower(n)
			if lower == cand || strings.HasSuffix(lower, "/"+cand) {
				log.Printf("[%s] 垃圾箱(名称匹配): %s", h.cfg.Name, n)
				return n
			}
		}
	}

	log.Printf("[%s] 未探测到垃圾箱文件夹（可用配置 spam.folder 指定），已跳过", h.cfg.Name)
	return ""
}

// pollSpamIfNeeded 到达轮询间隔时同步垃圾箱（与收件箱处理一致：分类→入库→告警）
func (h *Handler) pollSpamIfNeeded(c *client.Client) {
	if !h.spamCfg.Enabled {
		return
	}
	// 探测缓存
	if h.spamFolder == "" {
		h.spamFolder = h.detectSpamFolder(c)
		if h.spamFolder == "" {
			return // 未探测到，不再重复探测
		}
	}

	interval := h.spamCfg.PollSeconds // 默认值由 config.Load 兜底
	if !h.lastSpamPoll.IsZero() && time.Since(h.lastSpamPoll) < time.Duration(interval)*time.Second {
		return
	}
	h.lastSpamPoll = time.Now()

	if err := h.syncFolder(c, h.spamFolder); err != nil {
		log.Printf("[%s] 垃圾箱轮询失败: %v", h.cfg.Name, err)
	}
}

// syncFolder 按 UID 增量同步指定文件夹（首拉见 syncFolderBatched；水位推进更新）；
// 开头置位 accum，结尾 flushSync 聚合通知
func (h *Handler) syncFolder(c *client.Client, folder string) error {
	h.accum = &syncAccum{}
	defer h.flushSync()

	// 选中目标文件夹获取状态（SELECT 会切换当前邮箱，后续 IDLE 前需切回 INBOX）
	mbox, err := c.Select(folder, true)
	if err != nil {
		return err
	}

	lastUID, err := h.store.GetLastUID(h.cfg.Name, folder)
	if err != nil {
		return err
	}

	if lastUID == 0 && mbox.UidNext > 1 {
		// 首次同步：分批拉全量历史
		return h.syncFolderBatched(c, folder, mbox.UidNext)
	}
	if lastUID == 0 {
		return nil // 空文件夹
	}

	// 增量：lastUID+1 到最后
	seqSet := new(imap.SeqSet)
	seqSet.AddRange(lastUID+1, 0)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, section.FetchItem()}
	messages := make(chan *imap.Message, 32)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqSet, items, messages)
	}()

	var maxUID uint32
	for msg := range messages {
		if msg.Envelope == nil {
			continue
		}
		if msg.Uid > maxUID {
			maxUID = msg.Uid
		}
		h.process(msg, folder)
	}

	if err := <-done; err != nil {
		return err
	}

	if maxUID > 0 {
		if err := h.store.UpdateLastUID(h.cfg.Name, folder, maxUID); err != nil {
			return err
		}
		h.status.TouchSync(h.cfg.Name)
	}
	return nil
}

// syncFolderBatched 首次同步：从 UID 1 起每批 batch_size 个 UID 循环拉取直到追上 uidNext，
// 水位随批次推进（断线重连不重拉已完成部分）；期间广播 sync_progress，邮件静默入库
func (h *Handler) syncFolderBatched(c *client.Client, folder string, uidNext uint32) error {
	batchSize := uint32(h.tune.BatchSize) // 默认值由 config.Load 兜底
	total := int(uidNext) - 1
	if total <= 0 {
		return nil
	}

	log.Printf("[%s] 首次全量同步 %s：共约 %d 个 UID，分批拉取", h.cfg.Name, folder, total)

	fetched := 0
	for start := uint32(1); start < uidNext; start += batchSize {
		end := start + batchSize - 1
		if end >= uidNext {
			end = uidNext - 1
		}
		seqSet := new(imap.SeqSet)
		seqSet.AddRange(start, end)

		section := &imap.BodySectionName{}
		items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, section.FetchItem()}
		messages := make(chan *imap.Message, 32)
		done := make(chan error, 1)
		go func() {
			done <- c.UidFetch(seqSet, items, messages)
		}()

		var maxUID uint32
		for msg := range messages {
			if msg.Envelope == nil {
				continue
			}
			if msg.Uid > maxUID {
				maxUID = msg.Uid
			}
			h.process(msg, folder)
			fetched++
		}
		if err := <-done; err != nil {
			log.Printf("[%s] 全量同步批次 %d-%d 失败: %v", h.cfg.Name, start, end, err)
			// 水位推进到本批已处理的最大 UID，重连续拉
			if maxUID > 0 {
				h.store.UpdateLastUID(h.cfg.Name, folder, maxUID)
			}
			return err
		}

		if maxUID > 0 {
			if err := h.store.UpdateLastUID(h.cfg.Name, folder, maxUID); err != nil {
				return err
			}
		}
		h.status.TouchSync(h.cfg.Name)

		// 广播进度
		if h.hub != nil {
			h.hub.Publish(events.Event{Type: "sync_progress", Data: map[string]interface{}{
				"account": h.cfg.Name,
				"folder":  folder,
				"fetched": fetched,
				"total":   total,
				"percent": int(float64(end) / float64(uidNext-1) * 100),
			}})
		}
	}

	if h.hub != nil {
		h.hub.Publish(events.Event{Type: "sync_done", Data: map[string]interface{}{
			"account": h.cfg.Name,
			"folder":  folder,
			"fetched": fetched,
		}})
	}
	log.Printf("[%s] 首次全量同步 %s 完成：处理 %d 个 UID", h.cfg.Name, folder, fetched)
	return nil
}
