-- 00024：app_configs 表（T 线 OT-3 / IMPL-T1-4 Config 资源）。
--
-- 列口径（与 00015 的 app_secrets 同形；差异 = 值明文可回读）：
--   id          ULID 主键；
--   app_id      归属 app（REFERENCES apps；app 行 tombstone 不删，行随
--               app 生命周期保留）；
--   name        声明名（compose configs 引用的短名；UNIQUE (app_id, name)
--               承载覆盖即换版——同名 SetConfig 重盖 value/hash8/updated_at）；
--   value       配置内容明文（OT-3：明文、版本化、审计、可回读——本表就是
--               配置面的可读值；app_secrets 的 age 密文口径不适用于此）；
--   hash8       值 sha256 前 8（naming.Hash8——swarm config 对象名的内容
--               寻址尾缀与引用比对锚，与 app_secrets.hash8 同口径）；
--   created_at/updated_at  UnixNano（仓内时间列约定）。
--
-- Down 仅供 goose 演练；生产回滚 = 恢复快照（架构 §2.8 契约版本化纪律）。

-- +goose Up
CREATE TABLE app_configs (
    id         TEXT PRIMARY KEY,
    app_id     TEXT NOT NULL REFERENCES apps (id),
    name       TEXT NOT NULL,
    value      TEXT NOT NULL,
    hash8      TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (app_id, name)
);

-- +goose Down
DROP TABLE app_configs;
