/**
 * 管理后台：异常用量（/admin/abuse-events）。
 *
 * 意图（Why）：
 *   访问令牌外泄后，损失由站长承担。本页把检测结果（请求量相对用户自身历史
 *   异常突增）列出来，回答「什么时候、被谁、以什么方式用的」，
 *   供站长判断是盗刷还是正常业务高峰。
 *
 * 流转（Flow）：
 *   本页 → <AbuseEventsPanel /> → listAbuseEvents(limit)
 *        → GET /api/admin/abuse-events（只读，系统不自动处置）
 *
 * 扩展（Extend）：
 *   需要处置动作（限流 / 停用令牌）时，在本页加操作入口并复用
 *   令牌管理已有的接口——不要在这里另造一套处置逻辑，
 *   否则"页面显示的处置"与"令牌实际状态"会出现分叉。
 */
'use client'

import { AbuseEventsPanel } from '@/components/admin/AbuseEventsPanel'

export default function AdminAbuseEventsPage() {
  return (
    <div className="space-y-4">
      <AbuseEventsPanel />
    </div>
  )
}
