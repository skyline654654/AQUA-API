/**
 * 用户门户接口（需登录）。
 *
 * 意图（Why）：
 *   门户三个页面（概览 / 令牌 / 日志）共用这组接口；
 *   集中在此便于与契约的「三、用户门户接口」表格逐行对照。
 *
 * 流转（Flow）：
 *   views/console/* → 本文件 → /api/user/*
 *
 * 扩展（Extend）：
 *   新增门户接口：在此加函数 + 更新 types.ts。
 *   注意：创建令牌会返回一次性明文 key，函数返回类型必须是 CreateTokenResult。
 */
import { api, UPSTREAM_TIMEOUT_MS } from './client'
import type {
  AccessToken,
  AlertRecord,
  CostAttribution,
  CreateGamePayload,
  CreateOrderPayload,
  GameDetail,
  GameMatch,
  GameStepResult,
  CreateTokenPayload,
  CreateTokenResult,
  FinanceSummary,
  ModelRecommend,
  UserKey,
  UserKeyPayload,
  UserKeyProvider,
  LeaderboardStats,
  LogQuery,
  ModelStats,
  MyGroupsResponse,
  OrderQuery,
  Paged,
  PaymentOrder,
  Task,
  TaskQuery,
  TrialGrant,
  UpdateTokenPayload,
  UsageLog,
  UsageStats,
} from './types'

/** GET /api/user/tokens：我的访问令牌列表 */
export function listMyTokens(paged: { page?: number; size?: number } = {}): Promise<Paged<AccessToken>> {
  return api.get<Paged<AccessToken>>('/user/tokens', paged)
}

/** POST /api/user/tokens：创建访问令牌（响应含一次性明文 key） */
export function createMyToken(payload: CreateTokenPayload): Promise<CreateTokenResult> {
  return api.post<CreateTokenResult>('/user/tokens', payload)
}

/** PATCH /api/user/tokens/{id}：改名 / 启停 */
export function updateMyToken(id: number, payload: UpdateTokenPayload): Promise<AccessToken> {
  return api.patch<AccessToken>(`/user/tokens/${id}`, payload)
}

/** DELETE /api/user/tokens/{id}：删除令牌 */
export function deleteMyToken(id: number): Promise<unknown> {
  return api.delete<unknown>(`/user/tokens/${id}`)
}

/**
 * GET /api/user/tokens/{id}/key：取该令牌的明文密钥。
 *
 * 用途：创建弹层没来得及复制、或复制失败时的正式找回入口。
 * 列表出于安全只回掩码（masked_key），本接口是唯一能拿到明文的路径。
 */
export function getMyTokenKey(id: number): Promise<{ id: number; key: string }> {
  return api.get<{ id: number; key: string }>(`/user/tokens/${id}/key`)
}

/**
 * 读取门户可选的分组（用于创建令牌时选择「所属分组」）。
 *
 * 为什么不用公开的模型广场（GET /api/models）：广场不知道"你是谁"，
 * 无法告诉前端"这个分组你还没解锁"。带门槛的分组（如大客户价）如果
 * 不置灰，用户会选进去然后被后端 403，体验上等同于"界面骗人"。
 * 注意：前端置灰只是体验，真正的闸门在服务端（创建/更新令牌时校验），
 * 否则直接调接口就能绕过去。
 */
export function listMyGroups(): Promise<MyGroupsResponse> {
  return api.get<MyGroupsResponse>('/user/groups')
}

/** GET /api/user/usage?days=7：我的用量统计（days 由页面控件决定） */
export function fetchMyUsage(days: number): Promise<UsageStats> {
  return api.get<UsageStats>('/user/usage', { days })
}

/**
 * GET /api/user/leaderboard?days=30：用量排行榜（付费榜 + 免费榜）。
 *
 * days：统计窗口天数（默认 30）；管理员传 all=1 可查看完整榜单（不限前 20）。
 */
export function fetchLeaderboard(days = 30, all = false): Promise<LeaderboardStats> {
  return api.get<LeaderboardStats>('/user/leaderboard', all ? { days, all: '1' } : { days })
}

