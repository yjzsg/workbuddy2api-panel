package upstream

import "testing"

func TestSiblingModel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash-sg", true},
		{" deepseek-v4.1-flash ", "deepseek-v4.1-flash-sg", true},
		// 反向不兜底：显式选了 -sg 的客户端不该被悄悄换回 flash。
		{"deepseek-v4.1-flash-sg", "", false},
		{"glm-5.3", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := SiblingModel(c.in)
		if ok != c.ok || got != c.want {
			t.Fatalf("SiblingModel(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
