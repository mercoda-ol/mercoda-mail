package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"time"

	"mercado-mail/internal/config"
)

// OAuthClient OAuth2 客户端凭据（注册在 Google Cloud / Azure，内置或配置）
// 桌面应用走公共客户端 + PKCE：全程只用 ClientID，不使用 ClientSecret（带 secret 会被 Azure 拒 AADSTS90023）
type OAuthClient struct {
	ClientID string
	AuthURL  string
	TokenURL string
	Scopes   []string
	// RedirectPort 本地回调端口（0 = 随机）
	RedirectPort int
}

// builtinClients 内置 OAuth 客户端凭据（部署前替换为自己的）
// TODO: 上线前填入自己注册的 Google Cloud / Azure App 凭据
var builtinClients = map[string]OAuthClient{
	"gmail": {
		ClientID:     "", // TODO: 填入 Google OAuth Client ID
		AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		Scopes:       []string{"https://mail.google.com/"},
		RedirectPort: 0,
	},
	"outlook": {
		ClientID:     "515e90c8-62e0-4e93-a672-39eb6456a9c5",
		AuthURL:      "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		TokenURL:     "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		Scopes:       []string{"https://outlook.office.com/IMAP.AccessAsUser.All", "offline_access"},
		RedirectPort: 0,
	},
}

// GetOAuthClient 获取指定 provider 的 OAuth 客户端配置
func GetOAuthClient(provider string) (OAuthClient, error) {
	c, ok := builtinClients[provider]
	if !ok {
		return OAuthClient{}, fmt.Errorf("不支持的 OAuth2 邮箱: %s", provider)
	}
	if c.ClientID == "" {
		return OAuthClient{}, fmt.Errorf("provider %s 的 OAuth Client ID 未配置（请在 internal/auth/oauth2.go builtinClients 中填入）", provider)
	}
	return c, nil
}

// Authorize 首次授权：打开浏览器 → 本地回调收 code → 换 token → 加密存储
// 返回后 refresh token 可长期静默使用
func Authorize(cfg config.Account, ts *TokenStore) error {
	oc, err := GetOAuthClient(cfg.Provider)
	if err != nil {
		return err
	}

	// 本地回调 listener（localhost 以便匹配 Azure 注册的 http://localhost，微软 loopback 规则允许任意端口）
	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", oc.RedirectPort))
	if err != nil {
		return fmt.Errorf("本地回调监听失败: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://localhost:%d/", port)

	// PKCE + state
	verifierBytes := make([]byte, 32)
	rand.Read(verifierBytes)
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	authURL := oc.AuthURL + "?" + url.Values{
		"client_id":                {oc.ClientID},
		"redirect_uri":             {redirectURL},
		"response_type":            {"code"},
		"scope":                    {joinScopes(oc.Scopes)},
		"state":                    {state},
		"access_type":              {"offline"},
		"prompt":                   {"consent"},
		"code_challenge":           {challenge},
		"code_challenge_method":    {"S256"},
	}.Encode()

	// 等待回调（5 分钟超时）
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state 不匹配", http.StatusBadRequest)
			errCh <- fmt.Errorf("OAuth state 不匹配")
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, "授权失败: "+e, http.StatusBadRequest)
			errCh <- fmt.Errorf("授权被拒绝: %s", e)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "缺少 code", http.StatusBadRequest)
			errCh <- fmt.Errorf("回调缺少 code")
			return
		}
		fmt.Fprint(w, "授权成功，可以关闭此页面。")
		codeCh <- code
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	// 打开浏览器
	fmt.Printf("正在打开浏览器授权 %s ...\n若未自动打开，请访问:\n%s\n", cfg.Email, authURL)
	if err := openBrowser(authURL); err != nil {
		log.Printf("[auth] 打开浏览器失败: %v", err)
	}

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("授权超时（5 分钟）")
	}

	// 换 token（公共客户端 + PKCE：不能带 client_secret，Azure 会拒 AADSTS90023）
	form := url.Values{
		"client_id":     {oc.ClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURL},
	}
	resp, err := http.PostForm(oc.TokenURL, form)
	if err != nil {
		return fmt.Errorf("请求 token 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("换取 token 失败 [%d]: %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("解析 token 响应失败: %w", err)
	}
	if tr.RefreshToken == "" {
		return fmt.Errorf("未获得 refresh token，请撤销授权后重试")
	}

	return ts.Put(cfg.Name, tokenEntry{
		RefreshToken: tr.RefreshToken,
		AccessToken:  tr.AccessToken,
		ExpiresAt:    time.Now().Unix() + int64(tr.ExpiresIn),
	})
}

// AccessToken 获取可用的 access token（必要时用 refresh token 刷新）
func AccessToken(cfg config.Account, ts *TokenStore) (string, error) {
	oc, err := GetOAuthClient(cfg.Provider)
	if err != nil {
		return "", err
	}

	entry, err := ts.Get(cfg.Name)
	if err != nil {
		return "", err
	}
	if entry.RefreshToken == "" {
		return "", fmt.Errorf("账号 %s 未授权，请在管理界面「账号管理」中点击「授权」完成 OAuth 登录", cfg.Name)
	}

	// 有效期余量 5 分钟内则刷新
	if entry.AccessToken != "" && entry.ExpiresAt > time.Now().Unix()+300 {
		return entry.AccessToken, nil
	}

	form := url.Values{
		"client_id":     {oc.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {entry.RefreshToken},
	}
	resp, err := http.PostForm(oc.TokenURL, form)
	if err != nil {
		return "", fmt.Errorf("刷新 token 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("刷新 token 失败 [%d]: %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}

	entry.AccessToken = tr.AccessToken
	entry.ExpiresAt = time.Now().Unix() + int64(tr.ExpiresIn)
	if err := ts.Put(cfg.Name, entry); err != nil {
		return "", err
	}
	return tr.AccessToken, nil
}

func openBrowser(url string) error {
	// 不能用 cmd /c start：URL 含 & 时会被 cmd 当命令分截断（Azure 授权 URL 必带 &），
	// rundll32 不经 cmd 解析，天然免疫
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func joinScopes(scopes []string) string {
	s := ""
	for i, sc := range scopes {
		if i > 0 {
			s += " "
		}
		s += sc
	}
	return s
}
