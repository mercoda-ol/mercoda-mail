package auth

import (
	"fmt"
)

// xoauth2String 构造 XOAUTH2 初始字符串: user=xxx^Aauth=Bearer xxx^A^A
func xoauth2String(user, accessToken string) string {
	return fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", user, accessToken)
}
