/**
 * 成本归因看板：按场景标签切分的用量视图。
 *
 * 意图（Why）：
 *   "我花了多少"是账单已经告诉用户的事；真正没答案的是"钱花在哪"——
 *   是站内试玩、还是某个插件在后台静默烧、还是某个脚本在刷。
 *   缺少这个维度，所有优化都只能靠猜。
 *
 * 流转（Flow）：
 *   console 页面 → <CostAttributionPanel /> → fetchCostAttribution(days)
 *     └─ 后端按 (user_id, tag) 聚合 usage_logs，返回请求数 / Token / 额度 / 占比
 *         → 汇总卡（4 个数字）+ 占比条列表
 *
 * 设计说明：
 *   - 额度是内部计费单位，【不换算成金额】。同一个数值在不同站点的
 *     "1 元 = 多少额度"由配置决定，前端硬换算会在用户改动倍率后立刻说错话。
 *   - 占比用横向进度条而非饼图：场景数不固定（客户端可自定义标签），
 *     饼图在超过 5 块后就难以分辨，横向条则任意条数都可读。
 *   - 场景数无界（客户端可自由声明标签），后端按额度倒序截断到 50 条，
 *     truncated 为真时显式说明"仅显示前 N 项"，不静默截断。
 */
'use client'

import { useCallback, useEffect, useState } from 'react'

import { fetchCostAttribution } from '@/api/portal'
import type { CostAttribution } from '@/api/types'
import { Card, EmptyState, SkeletonRows, StatCard } from '@/components/ui/Display'
import { formatNumber, formatPercent } from '@/utils/format'

/** 可选统计窗口（天）。上限与后端一致（180 天），超出会被后端夹到默认 30。 */
const WINDOW_OPTIONS = [7, 30, 90] as const

type WindowDays = (typeof WINDOW_OPTIONS)[number]

/** 窗口按钮的布局容器：用 flex 而非 grid，避免移动端按钮换行后出现空列。 */
const TABS = 'flex items-center gap-1 rounded-lg border border-line bg-surface p-0.5'

/** 单个 tab 按钮的类名工厂：选中态用 brand 强调，未选中保持弱化。 */
function tabClass(active: boolean): string {
  return active
    ? 'rounded-md bg-brand px-2.5 py-1 text-xs font-medium text-on-brand'
    : 'rounded-md px-2.5 py-1 text-xs text-ink-3 hover:text-ink'
}

export function CostAttributionPanel() {
  const [days, setDays] = useState<WindowDays>(30)
  const [data, setData] = useState<CostAttribution | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async (windowDays: WindowDays) => {
    setLoading(true)
    setError('')
    try {
      setData(await fetchCostAttribution(windowDays))
    } catch (e) {
      // 失败必须让用户看见：静默失败会让人以为是"没有用量"，
      // 从而错过"接口挂了"这个真实问题。
      setError(e instanceof Error ? e.message : '加载成本归因失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load(days)
  }, [days, load])

  return (
    <Card>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-[15px] font-semibold text-ink">成本归因</h2>
          <p className="mt-0.5 text-[13px] text-ink-3">
            按调用场景切分用量，回答「钱花在哪」
          </p>
        </div>
        <div className={TABS} role="tablist" aria-label="统计窗口">
          {WINDOW_OPTIONS.map((option) => (
            <button
              key={option}
              type="button"
              role="tab"
              aria-selected={days === option}
              onClick={() => setDays(option)}
              className={tabClass(days === option)}
            >
              近 {option} 天
            </button>
          ))}
        </div>
      </div>

      {loading && <SkeletonRows rows={4} />}

      {!loading && error && (
        <div className="rounded-md border border-err/25 bg-err/5 px-3 py-2.5 text-[13px] text-err">
          {error}
        </div>
      )}

      {!loading && !error && data && data.items.length === 0 && (
        <EmptyState
          title="暂无用量数据"
          description="这个时间窗内还没有调用记录。调用时带上 X-Aqua-Tag 头可自动归因到对应场景。"
        />
      )}

      {!loading && !error && data && data.items.length > 0 && (
        <>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard label="总请求" value={formatNumber(data.summary.requests)} />
            <StatCard label="总 Token" value={formatNumber(data.summary.tokens)} />
            <StatCard label="总额度" value={formatNumber(data.summary.quota)} />
            <StatCard label="场景数" value={formatNumber(data.summary.tag_count)} />
          </div>

          {data.summary.truncated && (
            <p className="mt-3 text-xs text-warn">
              场景较多，仅显示消耗最高的前 {data.items.length} 项
            </p>
          )}

          <div className="mt-4 space-y-3">
            {data.items.map((item) => (
              <div key={item.tag} className="rounded-md border border-line p-3">
                <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
                  <span
                    className="truncate text-[13px] font-medium text-ink"
                    title={item.tag_label}
                  >
                    {item.tag_label}
                  </span>
                  <span className="text-[13px] font-medium tabular-nums text-ink-2">
                    {formatPercent(item.share, 1)}
                  </span>
                </div>

                {/* 占比条：宽度用百分比内联（Tailwind 无法表达动态值） */}
                <div className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-ink/8">
                  <div
                    className="h-full rounded-full bg-brand transition-[width] duration-300"
                    style={{ width: `${Math.max(item.share * 100, item.share > 0 ? 1.5 : 0)}%` }}
                  />
                </div>

                <div className="mt-2 flex flex-wrap gap-x-4 gap-y-0.5 text-xs text-ink-3">
                  <span>{formatNumber(item.requests)} 次请求</span>
                  <span>{formatNumber(item.tokens)} Token</span>
                  <span>额度 {formatNumber(item.quota)}</span>
                  <span>平均 {formatNumber(Math.round(item.avg_tokens))} Token/次</span>
                </div>
              </div>
            ))}
          </div>
        </>
      )}
    </Card>
  )
}
