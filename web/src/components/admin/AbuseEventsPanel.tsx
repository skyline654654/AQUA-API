/**
 * 管理后台：异常用量事件列表（盗 Key / 突发流量检测）。
 *
 * 意图（Why）：
 *   访问令牌一旦外泄，损失由站长承担。这里给出"什么时候、被谁、以什么方式用的"
 *   的证据链，供站长判断是盗刷还是正常业务高峰。
 *
 * 设计说明：
 *   - **"度量值 / 阈值"成对展示**。只给度量值会看不出"是不是阈值定太严了"；
 *     两者并列让站长能顺手校准检测灵敏度。
 *   - 事件是**只读留痕**，不提供一键限流按钮。自动处置的误伤代价高，
 *     且封禁动作应走既有的令牌/风控入口，避免这里变成第二套处置逻辑。
 *   - 严重级别用色调区分（严重=红 / 警告=橙 / 提示=蓝），
 *     站长扫一眼就能定位需要处理的行。
 */
'use client'

import { useCallback, useEffect, useState } from 'react'

import { listAbuseEvents } from '@/api/admin'
import type { AbuseEvent } from '@/api/types'
import { Badge } from '@/components/ui/Display'
import { DataTable, type Column } from '@/components/ui/Table'
import { formatDateTime, formatNumber } from '@/utils/format'

/** 严重级别 → 徽标色调。level 缺失时按"提示"处理，不让未知值把界面搞崩。 */
function severityTone(level: number): 'err' | 'warn' | 'info' {
  if (level >= 3) return 'err'
  if (level === 2) return 'warn'
  return 'info'
}

/** 严重级别的中文名 */
function severityText(level: number): string {
  if (level >= 3) return '严重'
  if (level === 2) return '警告'
  return '提示'
}

export function AbuseEventsPanel({ limit = 50 }: { limit?: number }) {
  const [rows, setRows] = useState<AbuseEvent[] | null>(null)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setError('')
    try {
      const res = await listAbuseEvents(limit)
      setRows(res.events ?? [])
    } catch (e) {
      setError(e instanceof Error ? e.message : '加载异常事件失败')
      setRows([])
    }
  }, [limit])

  useEffect(() => {
    void load()
  }, [load])

  const columns: Column<AbuseEvent>[] = [
    {
      title: '时间',
      render: (row) => <span className="text-ink-2">{formatDateTime(row.created_at)}</span>,
      width: 'w-40',
    },
    {
      title: '用户',
      render: (row) => (
        <span className="text-ink">{row.username || `#${row.user_id}`}</span>
      ),
      width: 'w-32',
    },
    {
      title: '类型',
      render: (row) => <span className="text-ink-2">{row.kind_text}</span>,
      width: 'w-28',
    },
    {
      title: '级别',
      render: (row) => <Badge tone={severityTone(row.severity)}>{severityText(row.severity)}</Badge>,
      width: 'w-20',
    },
    {
      title: '说明',
      render: (row) => (
        <span className="text-[13px] text-ink-2">{row.detail || '—'}</span>
      ),
    },
    {
      // 度量值与阈值成对：缺了阈值就看不出"是不是阈值太严"
      title: '度量 / 阈值',
      align: 'right',
      render: (row) => (
        <span className="tabular-nums text-ink-2">
          {formatNumber(row.metric)} / {formatNumber(row.threshold)}
        </span>
      ),
      width: 'w-36',
    },
  ]

  return (
    <div className="space-y-3">
      <div>
        <h2 className="text-[15px] font-semibold text-ink">异常用量</h2>
        <p className="mt-0.5 text-[13px] text-ink-3">
          请求量相对用户自身历史异常突增等检测结果（系统只记录，不自动处置）
        </p>
      </div>

      {error && (
        <div className="rounded-md border border-err/25 bg-err/5 px-3 py-2.5 text-[13px] text-err">
          {error}
        </div>
      )}

      <DataTable
        columns={columns}
        rows={rows}
        rowKey={(row) => row.id}
        loading={rows === null}
        emptyTitle="暂无异常记录"
        emptyDescription="系统会按周期比对各用户的请求量与其自身历史分布，未发现异常时这里为空。"
      />
    </div>
  )
}
