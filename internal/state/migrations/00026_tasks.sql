-- 00026：程序化动态工作负载面（T 线 DT-5 / IMPL-T2-1）。
--
-- 表口径（tasks.v1 proto 的 state 落点；加法迁移，既有表零改动）：
--
--   tasks —— 任务台账行。owner_token_id 是隔离与配额的唯一维度（机具令牌
--   scope tasks；跨令牌访问在 API 层 404）。状态机
--   queued → running → stopping → stopped / failed（deleting 是删除墓碑：
--   引擎移除底座服务后删行）；收敛由引擎 tick duty 承载（本表是唯一写点）。
--   env_cipher 是 envelope 密文（值明文不出库；与 app env 同纪律）。
--   network 是解析后的作用域网名（创建时钉定；scope_* 三列保留原引用）。
--
--   task_network_members —— 控制面服务对 task-group 网络的一次性挂靠声明
--   （DT-5「控制面服务每网一次性挂靠，摊销在网络创建时刻」）：成员经发布
--   管线重部署生效（planner 投影双挂），不是 per-task attach。主键
--   (network_ref, app_id, service) 是幂等/防重复的承载。
--
--   task_quotas —— 每令牌并发/资源配额覆盖（缺行 = 平台默认常量；
--   CreateTask 在同一事务内核对非终态行的并发数与 CPU/内存合计，
--   fail-closed 拒绝——见 state/tasks.go）。
--
-- 索引：属主+状态（列表/配额清点）、到期时刻（TTL 回收扫描）。
--
-- Down 仅供 goose 演练；生产回滚 = 恢复快照（架构 §2.8 契约版本化纪律）。

-- +goose Up
CREATE TABLE tasks (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL DEFAULT '',
    owner_token_id  TEXT NOT NULL REFERENCES tokens (id),
    status          TEXT NOT NULL,
    image           TEXT NOT NULL,
    command_json    TEXT NOT NULL DEFAULT '[]',
    args_json       TEXT NOT NULL DEFAULT '[]',
    env_cipher      TEXT NOT NULL DEFAULT '',
    scope_kind      TEXT NOT NULL,
    scope_ref       TEXT NOT NULL,
    scope_internal  INTEGER NOT NULL DEFAULT 0,
    network         TEXT NOT NULL,
    ttl_seconds     INTEGER NOT NULL,
    cpu_millis      INTEGER NOT NULL,
    memory_bytes    INTEGER NOT NULL,
    service         TEXT NOT NULL,
    stop_reason     TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    started_at      INTEGER,
    expires_at      INTEGER NOT NULL,
    stopped_at      INTEGER,
    delete_requested INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_tasks_owner_status ON tasks (owner_token_id, status);
CREATE INDEX idx_tasks_expires_at ON tasks (expires_at);

CREATE TABLE task_network_members (
    network_ref TEXT NOT NULL,
    app_id      TEXT NOT NULL,
    service     TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (network_ref, app_id, service)
);
CREATE INDEX idx_task_network_members_app ON task_network_members (app_id);

CREATE TABLE task_quotas (
    token_id         TEXT PRIMARY KEY REFERENCES tokens (id),
    max_concurrent   INTEGER NOT NULL,
    max_cpu_millis   INTEGER NOT NULL,
    max_memory_bytes INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- +goose Down
DROP TABLE task_quotas;
DROP TABLE task_network_members;
DROP INDEX idx_tasks_expires_at;
DROP INDEX idx_tasks_owner_status;
DROP TABLE tasks;
