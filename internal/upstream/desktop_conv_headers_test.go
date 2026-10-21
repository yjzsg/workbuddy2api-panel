package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// hex32 断言用：32 位小写 hex（会话头族的形状要求，B3 规范亦只认 16/32 hex）。
var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestDesktopChatInjectsConversationHeaderFamily 回归 2026-10-21 的生产故障：
// global 账号的每日对话保底任务全部 400 code=11-128「请求被拦截，请重新发送」。
//
// 根因（NAS 上对同一 global 号做的 A/B 实测）：desktopChat 手工拼头，绕过了
// ChatHeaders → injectConversationHeaders，出站缺整族会话头；global 上游对
// POST /v2/chat/completions 校验这一族头，缺任一子集即 11-128。
//
//	V0 现状（合成 conv id，无头族）         → 400
//	V1 只把 conv id 换成 UUID               → 400
//	V2 UUID + 整族头                        → 200
//	V3 完全不发 X-Conversation-ID           → 400
//	W1 合成 conv id + 整族头                → 200（故 conv id 形态不需要改）
//	W2..W5 只补单个头（Request-ID / Message-ID / B3 / Root）→ 全 400
//
// 本用例锁住「整族齐全且自洽」这一点，避免以后再被手工拼头绕过去。
func TestDesktopChatInjectsConversationHeaderFamily(t *testing.T) {
	cases := []struct {
		name   string
		realm  string
		global bool
	}{
		{"global", "global", true},
		{"cn", "cn", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, "data: {\"id\":\"0123456789abcdef0123456789abcdef\",\"choices\":[]}\n\n")
			}))
			defer srv.Close()

			a := &auth.Auth{UID: "uid-test-1", AccessToken: "tok-test"}
			if _, err := auth.BackfillRealmFor(a, tc.realm); err != nil {
				t.Fatalf("BackfillRealmFor(%q): %v", tc.realm, err)
			}
			c := &Client{
				HTTP:           srv.Client(),
				ChatBaseCN:     srv.URL,
				ChatBaseGlobal: srv.URL,
				GlobalEnabled:  true,
			}

			convID, reqID, err := c.desktopChat(a, "deepseek-v4.1-flash", "", false)
			if err != nil {
				t.Fatalf("desktopChat: %v", err)
			}
			if !strings.HasPrefix(convID, "wb2api-conv-") {
				t.Fatalf("conversationID 形态变了：%q（专家链路按它关联事件，勿改）", convID)
			}

			// 会话头族：四层各自到位，且同一轮的标识互相自洽。
			if got.Get("X-Conversation-ID") != convID {
				t.Errorf("X-Conversation-ID = %q, want %q", got.Get("X-Conversation-ID"), convID)
			}
			convReqID := got.Get("X-Conversation-Request-ID")
			if !hex32.MatchString(convReqID) {
				t.Errorf("X-Conversation-Request-ID = %q, want 32 hex（轮级聚合主键，必发）", convReqID)
			}
			msgID := got.Get("X-Conversation-Message-ID")
			if !hex32.MatchString(msgID) {
				t.Errorf("X-Conversation-Message-ID = %q, want 32 hex", msgID)
			}
			if got.Get("X-Request-ID") != msgID {
				t.Errorf("X-Request-ID = %q, want 等于 messageID %q", got.Get("X-Request-ID"), msgID)
			}
			if got.Get("X-Root-Request-ID") != convReqID {
				t.Errorf("X-Root-Request-ID = %q, want 等于 conversationRequestID %q", got.Get("X-Root-Request-ID"), convReqID)
			}
			if got.Get("X-Trace-ID") != convReqID {
				t.Errorf("X-Trace-ID = %q, want 等于 conversationRequestID %q", got.Get("X-Trace-ID"), convReqID)
			}
			if got.Get("X-B3-TraceId") != convReqID {
				t.Errorf("X-B3-TraceId = %q, want %q", got.Get("X-B3-TraceId"), convReqID)
			}
			if got.Get("X-B3-SpanId") != msgID[:16] {
				t.Errorf("X-B3-SpanId = %q, want messageID[:16] %q", got.Get("X-B3-SpanId"), msgID[:16])
			}
			if got.Get("X-B3-Sampled") != "1" {
				t.Errorf("X-B3-Sampled = %q, want 1", got.Get("X-B3-Sampled"))
			}

			// 桌面指纹覆盖：ChatHeaders 之后仍以桌面实测值收口。
			if got.Get("User-Agent") != desktopUA {
				t.Errorf("User-Agent = %q, want %q", got.Get("User-Agent"), desktopUA)
			}
			if got.Get("X-Domain") != srv.URL {
				t.Errorf("X-Domain = %q, want %q", got.Get("X-Domain"), srv.URL)
			}
			if got.Get("X-Product") != "SaaS" {
				t.Errorf("X-Product = %q, want SaaS", got.Get("X-Product"))
			}
			if got.Get("X-User-Id") != "uid-test-1" {
				t.Errorf("X-User-Id = %q, want uid-test-1", got.Get("X-User-Id"))
			}
			if got.Get("Authorization") != "Bearer tok-test" {
				t.Errorf("Authorization = %q", got.Get("Authorization"))
			}
			// realm 分支头：global 声明无企业 ID，cn 声明无部门信息（均不回退登录域）。
			if tc.global {
				if got.Get("X-No-Enterprise-Id") != "1" {
					t.Errorf("global 缺 X-No-Enterprise-Id=1")
				}
			} else if got.Get("X-No-Department-Info") != "1" {
				t.Errorf("cn 缺 X-No-Department-Info=1")
			}
			_ = reqID
		})
	}
}
