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
	k1, err := s.Add("a", "secret-a", "", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := s.Add("b", "secret-b", "", nil, nil, "", true)
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
	if _, err := s.Add("a", "dup", "", nil, nil, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("b", "dup", "", nil, nil, "", true); err == nil {
		t.Fatal("重复 secret 应被拒绝")
	}
}

func TestCannotDisableOrDeleteLastEnabled(t *testing.T) {
	s := NewStore("")
	k1, _ := s.Add("a", "a1", "", nil, nil, "", true)
	k2, _ := s.Add("b", "b1", "", nil, nil, "", true)

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
	if _, err := s.Add("生产", "wb2a_aaaabbbbccccdddd", "备注", []string{"glm-5.2"}, nil, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("测试", "wb2a_eeeeffff00001111", "", nil, nil, "", false); err != nil {
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

func TestRootKeyProtectedFromDisableAndDelete(t *testing.T) {
	s := NewStore("")
	root, _ := s.Add("默认密钥", "legacy-key", "", nil, nil, "", true)
	other, _ := s.Add("别的", "other-key", "", nil, nil, "", true)
	s.SetRootSecret("legacy-key")

	// 表里还有别的启用密钥 ⇒ 不会被「最后一把」规则拦，只能是被根密钥规则拦。
	dis := false
	if _, err := s.Update(root.ID, Patch{Enabled: &dis}); err == nil {
		t.Fatal("根密钥不应能被停用")
	}
	if err := s.Delete(root.ID); err == nil {
		t.Fatal("根密钥不应能被删除")
	}
	if !s.IsRoot(root) {
		t.Fatal("IsRoot 应认出根密钥")
	}
	if s.IsRoot(other) {
		t.Fatal("非根密钥不应被判为根密钥")
	}

	// 非根密钥不受影响。
	if _, err := s.Update(other.ID, Patch{Enabled: &dis}); err != nil {
		t.Fatalf("非根密钥应可停用：%v", err)
	}
	// 根密钥的其他字段照旧可改（改名/备注/白名单）。
	name := "改个名"
	got, err := s.Update(root.ID, Patch{Name: &name})
	if err != nil {
		t.Fatalf("根密钥应可改名：%v", err)
	}
	if got.Name != "改个名" || !got.Enabled || got.Secret != "legacy-key" {
		t.Fatalf("改名不应影响启用状态与密钥值：%+v", got)
	}
}

func TestRootGuardLiftedAfterRotation(t *testing.T) {
	s := NewStore("")
	k, _ := s.Add("默认密钥", "legacy-key", "", nil, nil, "", true)
	s.SetRootSecret("legacy-key")

	// 先改掉密钥值：它不再是 config.json 的 api_key ⇒ 根密钥保护解除。
	// （此时仍受「最后一把启用」保护，因为它是唯一启用中的。）
	rotated := "rotated-key"
	// 注意用 Update 的返回值判定：k 是 Add 时的旧副本，拿它判会永远命中。
	rotatedKey, err := s.Update(k.ID, Patch{Secret: &rotated})
	if err != nil {
		t.Fatalf("应允许轮换密钥值：%v", err)
	}
	if rotatedKey.Secret != rotated {
		t.Fatalf("密钥值未更新：%+v", rotatedKey)
	}
	if s.IsRoot(rotatedKey) {
		t.Fatal("轮换后不应再是根密钥")
	}
	// 注：IsRoot 是 (rootSecret, k.Secret) 的纯函数，拿轮换前的旧副本判仍会命中
	// "旧值"——那是预期行为（该副本已不对应表里的任何条目），不作断言。
	dis := false
	if _, err := s.Update(k.ID, Patch{Enabled: &dis}); err == nil {
		t.Fatal("唯一启用中的密钥仍应受「最后一把」保护")
	}

	// 补一把启用的：现在可以停用旧的了（证明根密钥保护确实解除）。
	if _, err := s.Add("新的", "rotated2", "", nil, nil, "", true); err != nil {
		t.Fatalf("新增失败：%v", err)
	}
	if _, err := s.Update(k.ID, Patch{Enabled: &dis}); err != nil {
		t.Fatalf("轮换 + 有别的启用密钥后应可停用：%v", err)
	}
	// 已停用且不再是根 ⇒ 可以删除。（不能删新的那把：它是唯一启用中的。）
	if err := s.Delete(k.ID); err != nil {
		t.Fatalf("已停用的非根密钥应可删除：%v", err)
	}
}

func TestUpdatePartialKeepsOtherFields(t *testing.T) {
	s := NewStore("")
	k, _ := s.Add("原名", "s1", "原备注", []string{"glm-5.2"}, nil, "", true)
	name := "新名"
	got, err := s.Update(k.ID, Patch{Name: &name})
	if err != nil {
		t.Fatalf("改名失败：%v", err)
	}
	// Patch 用指针字段区分「没传」与「传了零值」——没传的字段必须原样保留。
	// （面板的「编辑」只传部分字段，若语义退化会把 enabled 误写成 false。）
	if !got.Enabled || got.Secret != "s1" || got.Note != "原备注" || len(got.AllowedModels) != 1 {
		t.Fatalf("局部更新不应影响其他字段：%+v", got)
	}
}

func TestFindBySecret(t *testing.T) {
	s := NewStore("")
	k, _ := s.Add("a", "s1", "", nil, nil, "", true)
	if got, found := s.FindBySecret("s1"); !found || got.ID != k.ID {
		t.Fatalf("FindBySecret 未命中：%+v %v", got, found)
	}
	if _, found := s.FindBySecret("nope"); found {
		t.Fatal("不存在的密钥不应命中")
	}
	// 停用后仍要能按值找到——调用方需要区分「已停用」与「不存在」。
	dis := false
	order, _ := s.Add("b", "s2", "", nil, nil, "", true)
	if _, err := s.Update(order.ID, Patch{Enabled: &dis}); err != nil {
		t.Fatalf("停用失败：%v", err)
	}
	got, found := s.FindBySecret("s2")
	if !found || got.Enabled {
		t.Fatalf("停用的密钥应能找到且 Enabled=false：%+v %v", got, found)
	}
}

func TestModelAliasAndNorm(t *testing.T) {
	s := NewStore("")
	k, err := s.Add("a", "s1", "", []string{"global:deepseek-v4.1-flash"}, map[string]string{
		" deepseek-v4.1-flash ": " global:deepseek-v4.1-flash ",
		"same":                  "same", // 自映射 = 无操作 ⇒ 丢掉
		"":                      "x",    // 空键 ⇒ 丢掉
		"y":                     "  ",   // 空值 ⇒ 丢掉
	}, "", true)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(k.ModelMap) != 1 || k.ModelMap["deepseek-v4.1-flash"] != "global:deepseek-v4.1-flash" {
		t.Fatalf("别名映射未归一化：%+v", k.ModelMap)
	}
	real, hit := k.ModelAlias("deepseek-v4.1-flash")
	if !hit || real != "global:deepseek-v4.1-flash" {
		t.Fatalf("ModelAlias=%q,%v", real, hit)
	}
	if _, hit := k.ModelAlias("没配过的"); hit {
		t.Fatal("未配置的名字不应命中")
	}
	// 白名单按**真实模型名**判定（用户勾的就是它）；带域条目是精确匹配。
	if !k.AllowsModel("global", "deepseek-v4.1-flash") {
		t.Fatal("白名单应放行真实模型（global 域）")
	}
	if k.AllowsModel("cn", "deepseek-v4.1-flash") {
		t.Fatal("带域前缀的白名单条目不应放行 cn 域")
	}
}

func TestUpdateModelMap(t *testing.T) {
	s := NewStore("")
	k, _ := s.Add("a", "s1", "", nil, nil, "", true)
	mm := map[string]string{"x": "global:x"}
	got, err := s.Update(k.ID, Patch{ModelMap: &mm})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.ModelMap["x"] != "global:x" {
		t.Fatalf("Update 未写入别名：%+v", got.ModelMap)
	}
	// 传空 map ⇒ 清空（显式清空的语义；nil 指针才是"不改"）
	empty := map[string]string{}
	got2, err := s.Update(k.ID, Patch{ModelMap: &empty})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got2.ModelMap) != 0 {
		t.Fatalf("空 map 应清空别名：%+v", got2.ModelMap)
	}
	// nil 指针不动它
	got3, err := s.Update(k.ID, Patch{Name: strPtr("改名")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got3.ModelMap) != 0 {
		t.Fatalf("未传 ModelMap 不应改动：%+v", got3.ModelMap)
	}
}

func strPtr(s string) *string { return &s }

func TestThinkingSwitchNorm(t *testing.T) {
	s := NewStore("")
	k, err := s.Add("a", "s1", "", nil, nil, " OFF ", true)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if k.Thinking != "off" {
		t.Fatalf("Thinking 应归一化为 off，实际 %q", k.Thinking)
	}
	// 认不出的值退回"跟随客户端"，不报错（配置写错不该让整把密钥保存失败）
	if got := normThinking("maybe"); got != "" {
		t.Fatalf("未知值应归一化为空（跟随客户端），实际 %q", got)
	}
	for in, want := range map[string]string{
		"on": "on", "enabled": "on", "true": "on", "1": "on",
		"off": "off", "disabled": "off", "false": "off", "0": "off",
		"": "", "  ": "",
	} {
		if got := normThinking(in); got != want {
			t.Errorf("normThinking(%q)=%q want %q", in, got, want)
		}
	}
	// Update：传空串 = 显式恢复"跟随客户端"
	empty := ""
	got, err := s.Update(k.ID, Patch{Thinking: &empty})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Thinking != "" {
		t.Fatalf("传空串应恢复跟随客户端，实际 %q", got.Thinking)
	}
	// nil 指针不动它
	on := "on"
	if _, err := s.Update(k.ID, Patch{Thinking: &on}); err != nil {
		t.Fatal(err)
	}
	got3, err := s.Update(k.ID, Patch{Name: strPtr("改名")})
	if err != nil {
		t.Fatal(err)
	}
	if got3.Thinking != "on" {
		t.Fatalf("未传 Thinking 不应改动，实际 %q", got3.Thinking)
	}
}
