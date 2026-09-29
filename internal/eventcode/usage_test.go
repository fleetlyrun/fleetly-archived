package eventcode

// 注册表使用点扫描测试（MG-2，B4）：遍历注册表全部事件名，断言每个事件
// 在仓内生产代码（internal + cmd，排除 _test 与注册表自身）有 ≥1 引用点
//（带引号的字符串形态——事件名只以字面量出现在 AppendEvent/appendEvent
// 调用面）。注册但零引用的事件名 = 注册表与实现脱节（漏发或死注册）：
// 真实漏发补最小实现；确属预留的入下方豁免清单（每条必须有注释理由）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// eventExemptions 是零引用事件的豁免清单（MG-2：豁免必须有注释理由；
// 新增豁装需要同步写明预留依据，注册表行同加 `// 预留：…` 注释）。
var eventExemptions = map[string]string{
	// 预留：系统性失败恢复排队（release-semantics §2.7）——v0.1 失败分流
	// 不建新 deployment（D-REL-6 默认只告警），自动恢复动作随 v0.2 恢复器。
	"deployment.recovery_scheduled": "reserved: auto-recovery queueing event, wired with the v0.2 restorer",
	// 预留：部署被更新目标取代（陈旧终态）——v0.1 同 app 互斥排队
	//（§2.5），不存在「在途部署被新目标取代」的路径；superseded 随并发
	// 部署策略（v0.2）开放。
	"deployment.superseded": "reserved: no supersede path under per-app exclusive queueing; opens with the v0.2 concurrency policy",
	// 预留：绑定变更事件只在显式确认的迁移路径（rebind/move）发出——
	// v0.1 单机无第二候选（MultiNodeUnsupported 守卫），rebind CLI 属 v0.2。
	"placement.changed": "reserved: rebind/migration path in v0.2 (single node has no second candidate)",
	// 预留：DR 后绑定无法判定的显式放置要求——DR 恢复阶梯 L1/L2 的 v0.2
	// 面（单机 v0.1 无 DR 绑定歧义场景）。
	"placement.unresolved": "reserved: post-DR binding decision, with the v0.2 recovery ladder",
	// 预留：节点观测事件族（joined/down/up/removed）v0.1 零引用——v0.2
	// 多节点（E1-6）已由锚定 duty 差分发出（internal/state/clusteranchor.go），
	// 豁免条目随之移除；保留本注释作为词面纪律的变迁记录。
	// placement.changed / volume.discarded 的豁免同样移除：显式换点 Rebind
	//（E1-7，internal/placement/rebind.go）成为真实发出来源。
	// 预留：卷声明移除（detached）的显性化——v0.1 对账只动服务面，卷声明
	// 移除的显性化随卷生命周期票接线。
	"volume.detached": "reserved: surfacing volume-declaration removal, with the v0.2 volume lifecycle ticket",
	// 预留：控制面恢复流程完成事件——恢复器（state-model §2.7 恢复阶梯）
	// 未实现（同 E_BACKUP_KEY_MISSING 的预留裁决）。
	"restore.completed": "reserved: restorer not implemented (same as E_BACKUP_KEY_MISSING)",
	// cron.skipped 的预留豁免已移除：E5 Cron（W3-S5）触发链成为真实发出来
	// 源——internal/cron（overlap/node_unavailable/missed_downtime/interrupted
	// 四类 skip 路径）。
	// E4 数据库托管（managed-databases §5.3，S1 阶段注册）：db.* 18 事件中
	// 转移事件 9 的豁免已在 S2 移除——发出来源 = EnterDbPhase 单写点的调用
	// 方：db.provision_started（受理/重试，internal/api/databases.go）+
	// db.ready/db.provision_failed/db.degraded/db.recovered/db.deleted（收敛
	// duty，internal/database/converge.go）+ db.suspended/db.resumed（受理，
	// internal/api/databases.go）+ db.delete_started（删除守卫通过后，同上）。
	// db.credentials_rotated 的豁免已在 S4 移除：发出来源 = 轮换编排成功尾
	//（internal/database/rotate.go RotateCredentials）。S5 已接线操作事件
	// 其余 8 的豁免移除：db.upgrade_available（可升级公告 duty，
	// internal/database/upgrade.go）+ db.upgrade_started/finished/failed
	//（升级编排，同文件）+ db.backup_succeeded/db.backup_failed（备份编排，
	// internal/database/backup.go）+ db.restore_completed/db.restore_failed
	//（恢复编排，internal/database/restore.go）。注册表行已注明出处。
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
				if filepath.Base(path) == "eventcode" && path == filepath.Join(root, "internal", "eventcode") {
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

// TestRegistryEventsReferencedInProduction 注册表全量扫描：每个事件名在
// 生产代码有 ≥1 个带引号引用点（或进入带理由的豁免清单）。
func TestRegistryEventsReferencedInProduction(t *testing.T) {
	srcs := productionSources(t)
	for _, ev := range Default().All() {
		if _, exempt := eventExemptions[ev.Name]; exempt {
			continue
		}
		quoted := `"` + ev.Name + `"`
		found := ""
		for path, src := range srcs {
			if strings.Contains(src, quoted) {
				found = path
				break
			}
		}
		if found == "" {
			t.Errorf("event %s has zero production references since registration (add the missing emit if real; if genuinely reserved, note the reason in eventExemptions and on the registry row)", ev.Name)
		}
	}
}

// TestEventExemptionsStillRegistered 豁免清单健康度：豁免项必须仍在注册表
// （豁免的是「零引用」而不是「可注销」——名永不复用）；注册表新增事件若
// 零引用且未豁免由上一测试兜底。
func TestEventExemptionsStillRegistered(t *testing.T) {
	for name := range eventExemptions {
		if _, ok := Default().Get(name); !ok {
			t.Errorf("exemption entry %s not in the registry (exemption list out of sync with registry)", name)
		}
	}
}
