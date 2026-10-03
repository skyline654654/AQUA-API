-- 迁移 0053：预警记录与用户预警偏好
--
-- 意图（Why）：
--   预警有两类，触发条件与打扰频次都不同：
--     紧急（余额不足以支撑本次调用）
--     趋势（按当前速率 N 天内耗尽 / 用量异常增长）
--   两者都通过邮件发到【用户注册邮箱】——用户不需要配置额外通道，
--   也不该因为预警收不到而失去账户。
--
-- 为什么必须落表而不是发完就丢：
--   1) 防止重复轰炸：紧急预警需要"已通知过就不再发"的冷却，
--      冷却状态必须持久化（放内存则重启即失效，用户会突然收到一堆）；
--   2) 用户要能查："我上次收到预警是什么时候、为什么"——
--      门户的预警中心直接读这张表；
--   3) 事后复盘：钱快烧完了才发出去，说明阈值需要调整。
--
-- 唯一约束 (user_id, kind, window_bucket) 保证同一用户同一类预警
-- 在同一时间桶内只记一条 —— 窗口桶是"冷却期"的落地方式，
-- 例如紧急预警桶 = 6 小时，趋势预警桶 = 24 小时。

CREATE TABLE IF NOT EXISTS alert_notifications (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id        INTEGER NOT NULL,
    kind           TEXT    NOT NULL DEFAULT '',    -- quota_low / quota_drain / usage_spike
    severity       INTEGER NOT NULL DEFAULT 1,     -- 1=提示 2=警告 3=紧急
    -- window_bucket 是冷却窗口的量化表示：同一 (user, kind, bucket)
    -- 已存在记录时不再重复通知（唯一索引保证），从而实现"每 6 小时最多一条"。
    window_bucket  INTEGER NOT NULL DEFAULT 0,
    -- title / body 是邮件已发出的内容快照：用户中心直接展示，
    -- 避免"当时发了什么"随模板改动而无法复现。
    title          TEXT    NOT NULL DEFAULT '',
    body           TEXT    NOT NULL DEFAULT '',
    -- estimated_days_left 预计余额可支撑天数（趋势预警有值，紧急预警为 0）。
    estimated_days_left INTEGER NOT NULL DEFAULT 0,
    delivered      INTEGER NOT NULL DEFAULT 0,     -- 1=已发出 0=发送失败待重试
    created_at     INTEGER NOT NULL
);

-- 冷却判定：按 (用户, 类型) 查最近一条即可，无需扫全表。
CREATE INDEX IF NOT EXISTS idx_alert_user_kind ON alert_notifications (user_id, kind, created_at);

-- 同一用户同一类预警在同一冷却窗口内只留一条记录。
CREATE UNIQUE INDEX IF NOT EXISTS idx_alert_user_kind_bucket
    ON alert_notifications (user_id, kind, window_bucket);
