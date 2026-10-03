-- 迁移 0051：滥用事件（盗 Key 检测的结果落地）
--
-- 意图（Why）：
--   Key 一旦外泄，损失由站长承担。检测出异常后必须【留痕】：
--   事后要能回答"什么时候、被谁、以什么方式用的"，也便于让用户
--   申诉（"这是我自己的定时任务"）时能对照证据。
--   只在内存里计数是不够的：进程重启就丢，且无法向用户举证。
--
-- 每种异常一个类型（kind），便于聚合统计与差异化处置：
--   burst      请求量突增（相对自身历史 P99）
--   fingerprint 可疑调用指纹（无 UA / 高频重试等）
--   key_leak   凭据疑似外泄
--
-- 处置动作（action）单独留字段：observe（仅记录）/ limit（已自动限流）/
-- notify（已通知站长）。把"发现了什么"与"做了什么"分开记，
-- 便于后续调整阈值而不丢历史。

CREATE TABLE IF NOT EXISTS abuse_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL DEFAULT 0,     -- 关联用户（0 = 系统级，如 Key 泄露）
    token_id    INTEGER NOT NULL DEFAULT 0,     -- 关联令牌（定位是哪把 Key 被盗）
    kind        TEXT    NOT NULL DEFAULT '',    -- 异常类型：burst/fingerprint/key_leak
    severity    INTEGER NOT NULL DEFAULT 0,     -- 1=提示 2=警告 3=严重
    action      TEXT    NOT NULL DEFAULT '',    -- 处置：observe/limit/notify
    detail      TEXT    NOT NULL DEFAULT '',    -- 人类可读说明（已脱敏，不含凭据）
    metric      REAL    NOT NULL DEFAULT 0,     -- 触发时的度量值（如实际 RPS / P99 倍数）
    threshold   REAL    NOT NULL DEFAULT 0,     -- 触发阈值，便于事后调参复盘
    created_at  INTEGER NOT NULL
);

-- 处置与展示都按时间倒序读；按用户聚合趋势时用 user_id。
CREATE INDEX IF NOT EXISTS idx_abuse_events_created ON abuse_events (created_at);
CREATE INDEX IF NOT EXISTS idx_abuse_events_user_kind ON abuse_events (user_id, kind, created_at);
