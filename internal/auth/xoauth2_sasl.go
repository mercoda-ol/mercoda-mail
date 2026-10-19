package auth

import "fmt"

// xoauth2SASL 实现 go-sasl 的 Client 接口，用于 IMAP Authenticate
type xoauth2SASL struct {
	user  string
	token string
	done  bool
}

// NewXOAuth2SASL 创建 XOAUTH2 SASL 客户端
func NewXOAuth2SASL(user, accessToken string) *xoauth2SASL {
	return &xoauth2SASL{user: user, token: accessToken}
}

func (a *xoauth2SASL) Start() (mech string, ir []byte, err error) {
	return "XOAUTH2", []byte(xoauth2String(a.user, a.token)), nil
}

func (a *xoauth2SASL) Next(challenge []byte) ([]byte, error) {
	if a.done {
		// 第二次 Next：服务器要求发送空响应以获取错误详情
		return nil, fmt.Errorf("XOAUTH2 认证失败: %s", string(challenge))
	}
	a.done = true
	return []byte{}, nil
}
