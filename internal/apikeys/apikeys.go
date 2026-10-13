// Package apikeys 多 API 密钥：存储、常量时间匹配、模型白名单。
//
// 为什么单独成包：认证发生在两处（server 的 /v1+/status 与 panel 的 /panel/api/*），
// 白名单判定发生在 server 侧模型解析之后——两处必须同口径，且都要知道"这次命中的是
// 哪把密钥"（用量统计要按 key 分维度）。此前只有一根 config.json 的 api_key，
// 两处各自调 httpauth.VerifyBearer，拿不到身份。
//
// 存储：独立的 keys.json（不与 config.json 混写）。理由：
//   - 面板要增删改密钥，写回 config.json 会与"用户手工编辑的配置"互相覆盖；
//   - 密钥是敏感数据，独立文件便于单独收紧权限（Save 固定 0600）。
//
// 兼容：config.json 的 api_key 在首次启动时被迁移成默认密钥（不限制模型），
// 老客户端无需改动。迁移只做一次，之后以 keys.json 为准。
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fileVersion keys.json 的当前格式版本。
const fileVersion = 1

// bearerPrefix 认证方案前缀（与 internal/httpauth 同口径：大小写敏感）。
const bearerPrefix = "Bearer "

// Key 一把 API 密钥。
type Key struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Secret 明文保存：自托管单用户场景，且面板需要查看/复制（一旦只存哈希，
	// 用户丢了就只能重建）。文件权限由 Save 固定为 0600。
	Secret  string `json:"secret"`
	Note    string `json:"note,omitempty"`
	Enabled bool   `json:"enabled"`
	// AllowedModels 模型白名单；**空 = 不限制**（放行全部模型）。
	// 条目写法：`glm-5.2` 匹配任意域的该模型；`global:gpt-6-luna` 精确匹配带域写法。
	AllowedModels []string `json:"allowed_models,omitempty"`
	CreatedAt     string   `json:"created_at,omitempty"`
	// MigratedFromConfig 标记该密钥由 config.json 的 api_key 迁移而来，
	// 仅用于面板提示（"这是从旧配置迁移的"），不参与任何判定。
	MigratedFromConfig bool `json:"migrated_from_config,omitempty"`
}

// AllowsModel 判断该密钥是否可用某个模型。
//
// realm/model 是 handler 解析后的两段（见 server.resolveModel）；model 为裸名。
// 白名单为空 = 不限制。
func (k *Key) AllowsModel(realm, model string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	full := model
	if realm != "" {
		full = realm + ":" + model
	}
	for _, a := range k.AllowedModels {
		a = strings.TrimSpace(a)
		if a != "" && (a == model || a == full) {
			return true
		}
	}
	return false
}

// Store 并发安全的密钥表（内存镜像 + 落盘）。
type Store struct {
	mu   sync.RWMutex
	path string
	keys []Key
}

// NewStore 创建密钥表。path 为空时禁用落盘（纯内存，测试用）。
func NewStore(path string) *Store { return &Store{path: path} }

// Path 返回落盘路径。
func (s *Store) Path() string { return s.path }

type fileShape struct {
	Version int    `json:"version"`
	Saved   string `json:"saved"`
	Keys    []Key  `json:"keys"`
}

// Load 从磁盘读取。文件不存在视为空表（首次运行，不报错）。
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f fileShape
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.keys = f.Keys
	s.mu.Unlock()
	return nil
}

// Save 原子落盘（临时文件 + rename，权限 0600）。
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	f := fileShape{
		Version: fileVersion,
		Saved:   time.Now().Format(time.RFC3339),
		Keys:    append([]Key(nil), s.keys...),
	}
	s.mu.RUnlock()
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if d := filepath.Dir(s.path); d != "" && d != "." {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List 返回密钥副本（含明文 secret；调用方自行决定是否脱敏）。
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Key(nil), s.keys...)
}

// Count 返回密钥总数（含停用）。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Get 按 ID 取单把密钥。
func (s *Store) Get(id string) (Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if k.ID == id {
			return k, true
		}
	}
	return Key{}, false
}

// MatchAuthz 按 Authorization 头原文匹配密钥。
//
// 返回值语义：
//   - (nil, true)   表为空 ⇒ 未启用鉴权，放行（与 config.api_key 为空的旧行为一致）
//   - (&key, true)  命中且该密钥已启用
//   - (nil, false)  未命中 / 已停用 / 头缺失或方案不对
//
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare，且**对所有密钥都做一次比较**
// （不因命中提前返回），保持耗时形状与密钥数量一致、与命中位置无关。
func (s *Store) MatchAuthz(authz string) (*Key, bool) {
	s.mu.RLock()
	keys := append([]Key(nil), s.keys...)
	s.mu.RUnlock()
	if len(keys) == 0 {
		return nil, true
	}
	tok := ""
	if strings.HasPrefix(authz, bearerPrefix) {
		tok = authz[len(bearerPrefix):]
	}
	h := digest(tok)
	var hit *Key
	for i := range keys {
		k := &keys[i]
		match := subtle.ConstantTimeCompare(h, digest(k.Secret)) == 1
		if match && k.Enabled {
			hit = k
		}
	}
	if hit == nil {
		return nil, false
	}
	return hit, true
}

// Patch 一次局部更新；nil 字段表示不改。
type Patch struct {
	Name          *string
	Secret        *string
	Note          *string
	Enabled       *bool
	AllowedModels *[]string
}

