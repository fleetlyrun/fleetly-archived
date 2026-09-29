-- git push 部署面移除（2026-09-29，ADR-0012 裁决：砍 SSH 收包半，webhook
-- +拉源保留）。本迁移是「迁移只加法」惯例的刻意破例——新增文件不改写任何
-- 已应用迁移（sha256 golden 不受影响），但对 schema/数据做减法：
--   1. git_keys 表：SSH push 公钥认证台账，随 SSH 收包面退场（消费方
--      gitserver ssh.go PublicKeyCallback 与 api GitKeysService 已删）。
--   2. tokens 里 post-receive 钩子回调令牌（name = 'git hook <app>'）：
--      钩子已不再签发；存量行是仍然有效的 deploy-scope 凭证，属安全残留，
--      必须清理而非留置。
--   3. platform_settings 的 git SSH host key 指纹台账（hostkeysettings.go
--      已删，披露面 git_ssh_fingerprint 字段已 reserved）。
-- webhook + 拉源的存活依赖不受影响：apps 的 git_branch/webhook_secret/
-- source_* 列、deployments.source_git_sha/ref（(app,sha) 去重判据）、
-- bare 仓库（git.root）与 git.hostkey_first_seen TOFU（拉源 known_hosts）
-- 全部保留。
--
-- +goose Up
DROP TABLE git_keys;
DELETE FROM tokens WHERE name LIKE 'git hook %';
DELETE FROM platform_settings WHERE key = 'git.hostkey_fingerprint';

-- +goose Down
-- 仅供 goose 演练（架构 §2.8：生产回滚 = 恢复快照）。git_keys 结构按
-- 00028 时点形态重建（00007 DDL + 00018 的 user_id 列）——保证迁移链
-- 可下重放（00018/00007 的 Down 依赖此表在位）；数据不可恢复（密钥台账
-- 属快照恢复域）。tokens 的 hook 行与 platform_settings 指纹行不重建
-- （纯数据行，无 schema 依赖）。
CREATE TABLE git_keys (
    id          TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL UNIQUE,
    public_key  TEXT NOT NULL,
    key_type    TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    user_id     TEXT,
    created_at  INTEGER NOT NULL
);
