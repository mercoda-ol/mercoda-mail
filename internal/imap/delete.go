package imapclient

import (
	"crypto/tls"
	"fmt"
	"log"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"mercado-mail/internal/auth"
	"mercado-mail/internal/config"
)

// trashNameCandidates 回收站文件夹名猜测候选（小写）
var trashNameCandidates = []string{
	"deleted",
	"deleted items",
	"deleted messages",
	"trash",
	"已删除邮件",
	"回收站",
	"[gmail]/trash",
	"[gmail]/已删除邮件",
}

// detectTrashFolder 探测回收站：优先 SPECIAL-USE \Trash，其次名字猜测
func detectTrashFolder(c *client.Client) string {
	mailboxes := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", "*", mailboxes)
	}()

	var names []string
	trashBySpecialUse := ""
	for m := range mailboxes {
		names = append(names, m.Name)
		for _, attr := range m.Attributes {
			if strings.EqualFold(attr, `\Trash`) && trashBySpecialUse == "" {
				trashBySpecialUse = m.Name
			}
		}
	}
	if err := <-done; err != nil {
		return ""
	}
	if trashBySpecialUse != "" {
		return trashBySpecialUse
	}
	for _, cand := range trashNameCandidates {
		for _, n := range names {
			lower := strings.ToLower(n)
			if lower == cand || strings.HasSuffix(lower, "/"+cand) {
				return n
			}
		}
	}
	return ""
}

// DeleteRemoteEmail 临时建连删除远端邮件：优先 UID MOVE 到回收站；
// 不支持 MOVE 则 STORE \Deleted + UID EXPUNGE（精确删单封）
func DeleteRemoteEmail(cfg config.Account, tokens *auth.TokenStore, folder string, uid uint32) error {
	addr := fmt.Sprintf("%s:%d", cfg.IMAP.Host, cfg.IMAP.Port)
	c, err := client.DialTLS(addr, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	defer c.Logout()

	switch cfg.Auth.Type {
	case "app_password":
		if err := c.Login(cfg.Auth.Username, cfg.Auth.Password); err != nil {
			return fmt.Errorf("登录失败: %w", err)
		}
	case "oauth2":
		token, err := auth.AccessToken(cfg, tokens)
		if err != nil {
			return fmt.Errorf("获取 access token 失败: %w", err)
		}
		if err := c.Authenticate(auth.NewXOAuth2SASL(cfg.Auth.Username, token)); err != nil {
			return fmt.Errorf("XOAUTH2 登录失败: %w", err)
		}
	default:
		return fmt.Errorf("不支持的认证类型: %s", cfg.Auth.Type)
	}

	if _, err := c.Select(folder, false); err != nil {
		return fmt.Errorf("选择文件夹 %s 失败: %w", folder, err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	// 方式1：UID MOVE 到回收站（Outlook/Gmail 均支持，RFC 6851）
	trash := detectTrashFolder(c)
	if trash != "" {
		if err := c.UidMove(seqSet, trash); err == nil {
			log.Printf("[delete] %s: UID %d 已 MOVE 到 %s", cfg.Name, uid, trash)
			return nil
		}
		// MOVE 失败（服务器不支持）退化到 STORE+EXPUNGE
		log.Printf("[delete] %s: MOVE 失败(%v)，降级 STORE \\Deleted", cfg.Name, err)
	}

	// 方式2：UID STORE +\Deleted + EXPUNGE
	// 注意：EXPUNGE 为邮箱级（删所有 \Deleted），本连接仅标记了这一封；
	// Outlook/Gmail 支持 MOVE，此路径极少触发
	if err := c.UidStore(seqSet, imap.AddFlags, []interface{}{imap.DeletedFlag}, nil); err != nil {
		return fmt.Errorf("标记删除失败: %w", err)
	}
	if err := c.Expunge(nil); err != nil {
		return fmt.Errorf("彻底删除失败: %w", err)
	}
	log.Printf("[delete] %s: UID %d 已 STORE\\Deleted+EXPUNGE", cfg.Name, uid)
	return nil
}

// DeleteRemoteEmails 批量删除远端邮件：单连接登录，按 UID 分组先整批 MOVE，
// 整批失败降级逐 UID MOVE，再失败逐 UID STORE+\Deleted+EXPUNGE。
// 返回 deleted（成功 UID）与 failed（UID→错误）。
func DeleteRemoteEmails(cfg config.Account, tokens *auth.TokenStore, folder string, uids []uint32) ([]uint32, map[uint32]error) {
	deleted := make([]uint32, 0, len(uids))
	failed := make(map[uint32]error)
	if len(uids) == 0 {
		return deleted, failed
	}

	addr := fmt.Sprintf("%s:%d", cfg.IMAP.Host, cfg.IMAP.Port)
	c, err := client.DialTLS(addr, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		for _, u := range uids {
			failed[u] = fmt.Errorf("连接失败: %w", err)
		}
		return deleted, failed
	}
	defer c.Logout()

	switch cfg.Auth.Type {
	case "app_password":
		err = c.Login(cfg.Auth.Username, cfg.Auth.Password)
	case "oauth2":
		var token string
		token, err = auth.AccessToken(cfg, tokens)
		if err == nil {
			err = c.Authenticate(auth.NewXOAuth2SASL(cfg.Auth.Username, token))
		}
	default:
		err = fmt.Errorf("不支持的认证类型: %s", cfg.Auth.Type)
	}
	if err != nil {
		for _, u := range uids {
			failed[u] = fmt.Errorf("登录失败: %w", err)
		}
		return deleted, failed
	}

	if _, err := c.Select(folder, false); err != nil {
		for _, u := range uids {
			failed[u] = fmt.Errorf("选择文件夹 %s 失败: %w", folder, err)
		}
		return deleted, failed
	}

	trash := detectTrashFolder(c)

	moveOne := func(uid uint32) error {
		seq := new(imap.SeqSet)
		seq.AddNum(uid)
		if trash != "" {
			if err := c.UidMove(seq, trash); err == nil {
				return nil
			} else {
				log.Printf("[delete] %s: 批量 UID %d MOVE 失败(%v)，降级 STORE \\Deleted", cfg.Name, uid, err)
			}
		}
		if err := c.UidStore(seq, imap.AddFlags, []interface{}{imap.DeletedFlag}, nil); err != nil {
			return fmt.Errorf("标记删除失败: %w", err)
		}
		if err := c.Expunge(nil); err != nil {
			return fmt.Errorf("彻底删除失败: %w", err)
		}
		return nil
	}

	// 先尝试整批 MOVE（服务器无逐 UID 状态，整批失败再降级）
	if trash != "" {
		batch := new(imap.SeqSet)
		batch.AddNum(uids...)
		if err := c.UidMove(batch, trash); err == nil {
			log.Printf("[delete] %s: 批量 %d 封 UID MOVE 到 %s 成功", cfg.Name, len(uids), trash)
			deleted = append(deleted, uids...)
			return deleted, failed
		}
		log.Printf("[delete] %s: 整批 MOVE 失败(%v)，降级逐 UID", cfg.Name, err)
	}

	for _, uid := range uids {
		if err := moveOne(uid); err != nil {
			failed[uid] = err
		} else {
			deleted = append(deleted, uid)
		}
	}
	if len(failed) > 0 {
		log.Printf("[delete] %s: 批量删除完成 %d 成功 / %d 失败", cfg.Name, len(deleted), len(failed))
	} else {
		log.Printf("[delete] %s: 批量删除 %d 封全部成功", cfg.Name, len(deleted))
	}
	return deleted, failed
}
