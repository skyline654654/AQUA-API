-- 迁移 0052：渠道健康采样（动态权重的 EWMA 输入）
--
-- 意图（Why）：
--   渠道权重目前是人工静态配置的：权重设错就得手动调，而且"调完就定住"，
--   渠道质量随时间变化（上游限流、涨价、故障）时不会自动跟随。
--   动态权重需要持续采样 (成功率, 延迟, 成本) 才能算 EWMA。
--
-- 为什么是"采样表"而不是直接读 usage_logs：
--   EWMA 只需要**最近窗口**的统计，usage_logs 是逐请求明细（增长极快），
--   每轮调权重都去扫明细会越来越慢。采样表把窗口聚合前置到写入路径，
--   查询变成读一行，天然支撑"低频后台任务 + 高频选路"的分离。
--
-- 采样粒度：按 (渠道, 5 分钟桶)。桶内的请求数/成功数/延迟和/成本和累加，
-- 由后台任务每隔一段时间读取未完成桶并推进 EWMA。
--
-- 流转（Flow）：
--   relay 落 usage_logs 时同步累加本表（写一次、只做加减）
--     → 动态权重后台任务每 5 分钟读取一个完整桶
--       → 更新 channels.dynamic_weight（若启用动态权重）

CREATE TABLE IF NOT EXISTS channel_health_samples (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id     INTEGER NOT NULL,
    bucket_start   INTEGER NOT NULL,              -- 5 分钟桶起点（Unix 秒）
    requests       INTEGER NOT NULL DEFAULT 0,   -- 桶内请求数
    success        INTEGER NOT NULL DEFAULT 0,   -- 桶内成功数（2xx/3xx）
    latency_sum_ms INTEGER NOT NULL DEFAULT 0,   -- 桶内总耗时之和
    quota          INTEGER NOT NULL DEFAULT 0,   -- 桶内消耗额度（成本代理指标）
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

-- 同一渠道同一时间桶只累加一行（写入路径按此键 upsert/累加）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_health_channel_bucket
    ON channel_health_samples (channel_id, bucket_start);

-- 后台任务按时间顺序扫未完成桶。
CREATE INDEX IF NOT EXISTS idx_channel_health_bucket ON channel_health_samples (bucket_start);

-- 动态权重开关与平滑系数存在 settings 表：
--   channel.dynamic_weight  0=关闭（纯人工权重）1=开启（EWMA 自动调节）
--   channel.dynamic_smoothing EWMA 的平滑系数 alpha（0~1，越小越平滑）
