package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("创建测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSaveEmailDedup(t *testing.T) {
	st := newTestStore(t)
	e := &Email{Account: "店铺1", UID: 100, Folder: "INBOX", Subject: "test", Category: "order", Timestamp: time.Now()}

	id1, isNew1, err := st.SaveEmail(e)
	if err != nil || !isNew1 || id1 == 0 {
		t.Fatalf("首次入库应成功: id=%d new=%v err=%v", id1, isNew1, err)
	}

	id2, isNew2, err := st.SaveEmail(e)
	if err != nil || isNew2 {
		t.Fatalf("重复入库应跳过: new=%v err=%v", isNew2, err)
	}
	if id1 != id2 {
		t.Fatalf("重复邮件 ID 应一致: %d != %d", id1, id2)
	}
}

func TestSyncState(t *testing.T) {
	st := newTestStore(t)

	uid, err := st.GetLastUID("店铺1", "INBOX")
	if err != nil || uid != 0 {
		t.Fatalf("初始 UID 应为 0: uid=%d err=%v", uid, err)
	}

	if err := st.UpdateLastUID("店铺1", "INBOX", 55); err != nil {
		t.Fatalf("更新 UID 失败: %v", err)
	}
	if err := st.UpdateLastUID("店铺1", "INBOX", 99); err != nil {
		t.Fatalf("再次更新 UID 失败: %v", err)
	}

	uid, err = st.GetLastUID("店铺1", "INBOX")
	if err != nil || uid != 99 {
		t.Fatalf("UID 应为 99: uid=%d err=%v", uid, err)
	}

	// 不同账号独立
	if uid, _ := st.GetLastUID("店铺2", "INBOX"); uid != 0 {
		t.Fatalf("店铺2 应为独立状态: %d", uid)
	}
}
