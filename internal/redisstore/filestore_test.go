package redisstore

import (
	"path/filepath"
	"testing"
	"time"
)

// TestFileStoreRoundTrip 绑定必须跨进程实例持久化（重启后仍能恢复粘性）。
//
// 背景：此前未配 Upstash 时退化为 Noop（纯内存），重启即丢绑定 → 会话重哈希到
// 别的账号 → 上游前缀缓存按 uid 隔离 → 400k+ 上下文全量重算（实测 TTFB 3.3s → 10.9s）。
func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_binds.json")

	s := NewFileStore(path)
	s.SetBind("k1", "u1", time.Hour)
	s.SetBind("k2", "u2", time.Hour)
	if got := s.LoadBinds(); len(got) != 2 || got["k1"] != "u1" || got["k2"] != "u2" {
		t.Fatalf("内存视图不正确: %v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 新实例（模拟重启）应读回同样的绑定。
	s2 := NewFileStore(path)
	defer s2.Close()
	got := s2.LoadBinds()
	if len(got) != 2 || got["k1"] != "u1" || got["k2"] != "u2" {
		t.Fatalf("重启后未恢复绑定: %v", got)
	}
}

// TestFileStoreDeleteAndExpiry 删除与过期项都不应复活。
func TestFileStoreDeleteAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_binds.json")

	s := NewFileStore(path)
	s.SetBind("k1", "u1", time.Hour)
	s.SetBind("k2", "u2", time.Hour)
	s.DelBind("k1")
	s.SetBind("expired", "u9", -time.Second) // ttl 为负 = 已过期
	if got := s.LoadBinds(); len(got) != 1 || got["k2"] != "u2" {
		t.Fatalf("删除/过期过滤不正确: %v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := NewFileStore(path)
	defer s2.Close()
	if got := s2.LoadBinds(); len(got) != 1 || got["k2"] != "u2" {
		t.Fatalf("重启后删除/过期项复活: %v", got)
	}
}

// TestFileStoreEmptyPathNoop 空路径退回 Noop（与既有降级口径一致）。
func TestFileStoreEmptyPathNoop(t *testing.T) {
	if _, ok := NewFileStore("").(Noop); !ok {
		t.Fatal("空路径应返回 Noop")
	}
}

// TestFileStoreCloseIdempotent 重复 Close 不应 panic（停机路径可能被多次触发）。
func TestFileStoreCloseIdempotent(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "b.json"))
	s.SetBind("k", "u", time.Hour)
	if err := s.Close(); err != nil {
		t.Fatalf("第一次 Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("第二次 Close 应无副作用: %v", err)
	}
}

// TestFileStoreStateNoop 池状态快照由 state.json 负责，文件 Store 不重复落盘。
func TestFileStoreStateNoop(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "b.json"))
	defer s.Close()
	s.SaveState([]byte(`{"x":1}`))
	if _, ok := s.LoadState(); ok {
		t.Fatal("FileStore 不应提供 Redis 状态快照")
	}
}
