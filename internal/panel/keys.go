package panel

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// keyRow 面板「密钥」页的一行：密钥元信息 + 该密钥在所选窗口内的用量。
//
// Secret 返回明文（面板本身已鉴权，且自托管场景用户需要复制）；前端默认打码
// 显示、点「显示」才展开——避免录屏/截屏直接泄露。
type keyRow struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Secret        string   `json:"secret"`
	Note          string   `json:"note,omitempty"`
	Enabled       bool     `json:"enabled"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	// ModelMap 别名映射：客户端用左边的名字调用，实际请求右边的模型（可带域前缀）。
	ModelMap map[string]string `json:"model_map,omitempty"`
	// Thinking 密钥级思考开关："" = 跟随客户端；"on" = 强制开；"off" = 强制关。
	Thinking  string `json:"thinking,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Migrated  bool   `json:"migrated_from_config,omitempty"`
	// Root 标记该密钥持有 config.json 的 api_key（"根密钥"）：不可停用/删除，
	// 前端据此隐藏这两个按钮（后端也有硬拦，见 apikeys.Store.Update/Delete）。
	Root bool `json:"root,omitempty"`
	// Orphan 标记"这把密钥不在表里"的行：引入多密钥之前的历史记录（ID 为空）
	// 或已被删除的密钥。只读展示，不可编辑。
	Orphan bool `json:"orphan,omitempty"`

	Usage   usage.Agg        `json:"usage"`
	ByModel []usage.KeyedAgg `json:"by_model,omitempty"`
}

// keysWindowHours 解析窗口参数：0/缺省 = 全部历史（密钥维度看总量更有意义）；
// 非法值按全部历史处理，不报错（面板是只读视图，宽容优于严格）。
func keysWindowHours(r *http.Request) int {
	v := strings.TrimSpace(r.URL.Query().Get("hours"))
	if v == "" {
		return 0
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 24*60 {
			return 24 * 60
		}
	}
	return n
}

// keys 列表：密钥表 + 每把密钥的用量（含"未标记/已删除"的孤儿行）。
func (p *Panel) keys(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_not_enabled")
		return
	}
	hours := keysWindowHours(r)

	known := map[string]bool{}
	rows := make([]keyRow, 0, p.cfg.Keys.Count()+1)
	for _, k := range p.cfg.Keys.List() {
		known[k.ID] = true
		rows = append(rows, keyRow{
			ID:            k.ID,
			Name:          k.Name,
			Secret:        k.Secret,
			Note:          k.Note,
			Enabled:       k.Enabled,
			AllowedModels: k.AllowedModels,
			ModelMap:      k.ModelMap,
			Thinking:      k.Thinking,
			CreatedAt:     k.CreatedAt,
			Migrated:      k.MigratedFromConfig,
			Root:          p.cfg.Keys.IsRoot(k),
		})
	}

	if p.cfg.Usage != nil {
		snap := p.cfg.Usage.Snapshot(hours, nil)
		byID := make(map[string]usage.KeyUsage, len(snap.ByKey))
		for _, ku := range snap.ByKey {
			byID[ku.ID] = ku
		}
		for i := range rows {
			if ku, ok := byID[rows[i].ID]; ok {
				rows[i].Usage = ku.Agg
				rows[i].ByModel = ku.ByModel
			}
		}
		// 孤儿行：有用量但不在当前表里（历史未标记 / 已删除的密钥）。
		// 必须展示——否则这些用量会在"按密钥"视图里凭空消失。
		for _, ku := range snap.ByKey {
			if known[ku.ID] {
				continue
			}
			name := "未标记（启用多密钥之前的历史记录）"
			if ku.ID != "" {
				name = "已删除的密钥 " + ku.ID
			}
			rows = append(rows, keyRow{
				ID:      ku.ID,
				Name:    name,
				Enabled: false,
				Orphan:  true,
				Usage:   ku.Agg,
				ByModel: ku.ByModel,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"keys":        rows,
		"hours":       hours,
		"total":       len(rows),
		"keys_file":   p.cfg.Keys.Path(),
		"window_note": "hours=0 表示全部历史",
	})
}

// keyInput 新增/修改的请求体。指针字段用于区分"没传"与"传了零值"。
type keyInput struct {
	Name          *string            `json:"name"`
	Secret        *string            `json:"secret"`
	Note          *string            `json:"note"`
	Enabled       *bool              `json:"enabled"`
	AllowedModels *[]string          `json:"allowed_models"`
	ModelMap      *map[string]string `json:"model_map"`
	Thinking      *string            `json:"thinking"`
}

func decodeKeyInput(w http.ResponseWriter, r *http.Request) (keyInput, error) {
	var in keyInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		return in, err
	}
	return in, nil
}

// keyAdd 新增密钥。secret 省略或空 ⇒ 服务端随机生成（返回体里带明文，仅此一次展示）。
func (p *Panel) keyAdd(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_not_enabled")
		return
	}
	in, err := decodeKeyInput(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json")
		return
	}
	name, secret, note := "", "", ""
	if in.Name != nil {
		name = *in.Name
	}
	if in.Secret != nil {
		secret = *in.Secret
	}
	if in.Note != nil {
		note = *in.Note
	}
	var allowed []string
	if in.AllowedModels != nil {
		allowed = *in.AllowedModels
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var mmap map[string]string
	if in.ModelMap != nil {
		mmap = *in.ModelMap
	}
	thinking := ""
	if in.Thinking != nil {
		thinking = *in.Thinking
	}
	key, err := p.cfg.Keys.Add(name, secret, note, allowed, mmap, thinking, enabled)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key})
}

// keyUpdate 局部修改一把密钥。
func (p *Panel) keyUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_not_enabled")
		return
	}
	id := r.PathValue("id")
	in, err := decodeKeyInput(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json")
		return
	}
	patch := apikeys.Patch{
		Name:          in.Name,
		Secret:        in.Secret,
		Note:          in.Note,
		Enabled:       in.Enabled,
		AllowedModels: in.AllowedModels,
		ModelMap:      in.ModelMap,
		Thinking:      in.Thinking,
	}
	key, err := p.cfg.Keys.Update(id, patch)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key})
}

// keyDelete 删除一把密钥。
func (p *Panel) keyDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_not_enabled")
		return
	}
	if err := p.cfg.Keys.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}