// Add 新增一把密钥。secret 为空时随机生成。返回落盘后的完整密钥。
func (s *Store) Add(name, secret, note string, allowed []string, enabled bool) (Key, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "未命名"
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		sec, err := GenSecret()
		if err != nil {
			return Key{}, err
		}
		secret = sec
	}
	s.mu.Lock()
	for _, k := range s.keys {
		if k.Secret == secret {
			s.mu.Unlock()
			return Key{}, fmt.Errorf("该密钥值已存在（重复会让用量统计失去区分度）")
		}
	}
	key := Key{
		ID:            nextID(),
		Name:          name,
		Secret:        secret,
		Note:          note,
		Enabled:       enabled,
		AllowedModels: normAllowed(allowed),
		CreatedAt:     time.Now().Format(time.RFC3339),
	}
	s.keys = append(s.keys, key)
	s.mu.Unlock()
	if err := s.Save(); err != nil {
		return Key{}, err
	}
	return key, nil
}

// Update 局部更新一把密钥。返回更新后的完整密钥。
func (s *Store) Update(id string, p Patch) (Key, error) {
	s.mu.Lock()
	idx := -1
	for i, k := range s.keys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return Key{}, fmt.Errorf("密钥不存在")
	}
	cur := s.keys[idx]
	if p.Name != nil {
		n := strings.TrimSpace(*p.Name)
		if n == "" {
			n = "未命名"
		}
		cur.Name = n
	}
	if p.Note != nil {
		cur.Note = *p.Note
	}
	if p.AllowedModels != nil {
		cur.AllowedModels = normAllowed(*p.AllowedModels)
	}
	if p.Secret != nil {
		sec := strings.TrimSpace(*p.Secret)
		if sec == "" {
			s.mu.Unlock()
			return Key{}, fmt.Errorf("密钥值不能为空")
		}
		for i, k := range s.keys {
			if i != idx && k.Secret == sec {
				s.mu.Unlock()
				return Key{}, fmt.Errorf("该密钥值已存在")
			}
		}
		cur.Secret = sec
	}
	if p.Enabled != nil {
		if !*p.Enabled && cur.Enabled {
			// 停用最后一把启用中的密钥 = 网关不再鉴权，拒绝。
			remain := 0
			for i, k := range s.keys {
				if i != idx && k.Enabled {
					remain++
				}
			}
			if remain == 0 {
				s.mu.Unlock()
				return Key{}, fmt.Errorf("不能停用最后一把启用中的密钥（否则网关将不再鉴权）")
			}
		}
		cur.Enabled = *p.Enabled
	}
	s.keys[idx] = cur
	s.mu.Unlock()
	if err := s.Save(); err != nil {
		return Key{}, err
	}
	return cur, nil
}

// Delete 删除一把密钥。拒删最后一把启用中的密钥。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	idx := -1
	for i, k := range s.keys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return fmt.Errorf("密钥不存在")
	}
	remain := 0
	for i, k := range s.keys {
		if i != idx && k.Enabled {
			remain++
		}
	}
	if remain == 0 {
		s.mu.Unlock()
		return fmt.Errorf("不能删除最后一把启用中的密钥（否则网关将不再鉴权）")
	}
	s.keys = append(s.keys[:idx], s.keys[idx+1:]...)
	s.mu.Unlock()
	return s.Save()
}

// EnsureDefault 保证表非空：空表且 secret 非空时，用它建一把默认密钥。
// 返回是否新建。
//
// 两个用途：
//   - 首次升级：把 config.json 的 api_key 迁移过来（老客户端零改动）；
//   - 用户手工删掉 keys.json：重新按 config.json 兜底。
//
// secret 为空时**不建**——保持"表为空 ⇒ 不鉴权"的旧语义（config.api_key 为空的场景）。
func (s *Store) EnsureDefault(secret, note string) (bool, error) {
	if strings.TrimSpace(secret) == "" {
		return false, nil
	}
	s.mu.RLock()
	n := len(s.keys)
	s.mu.RUnlock()
	if n > 0 {
		return false, nil
	}
	key, err := s.Add("默认密钥", secret, note, nil, true)
	if err != nil {
		return false, err
	}
	// 打迁移标记后重存一次（Add 已经把文件写好了，这里只补标记）。
	s.mu.Lock()
	for i := range s.keys {
		if s.keys[i].ID == key.ID {
			s.keys[i].MigratedFromConfig = true
		}
	}
	s.mu.Unlock()
	return true, s.Save()
}

// GenSecret 生成随机密钥（wb2a_ 前缀 + 16 字节熵的 hex）。
func GenSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("gen secret: %w", err)
	}
	return "wb2a_" + hex.EncodeToString(b), nil
}

// nextID 生成短 ID（用于用量维度与面板 API 路径）。
func nextID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("k_%d", time.Now().UnixNano())
	}
	return "k_" + hex.EncodeToString(b)
}

// Mask 脱敏展示：保留前 4 后 4，中间打码；短密钥整体打码。
func Mask(secret string) string {
	r := []rune(secret)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + strings.Repeat("*", 6) + string(r[len(r)-4:])
}

// normAllowed 清洗白名单：去空白、去重、丢空项；结果为空返回 nil（= 不限制）。
func normAllowed(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
