package server

import "testing"

// TestResolveModel 覆盖前缀解析协议：
// 取第一个 ":"，前段为 cn/global 才剥离；裸名与非法前缀一律 realm=""（地域无关，
// 全池参与粘合，见 resolve_model.go 注释）。
func TestResolveModel(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"glm-5.2", "", "glm-5.2"}, // 裸名 → 全池
		{"deepseek-v4.1-flash", "", "deepseek-v4.1-flash"},
		{"deepseek:v3", "", "deepseek:v3"}, // 冒号前段不在枚举内，不剥离，按裸名（全池）
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveModelEdge 边界：空串、仅冒号、空前缀、大小写。
func TestResolveModelEdge(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"", "", ""},
		{":", "", ":"},
		{":model", "", ":model"},             // 空前缀不匹配 cn/global，不剥离
		{"GLOBAL:gpt-5", "", "GLOBAL:gpt-5"}, // 大小写敏感：不做归一 → 按裸名全池
		{"global:", "global", ""},            // 前缀合法 + 空裸名仍剥离
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"cn:", "cn", ""},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}
