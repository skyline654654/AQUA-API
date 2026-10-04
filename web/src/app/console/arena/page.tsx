/**
 * 对弈演示页（/console/arena）。
 *
 * 意图（Why）：
 *   这是"多模态"能力的可视化证明。用户看到的不该只是一句"我们的模型支持图片"，
 *   而应该是：两个模型对着一块【真实渲染出的棋盘图】轮流落子，
 *   每一手都能看到"模型看到了什么、它说了什么、我们怎么判的"。
 *
 *   因此页面上并列展示三样东西（缺一不可）：
 *     1) 交互棋盘（用户看的、人机模式下用来落子）；
 *     2) "模型看到的图"（后端渲染的那张 PNG）——这是演示可信度的来源，
 *        用户可以亲眼确认喂进去的局面没有错；
 *     3) 棋谱（含每次的原始输出与尝试次数）——"模型错在哪"才是结论本身。
 *
 * 双模式的差异只在"轮次由谁决定"：
 *   · AI 对 AI：点一次"走一步"，后端去问当前该谁走的那个模型；
 *   · 人机对弈：轮到人类时点棋盘落子，轮到 AI 时与前者相同。
 *   两种模式共用同一套推进逻辑（一个循环 + 一个按钮），
 *   避免为两种模式各写一套流程（那必然出现行为差异）。
 *
 * 为什么是"手动推进 / 自动连走"两种驱动而不是只有自动：
 *   自动连走看得爽（这是演示的主场景），但用户常常想逐步看清某一手；
 *   而且自动连走一旦出错（模型卡住），手动模式是唯一能继续观察的手段。
 *
 * 流转（Flow）：
 *   本页 → createGame（建局）→ 循环 stepGame（每手一次模型调用）
 *     · 人类回合同样走 stepGame，只是带上 move
 *     · 每步成功后刷新棋盘、棋谱与"模型看到的图"
 *
 * 扩展（Extend）：
 *   想加"自动跑完整局并导出棋谱"：复用 runAuto 的循环，
 *   在结束时把 moves 组装成文本下载即可。
 */
'use client'

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import {
  createGame,
  deleteGame,
  fetchGame,
  fetchGameBoard,
  listGames,
  stepGame,
} from '@/api/portal'
import type { GameDetail, GameMatch, GameMoveRow } from '@/api/types'
import { GameBoard } from '@/components/game/GameBoard'
import { Button } from '@/components/ui/Button'
import { Badge, Card, EmptyState } from '@/components/ui/Display'
import { Field, Input, Select } from '@/components/ui/Form'

/** 可选棋种（顺序即展示顺序，与后端 AllKinds 一致）。 */
const KINDS = [
  { value: 'gomoku', label: '五子棋', hint: '15 路，先连五子者胜。规则最简单，适合先试。' },
  { value: 'go', label: '围棋', hint: '19 路，含提子与劫争；不数子，认输或双方停一手终局。' },
  { value: 'xiangqi', label: '象棋', hint: '9×10，标准中文棋子；吃掉对方将/帅者胜。' },
] as const

/** 自动连走的安全上限（后端另有 200 手上限，这里更保守以便快速演示）。 */
const AUTO_MAX_STEPS = 60

