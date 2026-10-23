package panel

import (
	"os"
	"strings"
	"testing"
)

// TestExpertExperienceSpecs 四条「专家体验」任务的固定参数必须完整、且 id 与任务
// jump_url 一致（2026-10-23 用 CN 账号实测核对）。
func TestExpertExperienceSpecs(t *testing.T) {
	want := map[string]string{
		"expert_use_maidehao":           "ex_GDl31WahyIH0",
		"expert_use_douyinyin":          "ex_FbY5nlWTqDod",
		"expert_use_shangjia_xuanchuan": "ex_Weo3x2EZuSDm",
		"expert_use_bowangwang":         "ex_WZY136wV5aMt",
	}
	if len(expertExperienceTasks) != len(want) {
		t.Fatalf("任务数 = %d, want %d", len(expertExperienceTasks), len(want))
	}
	for code, id := range want {
		spec, ok := expertSpecFor(code)
		if !ok {
			t.Fatalf("%s 未登记", code)
		}
		if spec.ExpertID != id {
			t.Errorf("%s 的 expert_id = %q, want %q", code, spec.ExpertID, id)
		}
		// 昵称与职业名都参与服务端校验，任一为空都会导致事件被判无效。
		if spec.DisplayNameZH == "" || spec.ProfessionZH == "" {
			t.Errorf("%s 缺名称: display=%q profession=%q", code, spec.DisplayNameZH, spec.ProfessionZH)
		}
		if spec.ExpertType != "agent" {
			t.Errorf("%s 的 expert_type = %q, want agent", code, spec.ExpertType)
		}
	}
	if _, ok := expertSpecFor("expert_use_nope"); ok {
		t.Fatal("未登记的任务码不应命中")
	}
}

// TestAutoActionsContainNewExpertTasks 面板按钮与后端动作表必须同步——否则任务中心
// 不渲染「一键完成」按钮（上游 TestAutoActionsSyncWithFrontend 的同款兜底）。
func TestAutoActionsContainNewExpertTasks(t *testing.T) {
	codes := []string{
		"WorkBuddy_Valorant",
		"expert_use_maidehao",
		"expert_use_douyinyin",
		"expert_use_shangjia_xuanchuan",
		"expert_use_bowangwang",
	}
	have := map[string]bool{}
	for _, a := range autoActions {
		have[a.TaskCode] = true
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	for _, code := range codes {
		if !have[code] {
			t.Errorf("autoActions 缺 %s", code)
		}
		if !strings.Contains(string(js), "'"+code+"':") {
			t.Errorf("app.js 的 AUTO_TASKS 缺 %s", code)
		}
	}
}
