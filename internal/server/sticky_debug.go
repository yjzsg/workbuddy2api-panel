package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// 本文件服务于 WB2A_DBG_STICKY 粘性诊断开关（见 handler.go 粘性键计算处）。
//
// 为什么需要：中转层（New API 等）转发时不透传任何会话标识，粘性只能退到
// StickyFallbackKey（首条 user 消息哈希）。若中转层改写了首条消息（上下文压缩
// / 摘要重写 / 拼接），派生键每轮都变 → 同一会话被拆散到多个账号 → 上游前缀缓存
// 每条都是冷前缀（实测命中率 0.2%~5%，而粘住时 99.6%）。判定这一点需要看到
// "首条 user 文本"，但**不需要也不应该**落完整消息体。
//
// 隐私边界：stickyKey 只出 sha256 前 6 字节（48bit 指纹，不可逆）；
// 首条 user 只出前 40 字供人工比对；消息数/工具数只出数字。

// stickyFingerprint 返回粘性键的不可逆指纹（sha256 前 6 字节的十六进制）。
func stickyFingerprint(key string) string {
	if key == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// countMessages 返回 messages 数组长度（解析失败返回 -1）。
func countMessages(body []byte) int {
	var obj struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return -1
	}
	return len(obj.Messages)
}

// countTools 返回 tools 数组长度（无 tools 字段返回 0，解析失败返回 -1）。
// 工具数是判别"中转层是否剥离了 tools"的辅助指标（本仓 tool 修复层依赖它）。
func countTools(body []byte) int {
	var obj struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return -1
	}
	return len(obj.Tools)
}

// firstUserSnippet 返回首条 role=="user" 消息文本的前 40 字（单行化）。
// 取**首条** user（与 session.firstUserText 同语义，空内容不往后找——否则键会
// 随会话推进漂移）；差别仅在输出可读片段而非 sha256，供诊断人工比对。
func firstUserSnippet(body []byte) string {
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return "(parse-fail)"
	}
	for _, m := range obj.Messages {
		if m.Role != "user" {
			continue
		}
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			return snippet(s)
		}
		// content 非字符串（多模态数组形态）：取第一个 text 块。
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					return snippet(b.Text)
				}
			}
		}
		return "(非文本首条user)"
	}
	return "(无user消息)"
}

// snippet 单行化并截断到 40 字（按 rune 计，避免切碎多字节字符）。
func snippet(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", "|"), "\r", "")
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}
