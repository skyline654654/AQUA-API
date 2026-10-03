/**
 * 预警中心：展示用户收到过的额度预警。
 *
 * 意图（Why）：
 *   预警有两条投递通道——邮件与站内。缺了这个页面，"邮件被删/被归进垃圾箱/
 *   根本没送达"就意味着用户永远不知道自己错过过什么。
 *   它同时也是"我为什么收到这封邮件"的自查入口。
 *
 * 设计说明：
 *   - 左侧色条按severity 区分（3 紧急 / 2 警告 / 1 提示），
 *     比文字标签更快被扫到——预警列表通常很长，用户只扫一眼。
 *   - **未送达必须显式标注**。delivered=false 意味着"这封没发出去"，
 *     不标注会让用户以为"站内有就等于邮件也到了"。
 *   - summary 是后端从邮件正文里提取的【纯文本】（不是 HTML），
 *     因此可以安全地直接渲染成文本节点。
 */
'use client'

import { useCallback, useEffect, useState } from 'react'

import { fetchMyAlerts } from '@/api/portal'
import type { AlertRecord } from '@/api/types'
import { Badge, Card, EmptyState, SkeletonRows } from '@/components/ui/Display'
import { formatDateTime } from '@/utils/format'

/** 严重级别 → 左侧色条与徽标色调 */
const SEVERITY_STYLE: Record<number, { bar: string; tone: 'err' | 'warn' | 'info' }> = {
  3: { bar: 'bg-err', tone: 'err' },
  2: { bar: 'bg-warn', tone: 'warn' },
  1: { bar: 'bg-info', tone: 'info' },
}

/** 天数的人话渲染：0 天读起来像"已经没救了"，实际含义是"今天之内会用完"。 */
function daysText(days: number): string {
  if (days <= 0) return '不足 1 天'
  if (days === 1) return '约 1 天'
  return `约 ${days} 天`
}

export function AlertCenterPanel({ limit = 20 }: { limit?: number }) {
  const [alerts, setAlerts] = useState<AlertRecord[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const res = await fetchMyAlerts(limit)
      setAlerts(res.alerts ?? [])
    } catch (e) {
      setError(e instanceof Error ? e.message : '加载预警记录失败')
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
        <h2 className="text-[15px] font-semibold text-ink">预警中心</h2>
        <p className="mt-0.5 text-[13px] text-ink-3">
          额度不足与即将耗尽的历史预警（同时也是邮件之外的第二通道）
        </p>
      </div>

      {loading && <SkeletonRows rows={3} />}

      {!loading && error && (
        <div className="rounded-md border border-err/25 bg-err/5 px-3 py-2.5 text-[13px] text-err">
          {error}
        </div>
      )}

      {!loading && !error && alerts.length === 0 && (
        <EmptyState
          title="暂无预警记录"
          description="余额充足时不会收到预警。额度偏低时系统会发邮件提醒你。"
        />
      )}

      {!loading && !error && alerts.length > 0 && (
        <ul className="space-y-2.5">
          {alerts.map((alert) => {
            const style = SEVERITY_STYLE[alert.severity] ?? SEVERITY_STYLE[1]
            return (
              <li
                key={alert.id}
                className="flex gap-3 overflow-hidden rounded-md border border-line"
              >
                <div className={`w-1 shrink-0 ${style.bar}`} aria-hidden />
                <div className="min-w-0 flex-1 py-3 pr-3.5">
                  <div className="flex flex-wrap items-center gap-1.5">
                    <Badge tone={style.tone}>{alert.kind_text}</Badge>
                    <span className="text-[13px] font-medium text-ink">
                      {alert.summary}
                    </span>
                    {/* 未送达必须显式说明：站内可见 ≠ 邮件已到 */}
                    {!alert.delivered && <Badge tone="off">邮件未送达</Badge>}
                  </div>
                  <div className="mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-ink-3">
                    {alert.days_left > 0 && <span>预计可用 {daysText(alert.days_left)}</span>}
                    <span>{formatDateTime(alert.created_at)}</span>
                  </div>
                </div>
              </li>
            )
          })}
        </ul>
      )}
    </Card>
  )
}
