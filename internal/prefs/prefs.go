package prefs

import (
	"encoding/json"
	"os"
)

// Prefs 启动模式偏好（记忆上次选择）
type Prefs struct {
	Mode      string `json:"mode"`       // server / client
	ServerURL string `json:"server_url"` // client 模式连接的服务地址
}

// Load 读取偏好（文件不存在返回空）
func Load(path string) (Prefs, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Prefs{}, nil
	}
	if err != nil {
		return Prefs{}, err
	}
	var p Prefs
	if err := json.Unmarshal(data, &p); err != nil {
		return Prefs{}, err
	}
	return p, nil
}

// Save 保存偏好
func Save(path string, p Prefs) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
