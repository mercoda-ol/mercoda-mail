package status

import (
	"sync"
	"time"
)

// Status 单个账号的连接状态
type Status struct {
	Name      string    `json:"name"`
	Online    bool      `json:"online"`
	LastSync  time.Time `json:"last_sync"`
	LastError string    `json:"last_error"`
}

// Registry 全局账号状态表（manager 写，web 读）
type Registry struct {
	mu       sync.RWMutex
	statuses map[string]*Status
}

// NewRegistry 创建状态表
func NewRegistry() *Registry {
	return &Registry{statuses: map[string]*Status{}}
}

// Register 注册账号
func (r *Registry) Register(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[name] = &Status{Name: name}
}

// SetOnline 标记在线
func (r *Registry) SetOnline(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.statuses[name]; ok {
		s.Online = true
		s.LastError = ""
	}
}

// SetOffline 标记离线并记录错误
func (r *Registry) SetOffline(name, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.statuses[name]; ok {
		s.Online = false
		s.LastError = errMsg
	}
}

// TouchSync 更新最后同步时间
func (r *Registry) TouchSync(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.statuses[name]; ok {
		s.LastSync = time.Now()
	}
}

// Snapshot 返回所有账号状态副本
func (r *Registry) Snapshot() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Status, 0, len(r.statuses))
	for _, s := range r.statuses {
		out = append(out, *s)
	}
	return out
}
