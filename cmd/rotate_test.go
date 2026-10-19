package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	w, err := newRotateWriter(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 写入 4 次×60B：超限应滚动，最终原始文件 ≤ limit，历史存在 .1/.2
	for i := 0; i < 4; i++ {
		if _, err := w.Write([]byte(strings.Repeat("x", 60) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	w.mu.Lock()
	n := w.n
	w.mu.Unlock()
	if n > 100 {
		t.Errorf("轮转后当前文件 %dB 超限 100B", n)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("历史 .1 未生成: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Errorf("历史 .2 未生成: %v", err)
	}
	// keep=2 时不应有 .3
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("keep=2 不应生成 .3")
	}
	// 关闭句柄，否则 TempDir 清理失败（文件被占用）
	w.mu.Lock()
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	w.mu.Unlock()
}
