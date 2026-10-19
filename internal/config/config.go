package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 是整个应用的配置
type Config struct {
	Database Database `yaml:"database"`
	Notify   Notify   `yaml:"notify"`
	Web      Web      `yaml:"web"`
	Spam     Spam     `yaml:"spam"`
	Imap     ImapTuning `yaml:"imap"`
	Log      Log      `yaml:"log"`
	Translate Translate `yaml:"translate"`
	Rules    []Rule   `yaml:"rules"`
	Accounts []Account `yaml:"accounts"`
}

// Log 日志轮转配置（单文件超限滚动历史，磁盘封顶 max_size_mb×(keep+1)）
type Log struct {
	MaxSizeMB int `yaml:"max_size_mb"` // 单文件上限 MB，默认 20
	Keep      int `yaml:"keep"`        // 保留历史份数（.1~.N），默认 3
}

// Translate 邮件翻译代理配置（本地 Edge Translator 不可用时走后端转发）
type Translate struct {
	TimeoutSeconds int `yaml:"timeout_seconds"` // 后端转发超时（秒），默认 10
}

// ImapTuning IMAP 收信运行参数（防硬编码，统一进配置）
type ImapTuning struct {
	ReconnectInterval   int `yaml:"reconnect_interval"`   // 断线重连基础间隔（秒），默认 10，指数退避起点
	ReconnectMaxBackoff int `yaml:"reconnect_max_backoff"` // 重连退避上限（秒），默认 300
	IdleTimeout         int `yaml:"idle_timeout"`         // IDLE 兜底重启间隔（秒），默认 120
	IdleFailThreshold   int `yaml:"idle_fail_threshold"`  // IDLE 循环连续失败阈值，达到即退出回 Run 层退避重连（防日志风暴），默认 5
	BatchSize           int `yaml:"batch_size"`           // 增量同步分批 UID 拉取批量，默认 200
	MaxBodyBytes        int `yaml:"max_body_bytes"`       // 正文入库截断上限（字节），默认 262144
}

// Spam 垃圾箱收取配置（轮询方式，处理流程与收件箱一致）
type Spam struct {
	Enabled       bool   `yaml:"enabled"`
	PollSeconds   int    `yaml:"poll_seconds"`    // 轮询间隔，默认 120
	Folder        string `yaml:"folder"`          // 手动指定垃圾箱文件夹，留空自动探测
}

type Web struct {
	Enabled      bool   `yaml:"enabled"`
	Port         int    `yaml:"port"`
	EmailPageSize int   `yaml:"email_page_size"` // 邮件列表每页条数，默认 14
	Devtools     bool   `yaml:"devtools"`        // WebView2 开 DevTools（F12/右键），长跑性能取证用
}

type Database struct {
	Path string `yaml:"path"`
}

type Notify struct {
	Enabled        bool              `yaml:"enabled"`
	Sound          string            `yaml:"sound"`
	MinInterval    int               `yaml:"min_interval_seconds"`
	CategorySounds map[string]string `yaml:"category_sounds"`   // 按分类覆盖提示音（如 order: Call），未配置用 Sound
	OnlyCategories []string          `yaml:"only_categories"`   // 仅这些分类发系统通知（网页提示不受限），空则用默认三类
}

type Rule struct {
	Name     string   `yaml:"name"`
	Category string   `yaml:"category"`
	Alert    bool     `yaml:"alert"`
	Keywords []string `yaml:"keywords"`
}

type Account struct {
	Name     string `yaml:"name"`
	Email    string `yaml:"email"`
	Provider string `yaml:"provider"` // gmail, outlook, generic
	IMAP     IMAP   `yaml:"imap"`
	Auth     Auth   `yaml:"auth"`
}

type IMAP struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type Auth struct {
	Type     string `yaml:"type"` // oauth2, app_password
	Username string `yaml:"username"`
	Password string `yaml:"password"` // app_password
}

// Load 从 YAML 文件加载配置
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 默认值
	if cfg.Web.EmailPageSize <= 0 {
		cfg.Web.EmailPageSize = 14
	}
	if cfg.Spam.PollSeconds <= 0 {
		cfg.Spam.PollSeconds = 120
	}
	if cfg.Imap.ReconnectInterval <= 0 {
		cfg.Imap.ReconnectInterval = 10
	}
	if cfg.Imap.ReconnectMaxBackoff <= 0 {
		cfg.Imap.ReconnectMaxBackoff = 300
	}
	if cfg.Imap.IdleTimeout <= 0 {
		cfg.Imap.IdleTimeout = 120
	}
	if cfg.Imap.BatchSize <= 0 {
		cfg.Imap.BatchSize = 200
	}
	if cfg.Imap.MaxBodyBytes <= 0 {
		cfg.Imap.MaxBodyBytes = 262144
	}
	if cfg.Imap.IdleFailThreshold <= 0 {
		cfg.Imap.IdleFailThreshold = 5
	}
	if cfg.Log.MaxSizeMB <= 0 {
		cfg.Log.MaxSizeMB = 20
	}
	if cfg.Log.Keep <= 0 {
		cfg.Log.Keep = 3
	}
	if cfg.Translate.TimeoutSeconds <= 0 {
		cfg.Translate.TimeoutSeconds = 10
	}
	if len(cfg.Notify.OnlyCategories) == 0 {
		cfg.Notify.OnlyCategories = []string{"order", "abnormal_order", "account_security", "customer_support"}
	}

	return &cfg, nil
}
