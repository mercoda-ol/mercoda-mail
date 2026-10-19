package accounts

import (
	"log"

	"mercado-mail/internal/config"
	"mercado-mail/internal/crypt"
	"mercado-mail/internal/store"
)

// Load 加载账号：DB 有则用 DB，否则从 config.yaml 导入（密码加密存储）
func Load(st *store.Store, cfgAccounts []config.Account, keyPath string) ([]config.Account, error) {
	n, err := st.CountAccounts()
	if err != nil {
		return nil, err
	}

	if n == 0 && len(cfgAccounts) > 0 {
		log.Printf("[accounts] DB 无账号，从 config.yaml 导入 %d 个", len(cfgAccounts))
		for _, a := range cfgAccounts {
			enc := ""
			if a.Auth.Password != "" {
				enc, err = crypt.EncryptString(keyPath, a.Auth.Password)
				if err != nil {
					return nil, err
				}
			}
			if _, err := st.InsertAccount(&store.Account{
				Name: a.Name, Email: a.Email, Provider: a.Provider,
				IMAPHost: a.IMAP.Host, IMAPPort: a.IMAP.Port,
				AuthType: a.Auth.Type, AuthUsername: a.Auth.Username,
				AuthPasswordEnc: enc,
			}); err != nil {
				return nil, err
			}
		}
	}

	return fromDB(st, keyPath)
}

// fromDB 从 DB 读取账号并解密为运行时配置
func fromDB(st *store.Store, keyPath string) ([]config.Account, error) {
	dbAccounts, err := st.ListAccounts()
	if err != nil {
		return nil, err
	}
	out := make([]config.Account, 0, len(dbAccounts))
	for _, a := range dbAccounts {
		cfg := config.Account{
			Name: a.Name, Email: a.Email, Provider: a.Provider,
			IMAP: config.IMAP{Host: a.IMAPHost, Port: a.IMAPPort},
			Auth: config.Auth{Type: a.AuthType, Username: a.AuthUsername},
		}
		if a.AuthPasswordEnc != "" {
			plain, err := crypt.DecryptString(keyPath, a.AuthPasswordEnc)
			if err != nil {
				log.Printf("[accounts] 账号 %s 密码解密失败: %v", a.Name, err)
				continue
			}
			cfg.Auth.Password = plain
		}
		out = append(out, cfg)
	}
	return out, nil
}
