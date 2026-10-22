package session

import (
	"encoding/hex"
	"regexp"
	"strconv"
	"testing"
	"time"
)

var (
	reHex32   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reHex16   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	reUUIDV4  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	tolerance = 5 * time.Second
)

// TestNewMessageIDHasMillisPrefix 32 hex 且前 12 hex 解出来是当前毫秒时间戳
// （官方 X-Request-ID 形态：时间位在最前，便于同账号请求按时间排序）。
func TestNewMessageIDHasMillisPrefix(t *testing.T) {
	before := time.Now()
	id := NewMessageID()
	after := time.Now()
	if !reHex32.MatchString(id) {
		t.Fatalf("不是 32 hex: %q", id)
	}
	ms, err := strconv.ParseInt(id[:12], 16, 64)
	if err != nil {
		t.Fatalf("前 12 hex 不是数字: %v", err)
	}
	got := time.UnixMilli(ms)
	if got.Before(before.Add(-tolerance)) || got.After(after.Add(tolerance)) {
		t.Fatalf("时间前缀 %v 不在 [%v, %v] 内", got, before, after)
	}
}

// TestIDShapes 会话/链路 ID 形态：conv 是 UUID v4，trace 32 hex，span 16 hex。
func TestIDShapes(t *testing.T) {
	if id := NewConversationID(); !reUUIDV4.MatchString(id) {
		t.Errorf("NewConversationID = %q，期望 UUID v4 带横线", id)
	}
	trace := NewTraceID()
	if !reHex32.MatchString(trace) {
		t.Errorf("NewTraceID = %q，期望 32 hex", trace)
	}
	span := NewSpanID()
	if !reHex16.MatchString(span) {
		t.Errorf("NewSpanID = %q，期望 16 hex", span)
	}
	// trace/span 不带时间位：不能与 messageID 的毫秒前缀形态撞车。
	if ms, err := strconv.ParseInt(trace[:12], 16, 64); err == nil {
		got := time.UnixMilli(ms)
		now := time.Now()
		if !got.Before(now.Add(-time.Hour)) && !got.After(now.Add(time.Hour)) {
			t.Errorf("NewTraceID 前 12 hex 解出接近当前时间（%.0f），疑似掺了时间位", got.Sub(now).Seconds())
		}
	}
}

// TestNewIDsUnique 连发 1000 个不重复（熵源与时间位组合不撞）。
func TestNewIDsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewMessageID()
		if _, dup := seen[id]; dup {
			t.Fatalf("NewMessageID 重复: %q", id)
		}
		seen[id] = struct{}{}
		if raw, err := hex.DecodeString(id); err != nil || len(raw) != 16 {
			t.Fatalf("解码失败: %q", id)
		}
	}
}

// TestDerivedIDsStayStable 轮级/会话级聚合键必须纯派生（同一输入恒等）——
// 掺时间位会打散上游按 request-id 的聚合与缓存。
func TestDerivedIDsStayStable(t *testing.T) {
	if a, b := RequestIDForKey("k1"), RequestIDForKey("k1"); a != b {
		t.Fatalf("RequestIDForKey 不稳定: %q vs %q", a, b)
	}
	if a, b := TurnRequestID("turn-1"), TurnRequestID("turn-1"); a != b {
		t.Fatalf("TurnRequestID 不稳定: %q vs %q", a, b)
	}
	if RequestIDForKey("k1") == RequestIDForKey("k2") {
		t.Fatal("不同键派生出同一 ID")
	}
}
