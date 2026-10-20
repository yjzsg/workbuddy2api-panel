package redisstore

// filestore.go 本地文件版粘性会话存储（2026-10-20）。
//
// 背景：未配置 Upstash 时 Store 退化为 Noop（纯内存），于是**每次重启都丢光粘性绑定**。
// 会话被重新哈希到别的账号，而上游前缀缓存按 uid 隔离 —— 重启后活跃会话要整段重算。
// 2026-10-20 实测：容器 16:47:12 重启，16:47:22 / 16:47:44 / 16:47:50 三条大上下文
// 请求命中率分别 0.9% / 2.8% / 0.4%（TTFB 10.5s / 10.9s / 9.8s），而重启前的同类请求
// 命中 99.8%、TTFB 3.3s。当天为了发版重启了 4 次，每次都付一遍这个代价。
//
// 本实现把绑定落本地 JSON（原子写：tmp + rename），启动时读回；语义与 Redis 版一致，
// 只是持久介质从 Upstash 换成本地盘。池状态快照仍由 pool 自己的 state.json 负责，
// 故 SaveState/LoadState 在此为空操作。

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// fileBind 一条粘性绑定：uid + 过期时刻（Load 时按它过滤陈旧项）。
type fileBind struct {
	UID       string    `json:"uid"`
	ExpiresAt time.Time `json:"expires_at"`
}

// fileFlushInterval 脏数据落盘节流：绑定变更很频繁（每轮对话都 touch），
// 但一次进程崩溃最多只丢这个窗口内的绑定，代价可接受。
const fileFlushInterval = 5 * time.Second

// FileStore 本地文件版 Store。
type FileStore struct {
	path string

	mu    sync.Mutex
	binds map[string]fileBind
	dirty bool

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewFileStore 构建文件存储并加载已有绑定。
//
// 文件不存在 / 解析失败 → 视为空（只记日志，绝不因持久化问题阻断启动，
// 与 Redis 版"硬性降级"口径一致）。空 path → 返回 Noop。
func NewFileStore(path string) Store {
	if path == "" {
		return Noop{}
	}
	f := &FileStore{
		path:  path,
		binds: map[string]fileBind{},
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if raw, err := os.ReadFile(path); err == nil {
		var loaded map[string]fileBind
		if err := json.Unmarshal(raw, &loaded); err != nil {
			log.Printf("WARN: [redisstore] 粘性绑定文件解析失败，按空处理: %v", err)
		} else {
			now := time.Now()
			for k, v := range loaded {
				if v.UID == "" || (!v.ExpiresAt.IsZero() && now.After(v.ExpiresAt)) {
					continue
				}
				f.binds[k] = v
			}
			log.Printf("[redisstore] 从 %s 恢复 %d 条粘性绑定", path, len(f.binds))
		}
	}
	go f.loop()
	return f
}

// loop 周期性把脏数据落盘。
func (f *FileStore) loop() {
	defer close(f.done)
	t := time.NewTicker(fileFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			f.mu.Lock()
			f.flushLocked()
			f.mu.Unlock()
			return
		case <-t.C:
			f.mu.Lock()
			f.flushLocked()
			f.mu.Unlock()
		}
	}
}

// flushLocked 原子落盘（调用方须持锁）。
func (f *FileStore) flushLocked() {
	if !f.dirty {
		return
	}
	// 顺手清理过期项，防文件无限增长。
	now := time.Now()
	for k, v := range f.binds {
		if !v.ExpiresAt.IsZero() && now.After(v.ExpiresAt) {
			delete(f.binds, k)
		}
	}
	raw, err := json.Marshal(f.binds)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		log.Printf("WARN: [redisstore] 建目录失败 %s: %v", f.path, err)
		return
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [redisstore] 写粘性绑定失败 %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, f.path); err != nil {
		log.Printf("WARN: [redisstore] 替换粘性绑定文件失败 %s: %v", f.path, err)
		return
	}
	f.dirty = false
}

// SetBind 记录一条绑定（内存即时生效，落盘由后台节流）。
func (f *FileStore) SetBind(key, uid string, ttl time.Duration) {
	if key == "" || uid == "" {
		return
	}
	// ttl == 0 表示「不设过期」；负值即已过期（测试与显式失效用）。
	var exp time.Time
	if ttl != 0 {
		exp = time.Now().Add(ttl)
	}
	f.mu.Lock()
	f.binds[key] = fileBind{UID: uid, ExpiresAt: exp}
	f.dirty = true
	f.mu.Unlock()
}

// DelBind 删除一条绑定。
func (f *FileStore) DelBind(key string) {
	if key == "" {
		return
	}
	f.mu.Lock()
	if _, ok := f.binds[key]; ok {
		delete(f.binds, key)
		f.dirty = true
	}
	f.mu.Unlock()
}

// LoadBinds 返回未过期的全量绑定（启动时同步调用一次）。
func (f *FileStore) LoadBinds() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	out := make(map[string]string, len(f.binds))
	for k, v := range f.binds {
		if !v.ExpiresAt.IsZero() && now.After(v.ExpiresAt) {
			continue
		}
		out[k] = v.UID
	}
	return out
}

// SaveState 空操作：池状态快照由 pool 自己的 state.json 负责，这里不重复落盘。
func (f *FileStore) SaveState([]byte) {}

// LoadState 恒 false：没有 Redis 快照可比，恢复走本地 state.json。
func (f *FileStore) LoadState() ([]byte, bool) { return nil, false }

// Close 停止后台落盘并做最后一次 flush（幂等）。
func (f *FileStore) Close() error {
	f.closeOnce.Do(func() {
		close(f.stop)
		<-f.done
	})
	return nil
}
