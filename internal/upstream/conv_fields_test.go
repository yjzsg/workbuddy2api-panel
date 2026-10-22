package upstream

import (
	"encoding/json"
	"testing"
)

// lastUserMsg 取解析后最后一条 user 消息，供断言。
func lastUserMsg(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("解析出站 body: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "user" {
			return m
		}
	}
	t.Fatal("没有 user 消息")
	return nil
}

// TestInjectConversationFieldsLastUser 给最后一条 user 补三个字段，历史 user 不动。
func TestInjectConversationFieldsLastUser(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"s"},` +
		`{"role":"user","content":"old"},` +
		`{"role":"assistant","content":"a"},` +
		`{"role":"user","content":"cur"}]}`)
	out := injectConversationFields(body, "req-123")
	cur := lastUserMsg(t, out)
	if cur["conversationRequestId"] != "req-123" {
		t.Fatalf("conversationRequestId = %v", cur["conversationRequestId"])
	}
	if cur["startsNewUserRequest"] != true {
		t.Fatalf("startsNewUserRequest = %v", cur["startsNewUserRequest"])
	}
	if cur["agent"] != "cli" {
		t.Fatalf("agent = %v", cur["agent"])
	}
	// 历史消息不该被写：只认当前轮（不编造过去的轮级 ID）。
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	old := msgs[1].(map[string]any)
	if _, has := old["conversationRequestId"]; has {
		t.Fatal("历史 user 消息被写入了 conversationRequestId")
	}
}

// TestInjectConversationFieldsKeepsClientValue 客户端自带时原值优先。
func TestInjectConversationFieldsKeepsClientValue(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"x","conversationRequestId":"client-req","agent":"desktop"}]}`)
	out := injectConversationFields(body, "gw-req")
	cur := lastUserMsg(t, out)
	if cur["conversationRequestId"] != "client-req" {
		t.Fatalf("覆盖了客户端自带值: %v", cur["conversationRequestId"])
	}
	if cur["agent"] != "desktop" {
		t.Fatalf("覆盖了客户端自带 agent: %v", cur["agent"])
	}
}

// TestInjectConversationFieldsNoop 空 ID / 坏 JSON / 无 user 消息一律原样返回。
func TestInjectConversationFieldsNoop(t *testing.T) {
	cases := []struct {
		name string
		body string
		id   string
	}{
		{"空 ID", `{"messages":[{"role":"user","content":"x"}]}`, ""},
		{"坏 JSON", `{not json`, "req"},
		{"无 messages", `{"model":"m"}`, "req"},
		{"只有 assistant", `{"messages":[{"role":"assistant","content":"x"}]}`, "req"},
	}
	for _, c := range cases {
		got := injectConversationFields([]byte(c.body), c.id)
		if string(got) != c.body {
			t.Errorf("%s: body 被改动\n got=%s\nwant=%s", c.name, got, c.body)
		}
	}
}
