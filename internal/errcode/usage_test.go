package errcode

// 注册表使用点扫描测试（MG-2，B4）：遍历注册表全部错误/警告码，断言每码
// 在仓内生产代码（internal + cmd，排除 _test 与注册表自身）有 ≥1 引用点
//（带引号的字符串形态——错误码只以字面量进入 apperr.New/errorf 等信封
// 构造面）。注册但零引用的码 = 注册表与实现脱节（漏发或死注册）：真实
// 漏发补最小实现；确属预留的入下方豁免清单（每条必须有注释理由）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codeExemptions 是零引用码的豁免清单（MG-2：豁免必须有注释理由；注册表
// 行同步加 `// 预留：…` 注释）。
var codeExemptions = map[string]string{
	// 预留：恢复器校验失败码（备份集/主密钥指纹不匹配拒绝半恢复）——
	// 恢复器未实现（横切评审确认的预留码，state-model §2.7）。
	"E_BACKUP_KEY_MISSING": "reserved: restorer not implemented (confirmed by cross-cutting review)",
	// 退役面 + 保留码（E1-7 追认）：v0.1 的多节点操作守卫路径已删除
	//（internal/placement 的 GuardMultiNode/MultiNodeUnsupported 与
	// MoveBinding 静态守卫——多节点解析/显式换点 Rebind 取代）；码保留
	// 注册表、永不复用，零引用属退役而非死注册（multi-node §5.2）。
	"E_CAPABILITY_REQUIRES_MULTI_NODE": "retired guard path (E1-7); code retained in the registry forever, never reused",
	// E_PLACEMENT_MOVE_REQUIRES_ACK 的 v0.1 豁免已移除：显式换点 Rebind
	// 的 confirm 门（E1-7）成为真实引用点（internal/placement/rebind.go）。
	// E_S3_NOT_CONFIGURED 的预留豁免已移除：label fleetly.s3=true 注入
	// 前哨（E3-4，W3-S3）成为真实引用点——internal/engine/s3inject.go
	//（s3.mode=unset 时 plan 阶段诚实拒绝，设计 §2.4）。
	// E4 数据库托管（managed-databases 设计 §5.2，S1 阶段注册）：S2 已接线
	// 三码的豁免移除——E_DB_NOT_FOUND / E_DB_REFERENCED = 生命周期 API 删除
	// 守卫（internal/api/databases.go）；E_DB_TEMPLATE_UNSUPPORTED = 创建
	// 模板校验（同文件）。S3 已接线 E_DB_ENV_PREFIX_CONFLICT（引用 plan
	// 哨兵，internal/engine/dbinject.go）。S4 已接线 E_DB_ROTATE_FAILED
	//（轮换编排中途失败，internal/api/databases.go mapRotationErr）与
	// E_SECRET_NOT_FOUND（compose secrets preflight，
	// internal/engine/secretinject.go + 回滚 preflight）。S5 已接线
	// E_DB_BACKUP_FAILED/E_DB_RESTORE_FAILED 的豁免移除：备份 job/校验失败
	// 与恢复中途失败（internal/database/backup.go runBackup + restore.go
	// restoreAbort——异步编排的失败信封在底座邻接层构造，s3inject 用 apperr
	// 同款纪律）。
	// E_ENV_KEY_RESERVED 不豁免：保留前缀守卫已在 S1 接线
	//（internal/state/env.go SetAppEnv）。
}

// productionSources 收集 internal 与 cmd 下的生产 .go 文件文本（排除
// _test 与注册表包自身）。
func productionSources(t *testing.T) map[string]string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	out := map[string]string{}
	for _, sub := range []string{"internal", "cmd"} {
		base := filepath.Join(root, sub)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// 排除注册表包自身（定义文件不算引用点）。
				if filepath.Base(path) == "errcode" && path == filepath.Join(root, "internal", "errcode") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path) //nolint:gosec // G304：扫描本仓源码树，路径自 WalkDir
			if err != nil {
				return err
			}
			out[path] = string(raw)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("no production sources scanned (scan harness broken)")
	}
	return out
}

// TestRegistryCodesReferencedInProduction 注册表全量扫描：每码在生产代码
// 有 ≥1 个带引号引用点（或进入带理由的豁免清单）。
func TestRegistryCodesReferencedInProduction(t *testing.T) {
	srcs := productionSources(t)
	for _, c := range Default().All() {
		if _, exempt := codeExemptions[c.ID]; exempt {
			continue
		}
		quoted := `"` + c.ID + `"`
		found := ""
		for path, src := range srcs {
			if strings.Contains(src, quoted) {
				found = path
				break
			}
		}
		if found == "" {
			t.Errorf("code %s has zero production references since registration (add the missing emit if real; if genuinely reserved, note the reason in codeExemptions and on the registry row)", c.ID)
		}
	}
}

// TestCodeExemptionsStillRegistered 豁免清单健康度：豁免项必须仍在注册表
// （豁免的是「零引用」而不是「可注销」——码永不复用）。
func TestCodeExemptionsStillRegistered(t *testing.T) {
	for id := range codeExemptions {
		if _, ok := Default().Get(id); !ok {
			t.Errorf("exemption entry %s not in the registry (exemption list out of sync with registry)", id)
		}
	}
}
