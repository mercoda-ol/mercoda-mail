package events

import "sync"

// Event 推送给前端的事件
type Event struct {
	Type string      `json:"type"` // new_email / new_alert
	Data interface{} `json:"data"`
}

// Hub SSE 事件广播中心
type Hub struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

// NewHub 创建广播中心
func NewHub() *Hub {
	return &Hub{subs: map[chan Event]struct{}{}}
}

// Subscribe 订阅事件（返回接收 channel，用 Unsubscribe 释放）
func (h *Hub) Subscribe() chan Event {
	ch := make(chan Event, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe 取消订阅
func (h *Hub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

// Publish 广播事件（非阻塞，慢订阅者丢弃本条）
func (h *Hub) Publish(e Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
