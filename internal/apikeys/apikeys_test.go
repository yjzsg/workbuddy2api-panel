package apikeys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllowsModel(t *testing.T) {
	any := &Key{} // 空白名单 = 不限制
	if !any.AllowsModel("cn", "glm-5.2") || !any.AllowsModel("global", "gpt-6-luna") {
		t.Fatal("空白名单应放行全部模型")
	}

	k := &Key{AllowedModels: []string{"glm-5.2", "global:gpt-6-luna"}}
	cases := []struct {
		realm, model string
		want         bool
	}{
		{"cn", "glm-5.2", true},           // 裸名条目匹配任意域
		{"global", "glm-5.2", true},       // 同上
		{"global", "gpt-6-luna", true},    // realm:model 条目精确匹配
		{"cn", "gpt-6-luna", false},       // 域不对
		{"cn", "deepseek-v4-pro", false},  // 不在名单
		{"global", "gpt-6-luna-x", false}, // 前缀不算命中
	}
	for _, c := range cases {
		if got := k.AllowsModel(c.realm, c.model); got != c.want {
			t.Errorf("AllowsModel(%q,%q)=%v want %v", c.realm, c.model, got, c.want)
		}
	}
}

func TestMatchAuthz(t *testing.T) {
	// 空表 = 不鉴权：放行且无身份。
	empty := NewStore("")
	if k, ok := empty.MatchAuthz(""); !ok || k != nil {
		t.Fatalf("空表应放行且无 key：(%v,%v)", k, ok)
	}

	s := NewStore("")
	k1, err := s.Add("a", "secret-a", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := s.Add("b", "secret-b", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := s.MatchAuthz("Bearer secret-a"); !ok || got == nil || got.ID != k1.ID {
		t.Fatalf("命中 secret-a 失败：%+v %v", got, ok)
	}
	if got, ok := s.MatchAuthz("Bearer secret-b"); !ok || got == nil || got.ID != k2.ID {
		t.Fatalf("命中 secret-b 失败：%+v %v", got, ok)
	}
	if _, ok := s.MatchAuthz("Bearer nope"); ok {
		t.Fatal("错误密钥不应放行")
	}
	if _, ok := s.MatchAuthz("secret-a"); ok {
		t.Fatal("缺 Bearer 前缀不应放行")
	}
	if _, ok := s.MatchAuthz(""); ok {
		t.Fatal("空头不应放行")
	}

	// 停用后不再命中。
	dis := false
	if _, err := s.Update(k1.ID, Patch{Enabled: &dis}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MatchAuthz("Bearer secret-a"); ok {
		t.Fatal("停用的密钥不应放行")
	}
}

func TestAddRejectsDuplicateSecret(t *testing.T) {
	s := NewStore("")
	if _, err := s.Add("a", "dup", "", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("b", "dup", "", nil, true); err == nil {
		t.Fatal("重复 secret 应被拒绝")
	}
}

func TestCannotDisableOrDeleteLastEnabled(t *testing.T) {
	s := NewStore("")
	k1, _ := s.Add("a", "a1", "", nil, true)
	k2, _ := s.Add("b", "b1", "", nil, true)

	// 有两把启用时，停用一把是允许的。
	dis := false
	if _, err := s.Update(k1.ID, Patch{Enabled: &dis}); err != nil {
		t.Fatalf("停用非最后一把应允许：%v", err)
	}
	// 只剩 k2 启用：停用它必须被拒。
	if _, err := s.Update(k2.ID, Patch{Enabled: &dis}); err == nil {
		t.Fatal("停用最后一把启用中的密钥应被拒绝")
	}
	// 删掉 k2（唯一启用）也必须被拒。
	if err := s.Delete(k2.ID); err == nil {
		t.Fatal("删除最后一把启用中的密钥应被拒绝")
	}
	// 删掉已停用的 k1 是允许的。
	if err := s.Delete(k1.ID); err != nil {
		t.Fatalf("删除已停用的密钥应允许：%v", err)
	}
}

func TestEnsureDefault(t *testing.T) {
	// 空 secret：保持"不鉴权"，不建。
	s := NewStore("")
	if made, err := s.EnsureDefault("", ""); err != nil || made {
		t.Fatalf("空 secret 不应建默认密钥：%v %v", made, err)
	}
	if s.Count() != 0 {
		t.Fatal("表应仍为空")
	}
	// 有 secret：建默认密钥，且带迁移标记。
	made, err := s.EnsureDefault("legacy-key", "迁移")
	if err != nil || !made {
		t.Fatalf("应建默认密钥：%v %v", made, err)
	}
	list := s.List()
	if len(list) != 1 || list[0].Secret != "legacy-key" || !list[0].MigratedFromConfig || !list[0].Enabled {
		t.Fatalf("默认密钥不符：%+v", list)
	}
	if len(list[0].AllowedModels) != 0 {
		t.Fatal("默认密钥不应限制模型")
	}
	// 幂等：非空表不再建。
	if made2, _ := s.EnsureDefault("another", ""); made2 {
		t.Fatal("非空表不应再建默认密钥")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "keys.json")

	s := NewStore(p)
	if _, err := s.Add("生产", "wb2a_aaaabbbbccccdddd", "备注", []string{"glm-5.2"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("测试", "wb2a_eeeeffff00001111", "", nil, false); err != nil {
		t.Fatal(err)
	}

	// 权限 0600（密钥敏感）。
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keys.json 权限应为 0600，实际 %o", perm)
	}

	got := NewStore(p)
	if err := got.Load(); err != nil {
		t.Fatal(err)
	}
	if got.Count() != 2 {
		t.Fatalf("加载到 %d 把，want 2", got.Count())
	}
	list := got.List()
	if list[0].Name != "生产" || list[0].Secret != "wb2a_aaaabbbbccccdddd" || !list[0].Enabled {
		t.Fatalf("第一把不符：%+v", list[0])
	}
	if len(list[0].AllowedModels) != 1 || list[0].AllowedModels[0] != "glm-5.2" {
		t.Fatalf("白名单不符：%+v", list[0].AllowedModels)
	}
	if list[1].Enabled {
		t.Fatal("第二把应为停用")
	}
	// 加载后仍可认证。
	if k, ok := got.MatchAuthz("Bearer wb2a_aaaabbbbccccdddd"); !ok || k.Name != "生产" {
		t.Fatalf("加载后认证失败：%+v %v", k, ok)
	}
}

func TestLoadMissingFileIsNotError(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "nope.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("文件不存在不应报错：%v", err)
	}
	if s.Count() != 0 {
		t.Fatal("应为空表")
	}
}

func TestNormAllowedAndMask(t *testing.T) {
	// normAllowed：去空白/去重/丢空
	if got := normAllowed([]string{" glm-5.2 ", "glm-5.2", "", "a"}); len(got) != 2 || got[0] != "glm-5.2" || got[1] != "a" {
		t.Fatalf("normAllowed=%v", got)
	}
	if got := normAllowed([]string{" ", ""}); got != nil {
		t.Fatalf("全空应归一为 nil，实际 %v", got)
	}
	// Mask
	if got := Mask("wb2a_1234567890abcdef"); got != "wb2a******cdef" {
		t.Fatalf("Mask=%q", got)
	}
	if got := Mask("short"); got != "*****" {
		t.Fatalf("短密钥应整体打码，实际 %q", got)
	}
}
