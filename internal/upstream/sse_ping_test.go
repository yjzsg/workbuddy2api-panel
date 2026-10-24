package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowReader 按 chunk 顺序吐字节，chunk 之间可插入静默（模拟上游长时间思考、
// 一个字节都不发）。stall[i] 是第 i 个 chunk 与下一个之间的静默时长。
type slowReader struct {
	chunks []string
	stall  []time.Duration
	i      int
	stalls atomic.Int64
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.i >= len(s.chunks) {
		return 0, io.EOF
	}
	if s.i > 0 && s.i-1 < len(s.stall) && s.stall[s.i-1] > 0 {
		s.stalls.Add(1)
		time.Sleep(s.stall[s.i-1])
	}
	n := copy(p, s.chunks[s.i])
	if n < len(s.chunks[s.i]) {
		s.chunks[s.i] = s.chunks[s.i][n:]
		return n, nil
	}
	s.i++
	return n, nil
}

// TestStreamHintSendsPingDuringUpstreamSilence 锁定「上游静默时网关自己发保活
// 注释帧」的行为：Cloudflare 橙云对空闲连接约 100s 就掐断，而实测上游在开始吐
// 数据前只发一帧 `: heartbeat`，长思考期会长时间零字节（2026-10-24 三次被截断
// 的 kimi 请求都符合这个形态）。
func TestStreamHintSendsPingDuringUpstreamSilence(t *testing.T) {
	body := &slowReader{
		chunks: []string{
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"}}]}\n\n",
			"data: [DONE]\n\n",
		},
		stall: []time.Duration{600 * time.Millisecond, 600 * time.Millisecond, 0},
	}
	w := httptest.NewRecorder()

	start := time.Now()
	if err := StreamHint(w, body, nil, WithPingInterval(150*time.Millisecond)); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("用例没真正制造静默（%s），断言无意义", elapsed)
	}

	out := w.Body.String()
	if got := strings.Count(out, ": ping\n\n"); got < 2 {
		t.Fatalf("静默期间保活帧数=%d，期望 >=2；输出=%q", got, out)
	}
	// 保活不得吃掉正文帧，也不得重复 [DONE]。
	for _, want := range []string{`"content":"a"`, `"content":"b"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("正文帧丢失 %s；输出=%q", want, out)
		}
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] 出现 %d 次，期望恰好 1 次；输出=%q", got, out)
	}
}

// TestStreamHintPingKeepsFrameOrder 确认保活帧插在数据帧之间、不破坏 SSE 结构：
// 每个 ping 都是独立的注释行（以 ":" 开头 + 空行结束），客户端 scanner 会跳过。
func TestStreamHintPingKeepsFrameOrder(t *testing.T) {
	body := &slowReader{
		chunks: []string{
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n",
			"data: [DONE]\n\n",
		},
		stall: []time.Duration{800 * time.Millisecond, 0},
	}
	w := httptest.NewRecorder()
	if err := StreamHint(w, body, nil, WithPingInterval(100*time.Millisecond)); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	out := w.Body.String()
	idxPing := strings.Index(out, ": ping\n\n")
	idxData := strings.Index(out, `"content":"x"`)
	if idxPing < 0 || idxData < 0 {
		t.Fatalf("缺帧：ping=%d data=%d 输出=%q", idxPing, idxData, out)
	}
	if idxPing < idxData {
		t.Fatalf("保活帧应出现在数据帧之后的静默里；输出=%q", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("流应以 [DONE] 结尾；输出=%q", out)
	}
}