/**
 * GET /api/user/models/{model}/stats：模型实时指标（tokens/s、平均耗时、TTFB）。
 *
 * 供模型详情页的「实时 tokens/s」展示：5 秒轮询一次，
 * 窗口默认近 15 分钟，可传 minutes 调整。
 */
export function fetchModelStats(model: string, minutes = 15): Promise<ModelStats> {
  return api.get<ModelStats>(`/user/models/${encodeURIComponent(model)}/stats`, { minutes })
}

/** GET /api/user/logs：我的调用日志（分页 + 筛选） */
export function listMyLogs(query: LogQuery): Promise<Paged<UsageLog>> {
  return api.get<Paged<UsageLog>>('/user/logs', { ...query })
}

/* ── 异步任务 ───────────────────────────────────────────── */

/** GET /api/user/tasks：我的异步任务（图像/视频等生成类能力） */
export function listMyTasks(query: TaskQuery = {}): Promise<Paged<Task>> {
  return api.get<Paged<Task>>('/user/tasks', { ...query })
}

/* ── 充值 ───────────────────────────────────────────────── */

/**
 * GET /api/user/orders：我的充值记录。
 *
 * 说明：不提供"提交异步任务"的入口——任务接口走 /v1/tasks 并需要访问令牌，
 * 门户页面只负责展示结果，避免在浏览器里暴露访问令牌。
 */
export function listMyOrders(query: OrderQuery = {}): Promise<Paged<PaymentOrder>> {
  return api.get<Paged<PaymentOrder>>('/user/orders', { ...query })
}

/** POST /api/user/orders：下单（只传金额与通道，额度由服务端计算） */
export function createOrder(payload: CreateOrderPayload): Promise<PaymentOrder> {
  return api.post<PaymentOrder>('/user/orders', payload)
}

/* ── 财务记录 ───────────────────────────────────────────── */

/**
 * GET /api/user/finance：财务板块顶部汇总。
 *
 * 只返回四个数字（余额/累计充值/累计返利/累计消费）+ 充值笔数；
 * 明细各有专门接口（订单、返利明细、调用日志），因此这里保持轻量。
 */
export function fetchFinanceSummary(): Promise<FinanceSummary> {
  return api.get<FinanceSummary>('/user/finance')
}

/**
 * GET /api/user/trial：当前用户的限时试用额（概览页横幅用）。
 *
 * 没有试用额时后端返回 active=false（而不是 404），因此前端无需处理错误态。
 */
export function fetchMyTrial(): Promise<TrialGrant> {
  return api.get<TrialGrant>('/user/trial')
}

/**
 * GET /api/user/orders/{tradeNo}：查询单笔订单。
 *
 * 用途：用户在第三方收银台支付完成后回到本站，前端轮询该接口确认到账。
 */
export function getMyOrder(tradeNo: string): Promise<PaymentOrder> {
  return api.get<PaymentOrder>(`/user/orders/${encodeURIComponent(tradeNo)}`)
}

/* ── 智能运营：成本归因 / 模型推荐 / 预警 / 自备密钥 ─────────────────── */

/**
 * GET /api/user/cost/attribution：按场景标签切分的成本归因。
 *
 * days：统计窗口（默认 30，上限 180）。回答"我的钱花在哪"。
 */
export function fetchCostAttribution(days = 30): Promise<CostAttribution> {
  return api.get<CostAttribution>('/user/cost/attribution', { days })
}

/**
 * GET /api/user/recommend/models：基于个人使用历史的模型推荐。
 *
 * limit：返回条数（默认 6，上限 20）。
 * 路径刻意是 /recommend/models 而非 /models/recommend——
 * 后者会与已存在的 /models/:model/stats 在路由树上争同一层通配段。
 */
export function fetchModelRecommend(limit = 6): Promise<ModelRecommend> {
  return api.get<ModelRecommend>('/user/recommend/models', { limit })
}

