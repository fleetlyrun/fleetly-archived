-- 应用挂起位（app Stop/Start，2026-09-29 Console 对齐 dokploy 三轮 §5）：
-- suspended 是行上权威态位（0/1），不扩 lifecycle 词表（active/deleting/
-- deleted 三态不动——CHECK 约束在 00001，迁移只加法不改写）。语义照抄
-- DB paused 的「状态驱动渲染」形态：置位后引擎周期对账把全部受管长驻
-- 服务排水到副本 0（服务对象保留，引用方连不上是诚实暴露）；清位后由
-- resume 语义走重部署管线恢复副本。挂起期派生状态读面即时投影
-- suspended（engine.DeriveAppState 短路），drift/autoscaler/部署入队按位
-- 豁免——0 副本不是漂移，挂起期不接受新部署（E_APP_SUSPENDED）。
--
-- +goose Up
ALTER TABLE apps ADD COLUMN suspended INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE apps DROP COLUMN suspended;
