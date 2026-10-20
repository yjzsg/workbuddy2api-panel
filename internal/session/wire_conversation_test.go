package session

import "testing"

// TestWireConversationID 线形会话 ID：客户端给的合法值原样透传，网关合成的
// `fb:` / `d-` 前缀标识转成 32 位 hex（UUID 形态）。
//
// 背景（2026-10-20 上游 issue #197/#231）：X-Conversation-ID 参与渠道校验，
// 把 `fb:<hex>` 这类合成值原样发出去等于自报「非官方客户端」。
func TestWireConversationID(t *testing.T) {
	same := []string{
		"0123456789abcdef0123456789abcdef",     // 32 hex
		"01234567-89ab-cdef-0123-456789abcdef", // 带横线 UUID 小写
		"01234567-89AB-CDEF-0123-456789ABCDEF", // 带横线 UUID 大写
		"conv-1",                               // 客户端自报的非标准 ID
		"session_abc",                          // 同上
	}
	for _, in := range same {
		if got := WireConversationID(in); got != in {
			t.Errorf("WireConversationID(%q) = %q, 应原样透传", in, got)
		}
	}

	synthetic := []string{
		"fb:0123456789abcdef0123456789abcdef",
		"d-abcdef0123456789abcdef0123456789",
	}
	for _, in := range synthetic {
		got := WireConversationID(in)
		if !isUUID32(got) {
			t.Fatalf("WireConversationID(%q) = %q, 应为 32 位小写 hex", in, got)
		}
		if again := WireConversationID(in); again != got {
			t.Fatalf("同一输入必须确定: %q vs %q", got, again)
		}
	}
	if WireConversationID(synthetic[0]) == WireConversationID(synthetic[1]) {
		t.Fatal("不同输入不应得到同一线形 ID")
	}
}

// TestWireConversationIDEmptyIsRandomUUID 无任何会话标识时生成一次性 UUID 形态值
// （等价官方客户端为新建会话生成 ID），且两次调用不同。
func TestWireConversationIDEmptyIsRandomUUID(t *testing.T) {
	a, b := WireConversationID(""), WireConversationID("")
	if !isUUID32(a) || !isUUID32(b) {
		t.Fatalf("空入参应返回 32 hex: %q / %q", a, b)
	}
	if a == b {
		t.Fatal("空入参两次调用应不同（一次性会话）")
	}
}

// TestWireConversationIDPassThroughKeepsCacheStable 原样透传分支必须等价于恒等，
// 否则 handler 侧「缓存键仍用 convID」的分离设计会被破坏。
func TestWireConversationIDPassThroughKeepsCacheStable(t *testing.T) {
	const in = "11111111222222223333333344444444"
	for i := 0; i < 3; i++ {
		if got := WireConversationID(in); got != in {
			t.Fatalf("第 %d 次透传结果变化: %q", i+1, got)
		}
	}
}
