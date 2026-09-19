package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// fakeScriptExec 记录命令构建参数并按需模拟执行失败，替代真实 exec 拉起 python3 子进程。
type fakeScriptExec struct {
	lastName string
	lastArgs []string
	lastDir  string
	runN     int
	err      error
}

func (f *fakeScriptExec) SetDir(dir string) { f.lastDir = dir }
func (f *fakeScriptExec) Run() error        { f.runN++; return f.err }

// installFakeExec 替换 newScriptCmd，测试结束还原。
func installFakeExec(t *testing.T) *fakeScriptExec {
	t.Helper()
	f := &fakeScriptExec{}
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		f.lastName, f.lastArgs = name, args
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
	return f
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNextWakeSchoolSlot 开学季任务在 school_hours（默认 12 点）处有独立时点。
func TestNextWakeSchoolSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school 12:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskSchool {
		t.Errorf("kinds=%v want [school]", kinds)
	}
}

// TestNextWakeCatSlot 夜猫子任务在 cat_hours（默认 1 点）处有独立时点。
func TestNextWakeCatSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatHours:          []int{1},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 15, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（cat 01:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCat {
		t.Errorf("kinds=%v want [cat]", kinds)
	}
}

// TestNextWakeSchoolCatDisabled 显式禁用 school/cat 后排程只剩签到时点（互不影响）。
func TestNextWakeSchoolCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		SchoolDisabled:    true,
		CatDisabled:       true,
		DailyChatDisabled: true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school/cat 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestRunSchoolNowBuildsCommand RunSchoolNow 构造
// python3 scripts/school_open_day_2026.py ALL --run --yes，工作目录设为仓库根。
func TestRunSchoolNowBuildsCommand(t *testing.T) {
	// 防环境泄漏：WB2A_PYTHON 若在测试机已设置会改写 pythonCmd()，使默认值断言失败。
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunSchoolNow()
	if f.lastName != "python3" {
		t.Errorf("name=%q want python3", f.lastName)
	}
	want := []string{"scripts/school_open_day_2026.py", "ALL", "--run", "--yes"}
	if !equalArgs(f.lastArgs, want) {
		t.Errorf("args=%v want %v", f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
	if _, err := os.Stat(filepath.Join(f.lastDir, "scripts", "school_open_day_2026.py")); err != nil {
		t.Errorf("仓库根 %q 内应有 scripts/school_open_day_2026.py: %v", f.lastDir, err)
	}
}

// TestRunCatNowBuildsCommand RunCatNow 构造
// python3 scripts/task_runner.py ALL --yes --only black_cat，工作目录为仓库根。
func TestRunCatNowBuildsCommand(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunCatNow()
	want := []string{"scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"}
	if f.lastName != "python3" || !equalArgs(f.lastArgs, want) {
		t.Errorf("cmd=%s %v want python3 %v", f.lastName, f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
}

// TestDispatchSchoolCatGoRoutes dispatch 把 school/cat 分发给面板 Go 路线
// （RunSchoolAllNow / RunBlackcatNow）；上游脚本版 RunSchoolNow/RunCatNow 保留
// 定义但不再被 dispatch 调用（面板镜像未携带 scripts/*.py）。
func TestDispatchSchoolCatGoRoutes(t *testing.T) {
	f := installFakeExec(t)
	s := New(Config{Pool: pool.New("")}) // 空池：Go 路线空转，零上游调用

	s.dispatch(context.Background(), taskSchool)
	s.dispatch(context.Background(), taskCat)
	if f.runN != 0 {
		t.Errorf("dispatch(school/cat) 不应再执行脚本: runN=%d last=%s %v", f.runN, f.lastName, f.lastArgs)
	}
}

// TestPythonCmd WB2A_PYTHON 覆盖解释器名：缺省/空白回落 "python3"（保持
// 容器与既有测试的行为不变），显式设置时取其值（Windows 等仅有 python 的环境）。
func TestPythonCmd(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("default pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "   ")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("blank pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "python")
	if got := pythonCmd(); got != "python" {
		t.Errorf("override pythonCmd()=%q want python", got)
	}

	t.Setenv("WB2A_PYTHON", "  /usr/bin/python3.10  ")
	if got := pythonCmd(); got != "/usr/bin/python3.10" {
		t.Errorf("trim pythonCmd()=%q want /usr/bin/python3.10", got)
	}
}