/** GET /api/user/alerts：我的预警记录（门户预警中心） */
export function fetchMyAlerts(limit = 20): Promise<{ alerts: AlertRecord[] }> {
  return api.get<{ alerts: AlertRecord[] }>('/user/alerts', { limit })
}

/** GET /api/user/key-providers：可自助接入的上游白名单 */
export function fetchKeyProviders(): Promise<{ providers: UserKeyProvider[] }> {
  return api.get<{ providers: UserKeyProvider[] }>('/user/key-providers')
}

/** GET /api/user/keys：我的自备密钥列表（响应只含掩码，无明文） */
export function listUserKeys(): Promise<{ keys: UserKey[] }> {
  return api.get<{ keys: UserKey[] }>('/user/keys')
}

/** POST /api/user/keys：新增自备密钥（明文经 HTTPS 传给服务端后加密落库） */
export function createUserKey(payload: UserKeyPayload): Promise<{ key: UserKey }> {
  return api.post<{ key: UserKey }>('/user/keys', payload)
}

/**
 * PATCH /api/user/keys/{id}：修改自备密钥。
 *
 * api_key 留空表示"不修改"——编辑弹层不回填明文，只把掩码显示给用户辨认。
 */
export function updateUserKey(id: number, payload: UserKeyPayload): Promise<{ key: UserKey }> {
  return api.patch<{ key: UserKey }>(`/user/keys/${id}`, payload)
}

/** DELETE /api/user/keys/{id}：删除自备密钥（不可恢复，凭据本就在用户手里） */
export function deleteUserKey(id: number): Promise<{ deleted: number }> {
  return api.delete<{ deleted: number }>(`/user/keys/${id}`)
}

/* ── 对弈演示 ─────────────────────────────────────────────────────── */

/**
 * POST /api/user/games：创建一局对局。
 *
 * 创建时不调用模型（因此不消耗额度），只建立对局与初始局面；
 * 之后由前端循环调用 step 逐步推进。
 */
export function createGame(payload: CreateGamePayload): Promise<GameMatch> {
  return api.post<GameMatch>('/user/games', payload)
}

/** GET /api/user/games：我的对局列表 */
export function listGames(): Promise<{ matches: GameMatch[] }> {
  return api.get<{ matches: GameMatch[] }>('/user/games')
}

/** GET /api/user/games/{id}：对局详情（含棋谱） */
export function fetchGame(id: number): Promise<GameDetail> {
  return api.get<GameDetail>(`/user/games/${id}`)
}

/**
 * POST /api/user/games/{id}/step：推进一手。
 *
 * 轮到人类时 move 必填；轮到 AI 时不需要（后端会去调用模型）。
 * 超时用的是 UPSTREAM 档：AI 走子要等上游模型返回，可能数十秒，
 * 用默认 30 秒会让前端比后端先放弃（后端其实马上就会返回）。
 */
export function stepGame(id: number, move?: string): Promise<GameStepResult> {
  return api.post<GameStepResult>(`/user/games/${id}/step`, move ? { move } : {}, {
    timeout: UPSTREAM_TIMEOUT_MS,
  })
}

/** DELETE /api/user/games/{id}：删除对局 */
export function deleteGame(id: number): Promise<{ deleted: number }> {
  return api.delete<{ deleted: number }>(`/user/games/${id}`)
}

/**
 * 取当前局面的图片（即模型看到的那一张）。
 *
 * 必须走 getBlob 而不是把 URL 塞给 <img src>：会话鉴权只认 Authorization 头，
 * 而 <img> 不会带自定义头，直接引 URL 会拿到 401 与一张破图。
 * 拿到 Blob 后由页面转成 object URL 显示。
 *
 * version 参与请求参数只是为了绕过浏览器缓存（局面每推进一手图就变了）；
 * axios 的 params 会拼成查询串，因此与后端无关。
 */
export function fetchGameBoard(id: number, version: number): Promise<Blob> {
  return api.getBlob(`/user/games/${id}/board.png`, { v: version })
}
