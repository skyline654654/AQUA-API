-- 迁移 0055：usage_logs 增加「本次调用是否计费」标记，供排行榜按请求分榜
--
-- 意图（Why）：
--   用量排行榜原本按「用户是否充过值」拆付费榜 / 免费榜。这个口径有一个
--   结构性问题：**它分类的是人，不是请求**。于是同一个人的免费调用与计费调用
--   被混在一起、整体归到某一个榜里——两个榜的统计基数相互重叠，
--   免费榜里混着计费流量、计费榜里混着免费流量。
--
--   正确口径应当分类【请求本身】：一次调用要么计费、要么免费，二者互斥，
--   这样"两榜请求数之和 = 全站请求数"才成立，两榜也才各自可解释。
--
--   而"这次调用到底算不算计费"是计费层在转发时就已经确定的事实，
--   聚合时再去反推（比如用 quota > 0）并不可靠：
--     · 失败的计费请求 quota 为 0（额度已退还），会被误判成免费；
--     · BYOK 调用 quota 为 0，但它是用户用自己的额度付过钱的。
--   因此在【记录时就落一个标记】，是唯一不会事后误判的做法。
--
-- 判定口径（必须与计费层一致，见 relay.Billing.EstimateReserve）：
--   billing_free = 1  ⟺  该 (分组, 模型, 渠道) 未命中任何计价规则，
--                        或命中的规则被显式设为免费（BillingModeFree）。
--
-- 兼容性说明：
--   新增列默认 0（视为计费），随后把存量数据里 quota = 0 的行回填为免费。
--   之所以用 quota 回填而不是留默认值：存量数据没有这个标记，
--   而 quota = 0 是"当时很可能没计费"的最佳可得近似（它会把失败的计费请求
--   与 BYOK 调用一起算作免费，这是历史数据的固有限制，新数据不再有此问题）。
--   回填后旧账不会消失，只是分榜口径从"按人"变为"按请求"——这正是本次修复的目的。

ALTER TABLE usage_logs ADD COLUMN billing_free INTEGER NOT NULL DEFAULT 0;

-- 回填存量：未产生额度消耗的记录视为免费调用。
UPDATE usage_logs SET billing_free = 1 WHERE quota = 0;

-- 排行榜按 (billing_free, user_id, created_at) 聚合，索引按同一顺序建：
-- 前导列 billing_free 让"分别统计两个榜"各走一次索引区间扫描。
CREATE INDEX IF NOT EXISTS idx_usage_logs_free_user_created
    ON usage_logs (billing_free, user_id, created_at);
