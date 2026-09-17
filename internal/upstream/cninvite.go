// cninvite.go 邀请活动接口（面板层新增）：绑码 / 我的邀请码 / 邀请记录。
//
// 端点（CN 与 global **完全同名**，仅域名不同 —— 从活动页 chunk assets/events/invite-*.js 挖出）：
//
//	POST {webBase}/activity/workbuddy/invitation/v2/bind          {"inviteCode":"..."}
//	GET  {webBase}/activity/workbuddy/invitation/v2/my-code
//	GET  {webBase}/activity/workbuddy/invitation/v2/invite-records
//
// 业务码（2026-09-17 实测）：0=成功；12310=已绑过；12313=不能用自己的码（说明该号就是码主）；
// 12311=仅活动期内新注册用户可绑（老号被拒）；12301/12306/12314/12319=邀请码无效。
//
// ⚠️ 本文件**不用 c.doJSON**：doJSON 把 `code != 0` 当错误抛（&Error{Msg:"code=N msg=..."}），
// 而邀请接口的 12310/12313/12311 都是**正常业务态**（幂等/码主/老号），必须当结果读。
// 故这里自带 inviteRaw（原始 body + HTTP 状态），业务码由调用方 switch。
//
// ⚠️ my-code 字段名两侧不同：CN 返回 data.invite_code（snake_case），global 返回 data.inviteCode。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const inviteAPIPath = "/activity/workbuddy/invitation/v2"

// InviteBindResult 绑码结果（业务码 + 文案；非 0 也是"正常业务码"，不是 error）。
type InviteBindResult struct {
	Code    int
	Message string
}

// InviteFriend 邀请记录里的单个好友。
type InviteFriend struct {
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
	RegisteredAt string `json:"registered_at"`
	Status       string `json:"status"`
	Activated    bool   `json:"activated"`
	CreditsTags  []int  `json:"credits_tags"`
}

// InviteRecords 邀请记录汇总（用邀请人自己的 token 查）。
type InviteRecords struct {
	Friends      []InviteFriend `json:"friends"`
	TotalInvited int            `json:"total_invited"`
	TotalUsed    int            `json:"total_used"`
	TotalCredits int            `json:"total_credits"`
}

// inviteRaw 发一次邀请活动请求，返回原始 body 与 HTTP 状态。
// 只有传输错误 / HTTP >= 400 才算 error；业务码一律交给调用方解释。
func (c *Client) inviteRaw(a *auth.Auth, method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(raw)
	}
	base := c.webBase(a)
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return raw, resp.StatusCode, fmt.Errorf("invite %s: http %d: %s", path, resp.StatusCode, truncate(string(raw), 160))
	}
	return raw, resp.StatusCode, nil
}

// InviteBind 绑定邀请码（幂等；12310/12313/12311 都作为结果返回，不是 error）。
func (c *Client) InviteBind(a *auth.Auth, code string) (*InviteBindResult, error) {
	raw, _, err := c.inviteRaw(a, http.MethodPost, inviteAPIPath+"/bind", map[string]any{"inviteCode": code})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("invite bind 响应解析失败: %w", err)
	}
	msg := resp.Data.Message
	if msg == "" {
		msg = resp.Msg
	}
	return &InviteBindResult{Code: resp.Code, Message: msg}, nil
}

// InviteMyCode 查账号自己的邀请码（兼容 CN snake_case / global camelCase）。
func (c *Client) InviteMyCode(a *auth.Auth) (string, error) {
	raw, _, err := c.inviteRaw(a, http.MethodGet, inviteAPIPath+"/my-code", nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Snake string `json:"invite_code"`
			Camel string `json:"inviteCode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("my-code 响应解析失败: %w", err)
	}
	if resp.Data.Snake != "" {
		return resp.Data.Snake, nil
	}
	return resp.Data.Camel, nil
}

// InviteRecordsOf 查某账号的邀请记录（邀请人视角）。
func (c *Client) InviteRecordsOf(a *auth.Auth) (*InviteRecords, error) {
	raw, _, err := c.inviteRaw(a, http.MethodGet, inviteAPIPath+"/invite-records", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code int           `json:"code"`
		Data InviteRecords `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("invite-records 响应解析失败: %w", err)
	}
	return &resp.Data, nil
}
