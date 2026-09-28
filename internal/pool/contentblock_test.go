package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNoteContentBlockEvidenceThresholdNotReached：未达阈值的证据不禁用，但给软冷却
// 让位（让健康号顶上，同时不把一次偶发证据放大成长时间出池）。
func TestNoteContentBlockEvidenceThresholdNotReached(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 1; i < contentBlockThreshold; i++ {
		if p.NoteContentBlockEvidence("u1") {
			t.Fatalf("第 %d 次证据不应禁用（阈值 %d）", i, contentBlockThreshold)
		}
		st, ok := p.Status("u1")
		if !ok {
			t.Fatal("no status")
		}
		if st.Disabled {
			t.Fatalf("第 %d 次证据后不应 disabled: %+v", i, st)
		}
		if !st.Cooling {
			t.Fatalf("第 %d 次证据应给软冷却让位（否则死号会被反复选中）: %+v", i, st)
		}
	}
}

// TestNoteContentBlockEvidenceDisablesAtThreshold：达到阈值即 Disable——账号在上游
// 已被拒，留在池里只会被反复选中、白打上游、并让客户端多轮转几次。
func TestNoteContentBlockEvidenceDisablesAtThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 1; i < contentBlockThreshold; i++ {
		p.NoteContentBlockEvidence("u1")
	}
	if !p.NoteContentBlockEvidence("u1") {
		t.Fatalf("第 %d 次证据应禁用", contentBlockThreshold)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("达阈值后应 disabled: %+v", st)
	}
	if st.Reason != contentBlockReason {
		t.Errorf("reason=%q want %q", st.Reason, contentBlockReason)
	}
	// 禁用后不参与选号（池里只剩它 → 无号可选）。
	if got := p.Pick(""); got != nil {
		t.Errorf("disabled 号不应被选中, got %+v", got)
	}
}

// TestNoteContentBlockEvidenceClearedOnSuccess：成功是「该号没被上游拒」的直接证据，
// 必须清零——否则历史证据跨成功累积，最终误禁健康号。
func TestNoteContentBlockEvidenceClearedOnSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteContentBlockEvidence("u1")
	p.NoteContentBlockEvidence("u1")
	p.NoteSuccess("u1")
	if p.NoteContentBlockEvidence("u1") {
		t.Fatal("成功清零后第 1 次证据不应禁用（历史证据必须被成功清掉）")
	}
}

// TestReviveDisabledClearsContentBlock：人工复活清计数，账号回到「零证据」状态。
func TestReviveDisabledClearsContentBlock(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < contentBlockThreshold; i++ {
		p.NoteContentBlockEvidence("u1")
	}
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("达阈值后应已禁用")
	}
	if !p.ReviveDisabled("u1") {
		t.Fatal("revive 应成功")
	}
	if p.NoteContentBlockEvidence("u1") {
		t.Fatal("复活后计数应已清零（第 1 次证据不应禁用）")
	}
}

// TestNoteContentBlockEvidenceUnknownUID：未知 uid 为空操作，不 panic。
func TestNoteContentBlockEvidenceUnknownUID(t *testing.T) {
	p := New("")
	if p.NoteContentBlockEvidence("nope") {
		t.Fatal("未知 uid 不应返回 true")
	}
	p.ClearContentBlock("nope") // 不 panic
}
