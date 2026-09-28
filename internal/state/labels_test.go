package state

// 归属锚 label 契约的枚举守卫（IMPL-ARCH-C1）：锚常量块与谓词集合
// ownershipAnchorLabels 的脱节即红——F1 复发形态（声明地加了新锚常量、
// 兑现集合没跟上 → 携带新锚的对象被孤儿面误披露）在测试期拦下，不再等
// 运行期事故。源码扫描手法与 engine/labelscan_test.go 同款 idiom
// （runtime.Caller 定位 + CRLF 归一）。

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestIsOwnershipAnchorValueMatrix 谓词值矩阵（集合枚举 × 取值形态）：每个
// 锚 label 非空值 → 真；空值 → 假（空值 = 未落锚，不算归属事实）；无 label
// （nil/空 map）与非锚 label → 假。
func TestIsOwnershipAnchorValueMatrix(t *testing.T) {
	anchors := OwnershipAnchorLabels()
	if len(anchors) == 0 {
		t.Fatal("ownership anchor set is empty (contract regression)")
	}
	for _, key := range anchors {
		if !IsOwnershipAnchor(map[string]string{key: "anchor-value"}) {
			t.Errorf("IsOwnershipAnchor(map{%q: non-empty}) = false, want true", key)
		}
		if IsOwnershipAnchor(map[string]string{key: ""}) {
			t.Errorf("IsOwnershipAnchor(map{%q: \"\"}) = true, want false (an empty value is not an attribution fact)", key)
		}
	}
	if IsOwnershipAnchor(nil) {
		t.Error("IsOwnershipAnchor(nil) = true, want false")
	}
	if IsOwnershipAnchor(map[string]string{}) {
		t.Error("IsOwnershipAnchor(empty map) = true, want false")
	}
	if IsOwnershipAnchor(map[string]string{LabelManaged: ManagedLabelValue}) {
		t.Error("IsOwnershipAnchor with only the non-anchor managed label = true, want false")
	}
}

// TestOwnershipAnchorConstantsCoveredByPredicate 锚常量块 ↔ 谓词集合双向
// 钉死：labels.go 归属锚 const 块里声明的每个常量都必须进
// ownershipAnchorLabels（缺席 = F1 复发形态，本测试红）；集合条目也必须是
// 块内声明的常量（反向防手误拼错/悬空条目）。红态演示：往锚常量块加
// LabelDrillAnchor 而不加集合条目 → 本测试红。
func TestOwnershipAnchorConstantsCoveredByPredicate(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "labels.go"))
	if err != nil {
		t.Fatalf("read labels.go: %v", err)
	}
	// 行尾归一到 LF：Windows 检出（core.autocrlf=true）把磁盘文件写成
	// CRLF——行尾敏感判定不归一则结果随检出环境漂移（labelscan 同款教训）。
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(src, "\n")

	// 定位归属锚 const 块：LabelProjectNetwork 声明行所在块（锚块与集合/
	// 谓词同文件相邻声明是本票的结构约定，块移位即在此显式失败）。
	anchorDecl := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "\tLabelProjectNetwork = ") {
			anchorDecl = i
			break
		}
	}
	if anchorDecl < 0 {
		t.Fatal("LabelProjectNetwork declaration not found in labels.go (anchor block moved?)")
	}
	blockStart, blockEnd := -1, -1
	for i := anchorDecl; i >= 0; i-- {
		if lines[i] == "const (" {
			blockStart = i
			break
		}
	}
	for i := anchorDecl; i < len(lines); i++ {
		if lines[i] == ")" {
			blockEnd = i
			break
		}
	}
	if blockStart < 0 || blockEnd < 0 || blockStart >= anchorDecl || blockEnd <= anchorDecl {
		t.Fatalf("anchor const block bounds not found (start=%d end=%d decl=%d)", blockStart, blockEnd, anchorDecl)
	}
	declEntry := regexp.MustCompile(`^\t([A-Za-z][A-Za-z0-9]*) = "([^"]*)"$`)
	declared := map[string]string{} // label 值 → 常量名
	for _, line := range lines[blockStart+1 : blockEnd] {
		if m := declEntry.FindStringSubmatch(line); m != nil {
			declared[m[2]] = m[1]
		}
	}
	if len(declared) < 2 {
		t.Fatalf("anchor const block parsed to %d constants, want at least the two contract anchors", len(declared))
	}

	inSet := map[string]bool{}
	for _, key := range ownershipAnchorLabels {
		if inSet[key] {
			t.Errorf("ownershipAnchorLabels contains duplicate entry %q", key)
			continue
		}
		inSet[key] = true
		if _, ok := declared[key]; !ok {
			t.Errorf("ownershipAnchorLabels entry %q is not declared in the anchor const block of labels.go (typo or stale entry)", key)
		}
	}
	for value, name := range declared {
		if !inSet[value] {
			t.Errorf("anchor constant %s = %q is declared in labels.go but missing from ownershipAnchorLabels — the declared contract and the predicate set have diverged (F1 relapse form: objects carrying this anchor would be disclosed as orphans)", name, value)
		}
	}
}

// TestOwnershipAnchorLabelsReturnsCopy 枚举读口的副本语义：调用方改写返回
// 切片不得污染集合本体（识别面只读纪律）。
func TestOwnershipAnchorLabelsReturnsCopy(t *testing.T) {
	anchors := OwnershipAnchorLabels()
	if len(anchors) == 0 {
		t.Fatal("ownership anchor set is empty (contract regression)")
	}
	anchors[0] = "fleetly.tampered"
	if OwnershipAnchorLabels()[0] == "fleetly.tampered" {
		t.Fatal("OwnershipAnchorLabels leaked the internal set (mutation visible across calls)")
	}
}
