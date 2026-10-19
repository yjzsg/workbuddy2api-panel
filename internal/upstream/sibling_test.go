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

func TestRewriteModelField(t *testing.T) {
	cases := []struct {
		name, in, from, to, want string
	}{
		{"紧凑格式", `{"model":"deepseek-v4.1-flash-sg","x":1}`, "deepseek-v4.1-flash-sg", "deepseek-v4.1-flash", `{"model":"deepseek-v4.1-flash","x":1}`},
		{"冒号后带空格", `{"model": "deepseek-v4.1-flash-sg"}`, "deepseek-v4.1-flash-sg", "deepseek-v4.1-flash", `{"model": "deepseek-v4.1-flash"}`},
		{"from 为空", `{"model":"a"}`, "", "b", `{"model":"a"}`},
		{"相同不处理", `{"model":"a"}`, "a", "a", `{"model":"a"}`},
		{"正文里同名不误伤", `{"content":"deepseek-v4.1-flash-sg 是备胎","model":"deepseek-v4.1-flash-sg"}`, "deepseek-v4.1-flash-sg", "deepseek-v4.1-flash", `{"content":"deepseek-v4.1-flash-sg 是备胎","model":"deepseek-v4.1-flash"}`},
	}
	for _, c := range cases {
		if got := rewriteModelField(c.in, c.from, c.to); got != c.want {
			t.Fatalf("%s: rewriteModelField(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
