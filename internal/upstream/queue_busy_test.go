// queue_busy_test.go 排队类响应（6020 queue.waiting / 6021 queue.full）的判定与解析。
//
// 背景（2026-10-15 生产实测）：上游在容量吃紧时对 CN 号返 6021 queue.full，网关此前
// 零处理 → 落 status==429 兜底 → 账号级 600s 软冷却。一次请求轮转 6 个 CN 号各吃一次
// ⇒ 六个号全部账号级冷却 ⇒ 整个 CN 域（含 glm-5.3 / fast-model 等其他模型）一起死。
package upstream

import (
	"testing"
	"time"
)

func TestIsQueueBusy(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"6020 排队中（实测原文）", `{"code":6020,"msg":"queue.waiting.title","requestId":"15ce3266382ad7af8070bb6991652287","data":{"queue_position":526,"queue_size":526,"retry_after":40,"estimated_wait":2630,"client_user_type":"free"}}`, true},
		{"6021 队列满（实测原文）", `{"code":6021,"msg":"queue.full.title","requestId":"b870ff984ed4a6b78916b88893a6b7a2"}`, true},
		{"6021 带空格", `{"code": 6021, "msg": "queue.full.title"}`, true},
		{"6021 字符串码", `{"code":"6021"}`, true},
		{"6004 模型级限流不算排队", `{"code":6004,"msg":"您的使用量已超出频率限制"}`, false},
		{"14003 模型繁忙不算排队", `{"code":14003,"msg":"too many requests"}`, false},
		// 纪律：不做整段文本子串匹配——requestId 里恰好含 "6020" 不能误判。
		{"requestId 撞 6020 不误判", `{"code":10000,"msg":"invoke model error","requestId":"x6020x"}`, false},
		{"空 body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsQueueBusy(c.body); got != c.want {
				t.Fatalf("IsQueueBusy=%v want %v（body=%s）", got, c.want, c.body)
			}
		})
	}
}

func TestParseQueueRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
		ok   bool
	}{
		{"6020 实测带 retry_after:40", `{"code":6020,"data":{"queue_position":526,"retry_after":40,"client_user_type":"free"}}`, 40 * time.Second, true},
		{"6021 无 retry_after", `{"code":6021,"msg":"queue.full.title"}`, 0, false},
		{"retry_after 为 0（上游说立即）", `{"code":6020,"data":{"retry_after":0}}`, 0, false},
		{"retry_after 非数值", `{"code":6020,"data":{"retry_after":"soon"}}`, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseQueueRetryAfter(c.body)
			if ok != c.ok || got != c.want {
				t.Fatalf("ParseQueueRetryAfter=%v,%v want %v,%v", got, ok, c.want, c.ok)
			}
		})
	}
}
