-- 00027：上传构建面（T 线 DT-6 / IMPL-T2-2）。
--
-- builds.app_id 放开为可空（表重建，SQLite 十二步法收缩形态；00019 先例）：
-- 上传构建是**无 app 归属**的通用镜像构建（调用方给镜像仓名，产物进平台
-- registry 的 apps/<name> 命名空间）——app_id 引用保留（非空值仍受 FK
-- 约束），空值表示上传构建。app 级构建（compose/git）语义零变化。
--
-- 其余列（CHECK 词典/DEFAULT）逐字保留 00003 终态口径；三个索引重建后
-- 同名重铸。Down 仅供 goose 演练（生产回滚 = 恢复快照）：恢复 NOT NULL
-- 形态——带 NULL 行的库上执行会显性失败（fresh 链演练库恒空，拷贝语句
-- 只为迁移链完整性存在）。

-- +goose Up
CREATE TABLE builds_new (
    id           TEXT PRIMARY KEY,
    app_id       TEXT REFERENCES apps (id),
    service      TEXT NOT NULL,
    driver       TEXT NOT NULL CHECK (driver IN ('railpack', 'dockerfile', 'passthrough')),
    status       TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'building', 'succeeded', 'failed')),
    image_ref    TEXT NOT NULL DEFAULT '',
    image_digest TEXT NOT NULL DEFAULT '',
    request      TEXT NOT NULL DEFAULT '{}',
    plan_path    TEXT NOT NULL DEFAULT '',
    log_path     TEXT NOT NULL DEFAULT '',
    error_code   TEXT,
    created_at   INTEGER NOT NULL,
    started_at   INTEGER,
    finished_at  INTEGER
);
INSERT INTO builds_new
    (id, app_id, service, driver, status, image_ref, image_digest, request,
     plan_path, log_path, error_code, created_at, started_at, finished_at)
SELECT
    id, app_id, service, driver, status, image_ref, image_digest, request,
    plan_path, log_path, error_code, created_at, started_at, finished_at
FROM builds;
DROP TABLE builds;
ALTER TABLE builds_new RENAME TO builds;
CREATE INDEX idx_builds_app_created ON builds (app_id, created_at);
CREATE INDEX idx_builds_status ON builds (status);
CREATE INDEX idx_builds_digest ON builds (image_digest);

-- +goose Down
CREATE TABLE builds_rollback (
    id           TEXT PRIMARY KEY,
    app_id       TEXT NOT NULL REFERENCES apps (id),
    service      TEXT NOT NULL,
    driver       TEXT NOT NULL CHECK (driver IN ('railpack', 'dockerfile', 'passthrough')),
    status       TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'building', 'succeeded', 'failed')),
    image_ref    TEXT NOT NULL DEFAULT '',
    image_digest TEXT NOT NULL DEFAULT '',
    request      TEXT NOT NULL DEFAULT '{}',
    plan_path    TEXT NOT NULL DEFAULT '',
    log_path     TEXT NOT NULL DEFAULT '',
    error_code   TEXT,
    created_at   INTEGER NOT NULL,
    started_at   INTEGER,
    finished_at  INTEGER
);
INSERT INTO builds_rollback
    (id, app_id, service, driver, status, image_ref, image_digest, request,
     plan_path, log_path, error_code, created_at, started_at, finished_at)
SELECT
    id, app_id, service, driver, status, image_ref, image_digest, request,
    plan_path, log_path, error_code, created_at, started_at, finished_at
FROM builds;
DROP TABLE builds;
ALTER TABLE builds_rollback RENAME TO builds;
CREATE INDEX idx_builds_app_created ON builds (app_id, created_at);
CREATE INDEX idx_builds_status ON builds (status);
CREATE INDEX idx_builds_digest ON builds (image_digest);
