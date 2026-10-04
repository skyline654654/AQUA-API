-- 迁移 0054：对弈演示的对局与棋谱
--
-- 意图（Why）：
--   对弈是"多模态能力"的可视化演示，必须持久化，原因有三：
--     1) 一盘棋要几十次模型调用、跨数分钟，前端刷新或断线后必须能续上；
--     2) 棋谱是演示的产物本身——"模型哪一手走错了、错在哪个坐标"
--        正是用户想看的结论，不落库就等于演示完就没了；
--     3) 局中模型的原始输出要留档：只看解析后的着法无法区分
--        "模型走错了"与"我们解析错了"，而这两者的处置完全不同。
--
-- 为什么同时存「棋谱」与「局面快照」：
--   棋谱（game_moves）用于展示与复盘；局面快照（state_json）用于 O(1) 恢复对局。
--   只存棋谱意味着每次读对局都要重放全部着法才能渲染棋盘
--   （一盘围棋可下数百手），在读接口上做几百次规则运算并不划算。
--   两份数据的一致性由写入侧保证：同一事务内先追加棋谱、再更新快照。
--
-- 字段说明：
--   mode        对局模式：ai_vs_ai（两个模型互下）/ human_vs_ai（人机对弈）
--   human_color 人机模式下人类执哪一方（0 = 无人类参与）
--   status      playing / black_win / white_win / draw / aborted
--               aborted 与 draw 必须区分：前者是"某一方给不出合法着法、
--               对局没下完"，后者是"下完了、平局"。混为一谈会让人
--               误以为"模型下成了和棋"。
--   error_text  中止原因（如"连续 3 次未能给出合法着法"），供前端如实展示
--   state_json  局面快照（game.Position 的 JSON），含棋盘、手番、提子数、末手

CREATE TABLE IF NOT EXISTS game_matches (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL,               -- 发起人（对局归属该用户，消耗其额度）
    kind        TEXT    NOT NULL DEFAULT '',    -- gomoku / go / xiangqi
    mode        TEXT    NOT NULL DEFAULT '',    -- ai_vs_ai / human_vs_ai
    black_model TEXT    NOT NULL DEFAULT '',    -- 执黑方的模型名（人类席位为空串）
    white_model TEXT    NOT NULL DEFAULT '',    -- 执白方的模型名
    human_color INTEGER NOT NULL DEFAULT 0,     -- 人类席位：0=无 1=黑 2=白
    status      TEXT    NOT NULL DEFAULT 'playing',
    winner      TEXT    NOT NULL DEFAULT '',    -- black / white / draw / 空（未分胜负）
    move_count  INTEGER NOT NULL DEFAULT 0,
    error_text  TEXT    NOT NULL DEFAULT '',    -- 中止原因（人类可读）
    state_json  TEXT    NOT NULL DEFAULT '',    -- 局面快照（JSON）
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

-- 列表按用户 + 时间倒序读（"我的对局"）。
CREATE INDEX IF NOT EXISTS idx_game_matches_user_created ON game_matches (user_id, created_at);
-- 后台若要按状态统计（进行中/已完成），走这个索引。
CREATE INDEX IF NOT EXISTS idx_game_matches_status ON game_matches (status);

CREATE TABLE IF NOT EXISTS game_moves (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    match_id    INTEGER NOT NULL,
    seq         INTEGER NOT NULL,               -- 手数（从 1 开始，与棋谱一致）
    color       INTEGER NOT NULL,               -- 1=黑 2=白
    notation    TEXT    NOT NULL DEFAULT '',    -- 人类可读着法（如 H8 / H1H4 / PASS）
    raw_output  TEXT    NOT NULL DEFAULT '',    -- 模型原始输出（截断后；排查用）
    attempts    INTEGER NOT NULL DEFAULT 1,     -- 本手尝试了几次才成功（1 = 一次就对）
    error_text  TEXT    NOT NULL DEFAULT '',    -- 该手若最终失败，记录原因
    created_at  INTEGER NOT NULL
);

-- 按对局取棋谱：唯一索引同时起到"同一对局内手数不重复"的约束作用，
-- 避免并发推进时写出两个 seq=5 的着法（那会让复盘顺序错乱）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_game_moves_match_seq ON game_moves (match_id, seq);
