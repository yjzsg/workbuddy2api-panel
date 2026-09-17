// client_token.go 前端同款幂等令牌（randomUUID 语义）。[面板层抽取]
//
// 原住面板的 streak.go；面板的 school.go（开学季抽奖 draw_uuid、请求 ID）
// 也在用，故在同步上游时抽成独立文件保留。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// clientToken 幂等令牌（前端 randomUUID 同款语义）。
func clientToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}
