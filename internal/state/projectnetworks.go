package state

// 项目网参与原语（T 线 OT-1 / IMPL-T15-1）：app ∈ 恰一 project 的**网络参与
// 显式 opt-in**（apps.project_network_attached，00025 加法列）。
//
// 纪律（设计档 OT-1 + 票面）：
//   - 归属与网络参与两面分离：apps.project_id（00019 NOT NULL）是唯一 tenancy
//     轴；本表列只表达「成员服务是否双挂项目网」。缺省 0 = 不参加（既有 app
//     行为零变化）——唯一改变路径是 ProjectsService attach/detach RPC；
//   - 幂等写：目标值与现值相同 → 零变更返回（不重复审计/事件；changed=false，
//     调用方仍可重跑后置编排——重部署入队在 API 层，见票面实施记录）；
//   - 审计与事件同事务 fail-closed（Outbox）：审计动作
//     app.project_network_attached / app.project_network_detached（target =
//     app:<id>），事件 project.network_changed（subject = project:<id>，
//     payload change=attached|detached + app/app_id/network 名）；
//   - app 删除清位在 MarkAppDeleted（tombstone 第二拍）同事务完成。
//
// 读取面：ProjectNetworkMemberCounts（分组计数——Console 投影与引擎
// 对账/GC 的批量数据源，单个查询免 N+1）；成员口径 = lifecycle='active'
// 且 project_network_attached=1（deleting/deleted 的 app 不再计入成员）。

import (
	"context"
	"fmt"
	"sort"
)

// SetAppProjectNetworkAttached 置位/清位 app 的项目网参与（幂等）。
// 返回值 changed 报告本次是否发生状态变更（false = 目标值与现值相同，
// 未落审计/事件）。app 不存在返回 ErrAppNotFound。
func (s *Store) SetAppProjectNetworkAttached(ctx context.Context, appID string, attached bool, actorUserID, actorTokenID string) (App, bool, error) {
	if appID == "" {
		return App{}, false, fmt.Errorf("state: set project network attached: app id is empty")
	}
	var out App
	changed := false
	err := s.InTx(ctx, func(tx *Tx) error {
		app, err := tx.GetAppByID(ctx, appID)
		if err != nil {
			return err
		}
		if app.ProjectNetworkAttached == attached {
			out = app
			return nil // 幂等：零变更（不重复审计/事件）
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE apps SET project_network_attached = ?, updated_at = ? WHERE id = ?`,
			boolToInt(attached), nowNano(), appID)
		if err != nil {
			return fmt.Errorf("state: update project network flag: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read project network update count: %w", err)
		}
		if n == 0 {
			return ErrAppNotFound
		}
		if err := writeProjectNetworkAuditAndEvent(ctx, tx, app, attached, actorUserID, actorTokenID); err != nil {
			return err
		}
		changed = true
		updated, err := tx.GetAppByID(ctx, appID)
		if err != nil {
			return err
		}
		out = updated
		return nil
	})
	if err != nil {
		return App{}, false, err
	}
	return out, changed, nil
}

// writeProjectNetworkAuditAndEvent 落参与变更的审计与事件（同一事务内，
// fail-closed；调用方保证确有变更）。事件 subject = project:<id>（归属面），
// payload 只带事实字段（change/app/app_id/project_id；零敏感材料）。
// 项目网名不在此落——公式定义点在 internal/naming（state 不 import naming：
// naming 依赖 state，反向会成环），读者按 project_id 现推（公式表在
// naming 包注）。
func writeProjectNetworkAuditAndEvent(ctx context.Context, tx *Tx, app App, attached bool, actorUserID, actorTokenID string) error {
	action := "app.project_network_attached"
	change := "attached"
	if !attached {
		action = "app.project_network_detached"
		change = "detached"
	}
	if err := tx.WriteAudit(ctx, AuditEntry{
		Actor:        auditActor(actorUserID),
		ActorTokenID: actorTokenID,
		Action:       action,
		Target:       "app:" + app.ID,
		Result:       "ok",
		DiffSummary:  DiffSummary("app", app.Name, "project_id", app.ProjectID, "change", change),
	}); err != nil {
		return err
	}
	_, err := tx.AppendEvent(ctx, Event{
		Name:    "project.network_changed",
		Subject: "project:" + app.ProjectID,
		Payload: DiffSummary("change", change, "app_id", app.ID, "app", app.Name, "project_id", app.ProjectID),
	})
	return err
}

// ProjectNetworkMember 是一条「项目网成员计数」投影（项目 ID + 成员数）。
type ProjectNetworkMember struct {
	// ProjectID 是项目平台 ID（项目网名的参数源）。
	ProjectID string
	// Members 是参与位在位的 active app 数（≥1——分组只出行）。
	Members int
}

// ProjectNetworkMemberCounts 返回「项目 → 参与位在位的 active app 数」
// （单分组查询；Console 列表/详情投影与引擎项目网对账/GC 的批量数据源）。
// 无成员的项目不出现在 map（调用方以缺键 = 零成员处理）。
func (s *Store) ProjectNetworkMemberCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, COUNT(1) FROM apps
		WHERE project_network_attached = 1 AND lifecycle = 'active'
		GROUP BY project_id`)
	if err != nil {
		return nil, fmt.Errorf("state: project network member counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var projectID string
		var count int
		if err := rows.Scan(&projectID, &count); err != nil {
			return nil, fmt.Errorf("state: scan project network member count: %w", err)
		}
		out[projectID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate project network member counts: %w", err)
	}
	return out, nil
}

// ProjectNetworkMembers 返回有成员的项目清单（字典序稳定——对账/GC 的
// 期望面；ProjectNetworkMemberCounts 的切片形态投影）。
func (s *Store) ProjectNetworkMembers(ctx context.Context) ([]ProjectNetworkMember, error) {
	counts, err := s.ProjectNetworkMemberCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectNetworkMember, 0, len(counts))
	for projectID, members := range counts {
		out = append(out, ProjectNetworkMember{ProjectID: projectID, Members: members})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out, nil
}
