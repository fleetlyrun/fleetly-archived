-- 00025：项目网参与位（T 线 OT-1 / IMPL-T15-1）。
--
-- 列口径（与 apps 的加法列纪律一致；rbac-teams 00018/00019 之后的网络参与面）：
--   apps.project_network_attached  0/1 布尔位（默认 0）——app ∈ 恰一 project
--                                  的**网络参与显式 opt-in**（OT-1「缺省不参加
--                                  任何项目网，维持 app 私网隔离现状」）：
--                                  1 = 该 app 的成员服务在 app 私网之外双挂
--                                  项目网 fleetly-project-<projectID>（全量 ID），项目网
--                                  别名 = <app>-<service>；唯一改变路径 =
--                                  ProjectsService attach/detach RPC（审计 +
--                                  事件同事务），缺省 0 = 既有行为零变化。
--                                  项目归属（apps.project_id，00019 NOT NULL）
--                                  仍是唯一 tenancy 轴；本列只表达网络参与。
--   app 删除（tombstone 第二拍）随 MarkAppDeleted 清 0——删后不参加任何
--   项目网（成员计数不悬挂）。
--
-- 索引：成员计数/对账扫描列（idx_apps_lifecycle 同款纪律）。
--
-- Down 仅供 goose 演练；生产回滚 = 恢复快照（架构 §2.8 契约版本化纪律）。

-- +goose Up
ALTER TABLE apps ADD COLUMN project_network_attached INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_apps_project_network_attached ON apps (project_network_attached);

-- +goose Down
DROP INDEX idx_apps_project_network_attached;
ALTER TABLE apps DROP COLUMN project_network_attached;
