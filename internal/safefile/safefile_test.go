package safefile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteCreatesFileAndLeavesNoTemp 落盘后文件内容正确、目录里不留临时文件。
func TestWriteCreatesFileAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := Write(p, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("内容 = %q", got)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("目录里应只有目标文件，实际 %v", names)
	}
}

// TestWriteKeepKeepsPreviousAsBak 第二次写入把上一份有效内容留成 .bak。
func TestWriteKeepKeepsPreviousAsBak(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := WriteKeep(p, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatalf("WriteKeep#1: %v", err)
	}
	if _, err := os.Stat(Bak(p)); !os.IsNotExist(err) {
		t.Fatalf("首次写入不该产生 .bak: %v", err)
	}
	if err := WriteKeep(p, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatalf("WriteKeep#2: %v", err)
	}
	bak, err := os.ReadFile(Bak(p))
	if err != nil {
		t.Fatalf("读 .bak: %v", err)
	}
	if string(bak) != `{"v":1}` {
		t.Fatalf(".bak = %q，期望上一份内容", bak)
	}
	cur, _ := os.ReadFile(p)
	if string(cur) != `{"v":2}` {
		t.Fatalf("正文 = %q", cur)
	}
}

// TestReadFallsBackToBak 正文被截断（掉电典型现场）时回退 .bak；坏内容另存 .bad-*。
func TestReadFallsBackToBak(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := WriteKeep(p, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatalf("WriteKeep#1: %v", err)
	}
	if err := WriteKeep(p, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatalf("WriteKeep#2: %v", err)
	}
	if err := os.WriteFile(p, make([]byte, 32), 0o600); err != nil { // 全 0，模拟掉电
		t.Fatalf("写坏文件: %v", err)
	}
	raw, err := Read(p, nil)
	if err != nil {
		t.Fatalf("Read 应回退 .bak: %v", err)
	}
	if string(raw) != `{"v":1}` {
		t.Fatalf("回退内容 = %q", raw)
	}
	// 坏内容不能被拿来污染 .bak，而要另存 .bad-<时间>。
	if err := WriteKeep(p, []byte(`{"v":3}`), 0o600); err != nil {
		t.Fatalf("WriteKeep#3: %v", err)
	}
	bak, _ := os.ReadFile(Bak(p))
	if string(bak) != `{"v":1}` {
		t.Fatalf(".bak 被坏内容污染: %q", bak)
	}
	bad, _ := filepath.Glob(p + ".bad-*")
	if len(bad) != 1 {
		t.Fatalf("应另存一个 .bad-*，实际 %v", bad)
	}
}

// TestReadMissingIsNotExist 文件不存在返回 fs.ErrNotExist，且不去 .bak 复活。
func TestReadMissingIsNotExist(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := Write(Bak(p), []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatalf("写 .bak: %v", err)
	}
	if _, err := Read(p, nil); !os.IsNotExist(err) {
		t.Fatalf("文件缺失应返回 ErrNotExist（不复活 .bak），实际 %v", err)
	}
}

// TestReadCustomValid 自定义校验器生效（auth 文件不是纯 JSON 形状校验的场景）。
func TestReadCustomValid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	if err := Write(p, []byte("not json"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := Read(p, func(b []byte) bool { return json.Valid(b) }); err == nil {
		t.Fatal("坏内容且无 .bak 应报错")
	}
	if _, err := Read(p, func(b []byte) bool { return len(b) > 0 }); err != nil {
		t.Fatalf("自定义校验通过时不该报错: %v", err)
	}
}
