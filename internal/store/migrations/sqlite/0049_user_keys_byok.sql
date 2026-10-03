-- 迁移 0049：用户自备密钥（BYOK）——加密托管上游凭据
--
-- 意图（Why）：
--   有些用户愿意用自己的上游额度（例如 NVIDIA 官方 NIM 的 API Key），
--   但仍希望通过本网关的统一协议、计费、审计与用量统计来访问。
--   直接把用户自己的 Key 配到「渠道」里是不行的：渠道是【站点的资产】，
--   混进去会导致"这个渠道的钱算谁的"无法区分，也无法按用户路由。
--   BYOK 的语义是：凭据属于用户、加密后由平台托管，调用时按用户身份选用。
--
-- 为什么必须加密落库：
--   Key 是长期有效的账号级凭据，明文入库会随数据库备份一并泄露；
--   本表只存 api_key_enc（密文），与 channels/channel_keys 复用同一套
--   crypto.Cipher（AES-256-GCM），因此不重复引入加密实现。
--
-- 流转（Flow）：
--   用户在门户添加 → server 校验归属 → store 用 cipher.Encrypt 写入
--     → relay 选路时按 (user_id, provider) 命中并 Decrypt 使用
--       → 调用日志仍记 usage_logs（保留审计与统计能力）
--
-- 兼容性：全新表，不影响任何既有查询。

CREATE TABLE IF NOT EXISTS user_keys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL,                  -- 归属用户
    provider     TEXT    NOT NULL DEFAULT '',       -- 上游标识（如 nvidia）
    label        TEXT    NOT NULL DEFAULT '',       -- 用户自定义备注（便于自己分辨）
    api_key_enc  TEXT    NOT NULL DEFAULT '',       -- 密文（AES-256-GCM，与渠道密钥同格式）
    base_url     TEXT    NOT NULL DEFAULT '',       -- 自定义接入点（空 = 用 provider 默认地址）
    models       TEXT    NOT NULL DEFAULT '',       -- 逗号分隔的模型清单（空 = 该 provider 全部模型）
    status       INTEGER NOT NULL DEFAULT 1,        -- 1=启用 2=已停用
    fail_count   INTEGER NOT NULL DEFAULT 0,        -- 连续失败次数（用于自动熔断）
    cooldown_until INTEGER NOT NULL DEFAULT 0,      -- 熔断恢复时间（Unix 秒，0 = 未熔断）
    last_used_at INTEGER NOT NULL DEFAULT 0,        -- 最近一次被调用选中的时间
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- 按用户查可用凭据：选路热路径每次都要按 (user_id, provider) 定位。
CREATE INDEX IF NOT EXISTS idx_user_keys_user_provider ON user_keys (user_id, provider, status);

-- 同一用户同一 provider 最多一条启用凭据（避免选路歧义）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_keys_user_provider_uniq
    ON user_keys (user_id, provider) WHERE status = 1;