export default function ArenaPage() {
  // ── 建局表单 ──
  const [kind, setKind] = useState<string>('gomoku')
  const [mode, setMode] = useState<string>('ai_vs_ai')
  const [blackModel, setBlackModel] = useState('')
  const [whiteModel, setWhiteModel] = useState('')
  const [humanColor, setHumanColor] = useState<number>(1)
  const [models, setModels] = useState<string[]>([])

  // ── 对局状态 ──
  const [match, setMatch] = useState<GameMatch | null>(null)
  const [moves, setMoves] = useState<GameMoveRow[]>([])
  const [history, setHistory] = useState<GameMatch[]>([])
  const [busy, setBusy] = useState(false)
  const [autoRunning, setAutoRunning] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  /** 象棋：已选中的起点（先点起点再点终点） */
  const [selected, setSelected] = useState<{ x: number; y: number } | null>(null)
  /** 人类输入框（除了点棋盘，也允许直接打坐标） */
  const [manualMove, setManualMove] = useState('')

  const autoRef = useRef(false)
  /** 「模型看到的画面」的 object URL（由 blob 转出） */
  const [boardImage, setBoardImage] = useState('')
  const [boardImageError, setBoardImageError] = useState('')

  const kindHint = useMemo(
    () => KINDS.find((k) => k.value === kind)?.hint ?? '',
    [kind],
  )

  /** 载入可用模型清单与历史对局。 */
  useEffect(() => {
    // 模型清单取自公开的模型列表（与游乐场同一来源），只取名字供选择。
    void fetch('/v1/models')
      .then((r) => (r.ok ? r.json() : null))
      .then((data) => {
        const list: string[] = Array.isArray(data?.data)
          ? data.data.map((m: { id?: string }) => m.id).filter(Boolean)
          : []
        setModels(list)
        if (list.length > 0) {
          setBlackModel((prev) => prev || list[0])
          setWhiteModel((prev) => prev || list[Math.min(1, list.length - 1)])
        }
      })
      .catch(() => setModels([]))
    void listGames()
      .then((r) => setHistory(r.matches ?? []))
      .catch(() => setHistory([]))
  }, [])

  /**
   * 载入「模型看到的画面」。
   *
   * 走 blob 而不是把 URL 塞给 <img src>：会话鉴权只认 Authorization 头，
   * 而 <img> 不会带自定义头，直接引 URL 会拿到 401 与一张破图。
   *
   * 必须负责回收上一张的 object URL：不回收的话每走一手就泄漏一张图片，
   * 一盘围棋几百手下来是很实在的内存占用。
   */
  useEffect(() => {
    if (!match) {
      setBoardImage('')
      return
    }
    let revoked = false
    let created = ''
    void fetchGameBoard(match.id, match.updated_at)
      .then((blob) => {
        if (revoked) return
        created = URL.createObjectURL(blob)
        setBoardImage((prev) => {
          if (prev) URL.revokeObjectURL(prev)
          return created
        })
        setBoardImageError('')
      })
      .catch((e) => {
        setBoardImageError(e instanceof Error ? e.message : '载入棋盘图失败')
      })
    return () => {
      revoked = true
      // 注意：这里不能直接 revoke created——它可能已经被 setBoardImage 接管，
      // 而在 effect 清理时立刻撤销会让 <img> 正在用的 URL 失效（图闪一下变破图）。
      // 交给下一次 setBoardImage 的 prev 回收即可。
    }
  }, [match])

  /** 刷新某个对局的棋盘与棋谱。 */
  const refresh = useCallback(async (id: number) => {
    const detail: GameDetail = await fetchGame(id)
    setMatch(detail.match)
    setMoves(detail.moves ?? [])
  }, [])

  async function handleCreate() {
    setError('')
    setNotice('')
    setSelected(null)
    setManualMove('')
    setBusy(true)
    try {
      const created = await createGame({
        kind,
        mode,
        black_model: mode === 'ai_vs_ai' ? blackModel : humanColor === 1 ? '' : blackModel,
        white_model: mode === 'ai_vs_ai' ? whiteModel : humanColor === 2 ? '' : whiteModel,
        human_color: mode === 'human_vs_ai' ? humanColor : 0,
      })
      setMatch(created)
      setMoves([])
      setNotice('对局已创建。点「走一步」开始，或点「自动连走」让它自己下。')
      void listGames().then((r) => setHistory(r.matches ?? [])).catch(() => undefined)
    } catch (e) {
      setError(e instanceof Error ? e.message : '创建对局失败')
    } finally {
      setBusy(false)
    }
  }

  /**
   * 推进一手。
   *
   * 人类回合时把 move 一起发出去（后端据此走人类路径）。
   * 返回值用于告诉调用方"对局是否还在继续"，自动连走据此决定是否继续循环。
   */
  const stepOnce = useCallback(
    async (moveText?: string): Promise<boolean> => {
      if (!match) return false
      const res = await stepGame(match.id, moveText)
      setMatch(res.match)
      if (res.moves?.length) {
        setMoves((prev) => [...prev, ...res.moves])
      }
      if (res.error) {
        // 中止原因如实展示：要让用户看到"模型给不出合法着法"或"调用失败"，
        // 而不是笼统的"出错了"——这两者的含义完全不同。
        setNotice(res.aborted ? `对局已中止：${res.error}` : res.error)
      }
      return !res.over && !res.aborted
    },
    [match],
  )

  async function handleStep() {
    if (!match || busy) return
    setError('')
    setNotice('')
    setSelected(null)
    setBusy(true)
    try {
      const move = match.awaiting_human ? manualMove.trim() : undefined
      if (match.awaiting_human && !move) {
        setError('轮到你落子：点击棋盘，或在下方直接输入坐标（如 H8）。')
        return
      }
      setManualMove('')
      await stepOnce(move)
    } catch (e) {
      setError(e instanceof Error ? e.message : '推进对局失败')
    } finally {
      setBusy(false)
    }
  }

  /** 自动连走：循环推进直到对局结束、达到上限、或需要人类落子。 */
  async function handleAuto() {
    if (!match || busy || autoRunning) return
    setError('')
    setNotice('')
    setAutoRunning(true)
    autoRef.current = true
    setBusy(true)
    try {
      for (let i = 0; i < AUTO_MAX_STEPS; i += 1) {
        if (!autoRef.current) break
        // 需要人类时停下——自动连走不能代替人下棋。
        const current = await fetchGame(match.id)
        if (current.match.awaiting_human) {
          setMatch(current.match)
          setMoves(current.moves ?? [])
          setNotice('轮到你落子了（自动连走已暂停）。')
          break
        }
        const alive = await stepOnce()
        if (!alive) break
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : '自动推进中断')
    } finally {
      autoRef.current = false
      setAutoRunning(false)
      setBusy(false)
    }
  }

  function handleStopAuto() {
    autoRef.current = false
    setAutoRunning(false)
  }

  /** 棋盘点击：象棋走"起点→终点"两步，其余棋种一次落子。 */
  function handlePick(x: number, y: number) {
    if (!match || !match.awaiting_human || busy) return
    const pos = match.position
    if (!pos) return

    if (match.kind === 'xiangqi') {
      const piece = pos.cells[y * pos.width + x] ?? 0
      // 已选中起点时，第二次点击作为终点
      if (selected) {
        if (selected.x === x && selected.y === y) {
          setSelected(null)
          return
        }
        setManualMove(
          `${coordName(selected.x, selected.y)}${coordName(x, y)}`,
        )
        setSelected(null)
        return
      }
      // 未选中时，只能点自己那一方的棋子——否则用户点了半天也不知道为什么没反应。
      const myColor = match.human_color
      if (piece === myColor) {
        setSelected({ x, y })
        return
      }
      setNotice('请先点击你自己的一方棋子作为起点。')
      return
    }

    // 落子类棋种（五子棋 / 围棋）
    if ((pos.cells[y * pos.width + x] ?? 0) !== 0) {
      setNotice('该位置已有棋子。')
      return
    }
    setManualMove(coordName(x, y))
  }

  async function handleOpen(id: number) {
    setError('')
    setNotice('')
    setSelected(null)
    setManualMove('')
    setBusy(true)
    try {
      await refresh(id)
    } catch (e) {
      setError(e instanceof Error ? e.message : '读取对局失败')
    } finally {
      setBusy(false)
    }
  }

  async function handleDelete(id: number) {
    try {
      await deleteGame(id)
      if (match?.id === id) {
        setMatch(null)
        setMoves([])
      }
      const r = await listGames()
      setHistory(r.matches ?? [])
    } catch (e) {
      setError(e instanceof Error ? e.message : '删除失败')
    }
  }

  return (
    <div className="space-y-4">
      <header>
        <h1 className="text-lg font-semibold text-ink">AI 对弈</h1>
        <p className="mt-1 text-[13px] leading-relaxed text-ink-3">
          让两个模型下一盘棋。棋盘会渲染成【图片】发给模型 ——
          它必须先从像素里认出棋子与位置，这正是多模态能力最直观的检验。
          每一步都会真实调用模型并消耗额度。
        </p>
      </header>

      {error && (
        <div className="rounded-md border border-err/25 bg-err/5 px-3 py-2.5 text-[13px] text-err">
          {error}
        </div>
      )}
      {notice && (
        <div className="rounded-md border border-brand/25 bg-brand/5 px-3 py-2.5 text-[13px] text-ink-2">
          {notice}
        </div>
      )}

      {/* ── 开局 ── */}
      <Card>
        <div className="grid gap-4 md:grid-cols-4">
          <Field label="棋种" htmlFor="arena-kind">
            <Select id="arena-kind" value={kind} onChange={(e) => setKind(e.target.value)}>
              {KINDS.map((k) => (
                <option key={k.value} value={k.value}>
                  {k.label}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="模式" htmlFor="arena-mode">
            <Select
              id="arena-mode"
              value={mode}
              onChange={(e) => setMode(e.target.value)}
            >
              <option value="ai_vs_ai">AI 对 AI</option>
              <option value="human_vs_ai">人机对弈</option>
            </Select>
          </Field>
          {mode === 'ai_vs_ai' ? (
            <>
              <Field label="黑方模型" htmlFor="arena-black">
                <ModelInput id="arena-black" value={blackModel} onChange={setBlackModel} models={models} />
              </Field>
              <Field label="白方模型" htmlFor="arena-white">
                <ModelInput id="arena-white" value={whiteModel} onChange={setWhiteModel} models={models} />
              </Field>
            </>
          ) : (
            <>
              <Field label="你执" htmlFor="arena-human">
                <Select
                  id="arena-human"
                  value={String(humanColor)}
                  onChange={(e) => setHumanColor(Number(e.target.value))}
                >
                  <option value="1">黑方（先手）</option>
                  <option value="2">白方（后手）</option>
                </Select>
              </Field>
              <Field label="AI 模型" htmlFor="arena-ai">
                <ModelInput
                  id="arena-ai"
                  value={humanColor === 1 ? whiteModel : blackModel}
                  onChange={humanColor === 1 ? setWhiteModel : setBlackModel}
                  models={models}
                />
              </Field>
            </>
          )}
        </div>
        <p className="mt-3 text-xs text-ink-3">{kindHint}</p>
        <div className="mt-3 flex items-center gap-2">
          <Button variant="primary" onClick={handleCreate} loading={busy && !match}>
            开始新对局
          </Button>
          {models.length === 0 && (
            <span className="text-xs text-warn">
              未读取到可用模型清单，请手动输入模型名。
            </span>
          )}
        </div>
      </Card>

      {/* ── 对局进行中 ── */}
      {match && (
        <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,320px)]">
          <Card>
            <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
              <div className="flex flex-wrap items-center gap-2">
                <Badge tone="brand">{match.kind_label}</Badge>
                <Badge tone="off">{match.mode_label}</Badge>
                <Badge
                  tone={
                    match.status === 'playing'
                      ? match.awaiting_human
                        ? 'warn'
                        : 'info'
                      : match.status === 'aborted'
                        ? 'err'
                        : 'ok'
                  }
                >
                  {match.status_text}
                  {match.status === 'playing' &&
                    (match.awaiting_human ? '· 等你落子' : '· AI 思考中')}
                </Badge>
                <span className="text-xs text-ink-3">第 {match.move_count} 手</span>
              </div>
              <div className="flex items-center gap-2">
                {autoRunning ? (
                  <Button size="sm" variant="danger" onClick={handleStopAuto}>
                    停止连走
                  </Button>
                ) : (
                  <Button size="sm" variant="secondary" onClick={handleAuto} loading={busy && autoRunning}>
                    自动连走
                  </Button>
                )}
                <Button size="sm" variant="primary" onClick={handleStep} loading={busy && !autoRunning}>
                  走一步
                </Button>
              </div>
            </div>

            {match.position && (
              <GameBoard
                position={match.position}
                interactive={match.awaiting_human && !busy}
                onPick={handlePick}
                selected={selected}
                className="mx-auto block w-full max-w-[540px]"
              />
            )}

            {/* 人类输入：点棋盘为主，手打坐标为辅（象棋记谱习惯的人会想直接打） */}
            {match.awaiting_human && (
              <div className="mt-3 flex flex-wrap items-end gap-2">
                <div className="min-w-[180px] flex-1">
                  <Field
                    label="你的着法"
                    htmlFor="arena-move"
                    help={
                      match.kind === 'xiangqi'
                        ? '点棋盘选起点与终点，或直接输入如 H1H4'
                        : '点棋盘落子，或直接输入如 H8'
                    }
                  >
                    <Input
                      id="arena-move"
                      value={manualMove}
                      placeholder={match.kind === 'xiangqi' ? 'H1H4' : 'H8'}
                      onChange={(e) => setManualMove(e.target.value)}
                    />
                  </Field>
                </div>
                <Button variant="primary" onClick={handleStep} loading={busy}>
                  落子
                </Button>
              </div>
            )}

            {match.error_text && (
              <div className="mt-3 rounded-md border border-err/25 bg-err/5 px-3 py-2 text-xs text-err">
                中止原因：{match.error_text}
              </div>
            )}
          </Card>

          <div className="space-y-4">
            {/* 模型看到的图：演示可信度的关键证据 */}
            <Card padding="none">
              <div className="border-b border-line px-4 py-2.5">
                <div className="text-[13px] font-medium text-ink">模型看到的画面</div>
                <div className="mt-0.5 text-xs text-ink-3">
                  发给模型的就是这张图（服务端渲染）
                </div>
              </div>
              {boardImageError ? (
                <div className="px-4 py-6 text-center text-xs text-err">
                  载入棋盘图失败：{boardImageError}
                </div>
              ) : boardImage ? (
                // eslint-disable-next-line @next/next/no-img-element
                <img src={boardImage} alt="模型看到的棋盘" className="block w-full" />
              ) : (
                <div className="px-4 py-6 text-center text-xs text-ink-3">渲染中…</div>
              )}
            </Card>

            {/* 棋谱 */}
            <Card padding="none">
              <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
                <div className="text-[13px] font-medium text-ink">棋谱</div>
                <span className="text-xs text-ink-3">{moves.length} 手</span>
              </div>
              <div className="max-h-[420px] overflow-y-auto">
                {moves.length === 0 ? (
                  <div className="px-4 py-6 text-center text-xs text-ink-3">
                    还没有落子
                  </div>
                ) : (
                  <ul className="divide-y divide-line">
                    {moves.map((m) => (
                      <li key={m.seq} className="px-4 py-2">
                        <div className="flex items-center justify-between gap-2">
                          <span className="text-[13px] text-ink">
                            <span className="mr-2 tabular-nums text-ink-3">{m.seq}.</span>
                            {m.color_text} {m.notation}
                          </span>
                          {/* 尝试次数 > 1 是"模型看图吃力"的直接信号，值得显眼 */}
                          {m.attempts > 1 && (
                            <Badge tone="warn">试了 {m.attempts} 次</Badge>
                          )}
                        </div>
                        {m.raw && (
                          <div
                            className="mt-1 truncate text-xs text-ink-3"
                            title={m.raw}
                          >
                            {m.raw}
                          </div>
                        )}
                        {m.error_text && (
                          <div className="mt-1 text-xs text-err">{m.error_text}</div>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </Card>
          </div>
        </div>
      )}

      {/* ── 历史对局 ── */}
      <Card>
        <div className="mb-3 text-[13px] font-medium text-ink">我的对局</div>
        {history.length === 0 ? (
          <EmptyState title="还没有对局" description="在上方创建一局，让它替你下完。" />
        ) : (
          <ul className="divide-y divide-line">
            {history.map((h) => (
              <li key={h.id} className="flex flex-wrap items-center justify-between gap-2 py-2.5">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-1.5">
                    <span className="text-[13px] text-ink">
                      #{h.id} {h.kind_label}
                    </span>
                    <Badge tone="off">{h.mode_label}</Badge>
                    <Badge tone={h.status === 'playing' ? 'info' : h.status === 'aborted' ? 'err' : 'ok'}>
                      {h.status_text}
                    </Badge>
                    <span className="text-xs text-ink-3">{h.move_count} 手</span>
                  </div>
                  <div className="mt-0.5 truncate text-xs text-ink-3">
                    黑 {h.black_model || '人类'} · 白 {h.white_model || '人类'}
                  </div>
                </div>
                <div className="flex gap-1.5">
                  <Button size="sm" variant="secondary" onClick={() => handleOpen(h.id)}>
                    查看
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => handleDelete(h.id)}>
                    删除
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  )
}

/** 模型名输入：有清单时给下拉，没有时退化为文本输入。 */
function ModelInput({
  id,
  value,
  onChange,
  models,
}: {
  id: string
  value: string
  onChange: (v: string) => void
  models: string[]
}) {
  if (models.length === 0) {
    return (
      <Input
        id={id}
        value={value}
        placeholder="模型名，如 deepseek-chat"
        onChange={(e) => onChange(e.target.value)}
      />
    )
  }
  return (
    <Select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">请选择</option>
      {models.map((m) => (
        <option key={m} value={m}>
          {m}
        </option>
      ))}
    </Select>
  )
}

/** 把 0 基坐标渲染成棋盘标注（列字母跳过 I，行号从上方 1 起）。 */
function coordName(x: number, y: number): string {
  const letters = 'ABCDEFGHJKLMNOPQRSTUVWXYZ'
  const col = x >= 0 && x < letters.length ? letters[x] : '?'
  return `${col}${y + 1}`
}
