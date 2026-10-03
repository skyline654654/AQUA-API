/**
 * 模型推荐面板：基于个人使用历史推荐「该用哪个模型」。
 *
 * 意图（Why）：
 *   模型广场摆的是"有什么"，但用户面对几十个模型时的真实问题是
 *   "我这种情况该用哪个"。选错的代价不是"不好看"，而是用贵 10 倍的模型干小事、
 *   或用弱模型干大事（质量不够还得返工）。
 *
 * 设计说明：
 *   - **每条推荐都给出人类可读的理由**。只丢一个模型名没有说服力，
 *     用户会问"凭什么"；理由本身就是引擎的可解释性。
 *   - **不展示推荐分**。分数是内部排序工具，把"推荐分 78 分"给用户看
 *     会引发"那 100 分的模型是什么"这类无意义追问。
 *   - **冷启动时明说**。零个人信息时退回站内热门，并诚实地告诉用户
 *     "你还没有使用记录"——基于零信息瞎猜比承认无信息更糟。
 *   - 预估额度不换算成金额：内部额度与站点的倍率配置绑定，前端换算会失真。
 */
'use client'

import { useCallback, useEffect, useState } from 'react'

import { fetchModelRecommend } from '@/api/portal'
import type { ModelRecommend, ModelRecommendItem } from '@/api/types'
import { Badge, Card, EmptyState, SkeletonRows } from '@/components/ui/Display'
import { formatNumber } from '@/utils/format'

/** 理由类型的视觉映射：不同来源给不同色调，让用户一眼看出"凭什么推它"。 */
const REASON_TONE: Record<string, 'ok' | 'info' | 'brand' | 'off'> = {
  complement: 'brand',
  popular: 'ok',
  new: 'info',
}

const REASON_LABEL: Record<string, string> = {
  complement: '互补',
  popular: '热门',
  new: '新上线',
}

/** 单个推荐卡片 */
function RecommendCard({ item }: { item: ModelRecommendItem }) {
  return (
    <div className="flex flex-col rounded-md border border-line p-3.5 transition-colors hover:border-line-2">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="truncate text-[13px] font-semibold text-ink">
              {item.display_name}
            </span>
            {item.vendor && (
              <span className="shrink-0 text-xs text-ink-3">{item.vendor}</span>
            )}
            {item.is_using && <Badge tone="info">你正在用</Badge>}
          </div>
          <div className="mt-0.5 truncate font-mono text-[11px] text-ink-3" title={item.model}>
            {item.model}
          </div>
        </div>
        {item.estimated_quota_per_call > 0 && (
          <span className="shrink-0 text-xs tabular-nums text-ink-3">
            约 {formatNumber(item.estimated_quota_per_call)} / 次
          </span>
        )}
      </div>

      {item.description && (
        <p className="mt-2 line-clamp-2 text-xs leading-relaxed text-ink-2">
          {item.description}
        </p>
      )}

      {item.reasons.length > 0 && (
        <ul className="mt-2.5 space-y-1.5">
          {item.reasons.map((reason, index) => (
            <li key={`${reason.kind}-${index}`} className="flex items-start gap-1.5">
              <Badge tone={REASON_TONE[reason.kind] ?? 'off'}>
                {REASON_LABEL[reason.kind] ?? reason.kind}
              </Badge>
              <span className="min-w-0 flex-1 text-xs leading-relaxed text-ink-2">
                {reason.text}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

export function ModelRecommendPanel({ limit = 6 }: { limit?: number }) {
  const [data, setData] = useState<ModelRecommend | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      setData(await fetchModelRecommend(limit))
    } catch (e) {
      setError(e instanceof Error ? e.message : '加载推荐失败')
    } finally {
      setLoading(false)
    }
  }, [limit])

  useEffect(() => {
    void load()
  }, [load])

  return (
    <Card>
      <div className="mb-4">
        <h2 className="text-[15px] font-semibold text-ink">为你推荐</h2>
        <p className="mt-0.5 text-[13px] text-ink-3">
          {data?.profile_hint ||
            (data?.cold_start
              ? '你还没有使用记录，先按站内热门推荐'
              : '基于你的使用历史推荐')}
        </p>
      </div>

      {loading && <SkeletonRows rows={3} />}

      {!loading && error && (
        <div className="rounded-md border border-err/25 bg-err/5 px-3 py-2.5 text-[13px] text-err">
          {error}
        </div>
      )}

      {!loading && !error && data && data.items.length === 0 && (
        <EmptyState title="暂无推荐" description="站点当前没有可推荐的已启用模型。" />
      )}

      {!loading && !error && data && data.items.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2">
          {data.items.map((item) => (
            <RecommendCard key={item.model} item={item} />
          ))}
        </div>
      )}
    </Card>
  )
}
