package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// TokenStore refresh token 加密存储（AES-GCM）
// 文件格式: secrets.enc = nonce + ciphertext；密钥存 secrets.key（32 字节随机，需备份）
type TokenStore struct {
	path     string
	keyPath string
	mu       sync.Mutex
}

type tokenEntry struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

type tokenFile map[string]tokenEntry // key: 账号名

// NewTokenStore 创建 token 存储（懒加载密钥）
func NewTokenStore(path, keyPath string) *TokenStore {
	return &TokenStore{path: path, keyPath: keyPath}
}

// loadKey 加载或生成 AES 密钥
func (ts *TokenStore) loadKey() ([]byte, error) {
	if data, err := os.ReadFile(ts.keyPath); err == nil && len(data) == 32 {
		return data, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(ts.keyPath, key, 0600); err != nil {
		return nil, err
	}
	return key, nil
}

// load 读取并解密 token 文件
func (ts *TokenStore) load() (tokenFile, error) {
	data, err := os.ReadFile(ts.path)
	if os.IsNotExist(err) {
		return tokenFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) < 12 {
		return tokenFile{}, fmt.Errorf("token 文件损坏")
	}
	key, err := ts.loadKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, ct := data[:12], data[12:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("解密 token 失败: %w", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(plain, &tf); err != nil {
		return nil, err
	}
	return tf, nil
}

// save 加密并写入 token 文件
func (ts *TokenStore) save(tf tokenFile) error {
	plain, err := json.Marshal(tf)
	if err != nil {
		return err
	}
	key, err := ts.loadKey()
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nonce, nonce, plain, nil)
	return os.WriteFile(ts.path, ct, 0600)
}

// Get 读取某账号的 token 条目
func (ts *TokenStore) Get(account string) (tokenEntry, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	tf, err := ts.load()
	if err != nil {
		return tokenEntry{}, err
	}
	return tf[account], nil
}

// Put 保存某账号的 token 条目
func (ts *TokenStore) Put(account string, entry tokenEntry) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	tf, err := ts.load()
	if err != nil {
		return err
	}
	tf[account] = entry
	return ts.save(tf)
}
