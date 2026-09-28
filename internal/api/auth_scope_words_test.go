package api

// scope 词表的守卫测试（IMPL-ARCH-I；新增文件——既有测试断言零修改）：
// 词表收敛为 scopeWords 单点声明后，本文件钉住三件事——
//  1. 表 ⇆ 导出常量双向覆盖（加常量漏加表行、或表行残留常量已删，都红）；
//  2. 蕴含形状（impliesAll 仅 admin；其余词蕴含自身 = 显式授予语义；
//     admin 开放集语义——对表内一切词都满足）；
//  3. scopeSetToList 的固定词表序（Principal.Scopes 存储形态的规范序，
//     与收编前手写排序清单逐位一致）。
// containsScope 的语义矩阵本身由既有 TestScopeContainment（auth_test.go）
// 与 tasks/build 面的独立蕴含断言钉住，此处不重复。

import "testing"

func TestScopeWordTableCoversConstants(t *testing.T) {
	constants := []string{ScopeRead, ScopeDeploy, ScopeAdmin, ScopeTerminal, ScopeTasks, ScopeBuild}
	table := map[string]int{} // 词 → 出现次数
	for _, spec := range scopeWords {
		table[spec.word]++
	}
	for _, c := range constants {
		if table[c] != 1 {
			t.Fatalf("scope constant %q appears %d times in scopeWords, want exactly once (add the word to the table)", c, table[c])
		}
	}
	for word := range table {
		found := false
		for _, c := range constants {
			if c == word {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("scopeWords contains %q which is not an exported scope constant (stale table row)", word)
		}
	}
}

func TestScopeWordTableImplicationShape(t *testing.T) {
	words := make([]string, 0, len(scopeWords))
	for _, spec := range scopeWords {
		words = append(words, spec.word)
		if spec.impliesAll && spec.word != ScopeAdmin {
			t.Fatalf("scope word %q declares impliesAll — only admin carries the open-set semantics", spec.word)
		}
		if !spec.impliesAll {
			self := false
			for _, implied := range spec.implies {
				if implied == spec.word {
					self = true
					break
				}
			}
			if !self {
				t.Fatalf("scope word %q does not imply itself (explicit grant must satisfy its own scope)", spec.word)
			}
		}
	}
	// admin 开放集语义：对一切词（含 admin 自身）恒满足——与旧 switch 的
	// 「case ScopeAdmin: return true」逐字同语义（未来新增词无需改 admin
	// 条目）。
	for _, need := range words {
		if !containsScope(ScopeAdmin, need) {
			t.Fatalf("admin must imply every scope word, but containsScope(admin, %q) = false", need)
		}
	}
	// 非 admin 词不满足 admin 需求（fail-closed：admin 需求仅 admin 满足）。
	for _, spec := range scopeWords {
		if spec.word == ScopeAdmin {
			continue
		}
		if containsScope(spec.word, ScopeAdmin) {
			t.Fatalf("scope word %q must not imply admin", spec.word)
		}
	}
}

func TestScopeSetToListFollowsTableOrder(t *testing.T) {
	// 全集投影 = 固定词表序逐位一致（收编前手写排序清单的快照序）。
	full := map[string]bool{}
	for _, spec := range scopeWords {
		full[spec.word] = true
	}
	got := scopeSetToList(full)
	want := []string{ScopeRead, ScopeDeploy, ScopeTerminal, ScopeTasks, ScopeBuild, ScopeAdmin}
	if len(got) != len(want) {
		t.Fatalf("scopeSetToList(full set) length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopeSetToList(full set)[%d] = %q, want %q (fixed word order drifted from the table snapshot)", i, got[i], want[i])
		}
	}
	// 子集投影与空集（稀疏集只出既有词、序不变；零集出空切片不出 nil 形态
	// 的歧义——make(…, 0, n) 语义保持）。
	subset := scopeSetToList(map[string]bool{ScopeAdmin: true, ScopeRead: true})
	if len(subset) != 2 || subset[0] != ScopeRead || subset[1] != ScopeAdmin {
		t.Fatalf("scopeSetToList({read,admin}) = %v, want [read admin]", subset)
	}
	if empty := scopeSetToList(map[string]bool{}); len(empty) != 0 {
		t.Fatalf("scopeSetToList(empty set) = %v, want empty", empty)
	}
}
