-- 迁移 0050：调用场景标签（成本归因的基础）
--
-- 意图（Why）：
--   站长最需要的不是"用户花了多少"，而是"钱花在哪"——是 Playground 试玩、
--   是某个插件、还是某个脚本在刷。缺少维度就找不到可优化的环节。
--   标签由客户端通过 HTTP 头 X-Aqua-Tag 传入（缺省归为 untagged），
--   落进每条 usage_logs，成本归因与排行榜都基于它聚合。
--
-- 为什么不用「令牌」这个天然维度：
--   令牌本身能回答"哪个 Key 花的"，但同一把 Key 往往被多个场景复用
--   （开发调试 + 线上服务），按令牌切分仍然回答不了"哪个场景"。
--   标签是对令牌维度的正交补充，两者可以一起看。
--
-- 兼容性：
--   NOT NULL + 常量默认值，SQLite 允许对已有表直接 ADD COLUMN；
--   历史行统一为 ''（未标注），归因时按「未标注」聚合即可。

ALTER TABLE usage_logs ADD COLUMN tag TEXT NOT NULL DEFAULT '';

-- 成本归因热路径：按 (user_id, tag) 聚合用量。
CREATE INDEX IF NOT EXISTS idx_usage_logs_user_tag ON usage_logs (user_id, tag, created_at);
