/**
 * 棋盘组件：用 SVG 绘制三种棋的棋盘，并支持点击落子。
 *
 * 意图（Why）：
 *   前端棋盘服务于两件事：
 *     1) 让用户看清局势（与模型看到的是同一个局面）；
 *     2) 人机对弈时，让用户点一下就把着法填出来——比手打坐标可靠得多
 *        （手打"G7"很容易打成"H7"，而落子的位置是所见即所得）。
 *
 *   刻意不用 Canvas：SVG 的每个棋子都是 DOM 节点，因此
 *     · 可以跟随主题变量自动换色（Canvas 需要重绘）；
 *     · 可以给棋子挂 title/aria，无障碍可读；
 *     · 天然随容器缩放（viewBox），不需要处理 DPR。
 *   本组件不是性能敏感的（最多 361 个节点），这些好处都值得。
 *
 * 与后端图片的关系：
 *   后端 PNG 是【给模型看的】（那是演示的核心证据），
 *   本组件是【给用户看的+交互的】。两者都从同一份 position 数据渲染，
 *   因此语义一致；但视觉上刻意不完全相同——PNG 用木色棋盘模拟实体棋具，
 *   这里用主题色以融入界面，避免在深色主题下突然出现一块刺眼的木色。
 *
 * 流转（Flow）：
 *   /console/arena → <GameBoard position onPick />
 *     └─ 按 kind 计算几何（格线数 / 星位 / 九宫 / 河界）
 *         → 渲染棋子 → 点击换算成交叉点坐标 → onPick(x, y)
 *
 * 扩展（Extend）：
 *   新增棋种：在 GEOMETRY 里补一行几何参数，并在 extraLines 里补该棋种
 *   特有的线条（如象棋九宫）。棋子绘制按 kind 分支即可。
 */
'use client'

import { useMemo } from 'react'

import type { GamePosition } from '@/api/types'

/** 各棋种的棋盘几何（单位：交叉点数量，与后端一致）。 */
const GEOMETRY: Record<string, { cols: number; rows: number }> = {
  gomoku: { cols: 15, rows: 15 },
  go: { cols: 19, rows: 19 },
  xiangqi: { cols: 9, rows: 10 },
}

/** 象棋棋子类型 → 汉字（红黑写法不同，与后端渲染保持同一套对应）。 */
const XIANGQI_GLYPH: Record<number, { black: string; red: string }> = {
  2: { black: '将', red: '帅' },
  3: { black: '士', red: '仕' },
  4: { black: '象', red: '相' },
  5: { black: '马', red: '马' },
  6: { black: '车', red: '车' },
  7: { black: '砲', red: '炮' },
  8: { black: '卒', red: '兵' },
}

/** 围棋星位（9 个），坐标为 0 基。 */
const GO_STARS: Array<[number, number]> = [
  [3, 3], [9, 3], [15, 3],
  [3, 9], [9, 9], [15, 9],
  [3, 15], [9, 15], [15, 15],
]

/** 视图内的边长（用户单位）。实际显示尺寸由外层容器决定。 */
const VIEW = 100

interface GameBoardProps {
  position: GamePosition
  /** 是否允许点击落子（人机对弈轮到人类时开启）。 */
  interactive?: boolean
  /** 点击某个交叉点（0 基坐标）。 */
  onPick?: (x: number, y: number) => void
  /** 已选中待走的棋子（象棋：先点起点再点终点）。 */
  selected?: { x: number; y: number } | null
  className?: string
}

/**
 * 棋盘主体。
 *
 * 坐标换算：SVG 的 viewBox 是 0..VIEW，棋盘区域向内留出 PAD，
 * 交叉点 i 的位置 = PAD + i * STEP。点击时反向换算并四舍五入到最近交叉点。
 */
export function GameBoard({
  position,
  interactive = false,
  onPick,
  selected = null,
  className = '',
}: GameBoardProps) {
  const geo = GEOMETRY[position.kind] ?? { cols: position.width, rows: position.height }
  // 象棋格子更"扁"（9×10 是竖长方形），因此横纵按比例分配：
  // 都按正方形排会让象棋棋盘显得过宽，与实体棋盘比例不符。
  const pad = 6
  const stepX = (VIEW - pad * 2) / (geo.cols - 1)
  const stepY = (VIEW - pad * 2) / (geo.rows - 1)

  const px = (x: number) => pad + x * stepX
  const py = (y: number) => pad + y * stepY

  /** 最后一手的目标点（用于高亮） */
  const lastTarget = useMemo(() => {
    const lm = position.last_move
    if (!lm) return null
    if (position.kind === 'xiangqi' && lm.to) return lm.to
    if (lm.x !== undefined && lm.y !== undefined) return { x: lm.x, y: lm.y }
    return null
  }, [position])

  /** 点是否在棋盘内 */
  const inBoard = (x: number, y: number) =>
    x >= 0 && y >= 0 && x < geo.cols && y < geo.rows

  /** 点击换算成交叉点 */
  function handleClick(evt: React.MouseEvent<SVGSVGElement>) {
    if (!interactive || !onPick) return
    const rect = evt.currentTarget.getBoundingClientRect()
    // 视口坐标 → 用户单位（viewBox 坐标）
    const ux = ((evt.clientX - rect.left) / rect.width) * VIEW
    const uy = ((evt.clientY - rect.top) / rect.height) * VIEW
    const gx = Math.round((ux - pad) / stepX)
    const gy = Math.round((uy - pad) / stepY)
    if (!inBoard(gx, gy)) return
    onPick(gx, gy)
  }

  /** 棋子半径（用户单位）：取格距的 42%，保证相邻棋子之间有可见缝隙。 */
  const radius = Math.min(stepX, stepY) * 0.42

  return (
    <svg
      viewBox={`0 0 ${VIEW} ${VIEW}`}
      className={`${className} ${interactive ? 'cursor-pointer' : ''}`}
      onClick={handleClick}
      role="img"
      aria-label={`${position.kind} 棋盘`}
    >
      {/* 棋盘底色 */}
      <rect x="0" y="0" width={VIEW} height={VIEW} rx="2" className="fill-card" />

      {/* 格线 */}
      <g className="stroke-line-2" strokeWidth="0.3">
        {Array.from({ length: geo.rows }).map((_, r) => (
          <line key={`h${r}`} x1={px(0)} y1={py(r)} x2={px(geo.cols - 1)} y2={py(r)} />
        ))}
        {Array.from({ length: geo.cols }).map((_, c) => {
          // 象棋河界：中间列在楚河汉界处断开（传统画法）
          if (position.kind === 'xiangqi' && c !== 0 && c !== geo.cols - 1) {
            return (
              <g key={`v${c}`}>
                <line x1={px(c)} y1={py(0)} x2={px(c)} y2={py(4)} />
                <line x1={px(c)} y1={py(5)} x2={px(c)} y2={py(geo.rows - 1)} />
              </g>
            )
          }
          return <line key={`v${c}`} x1={px(c)} y1={py(0)} x2={px(c)} y2={py(geo.rows - 1)} />
        })}
      </g>

      {/* 外框加粗：外框是棋盘边界，细一线会让整体显得"没画完" */}
      <rect
        x={px(0)}
        y={py(0)}
        width={px(geo.cols - 1) - px(0)}
        height={py(geo.rows - 1) - py(0)}
        fill="none"
        className="stroke-line-2"
        strokeWidth="0.7"
      />

      {/* 围棋星位 */}
      {position.kind === 'go' &&
        GO_STARS.map(([sx, sy]) => (
          <circle key={`star-${sx}-${sy}`} cx={px(sx)} cy={py(sy)} r="0.8" className="fill-line-2" />
        ))}

      {/* 象棋九宫斜线 */}
      {position.kind === 'xiangqi' && (
        <g className="stroke-line-2" strokeWidth="0.3">
          <line x1={px(3)} y1={py(0)} x2={px(5)} y2={py(2)} />
          <line x1={px(5)} y1={py(0)} x2={px(3)} y2={py(2)} />
          <line x1={px(3)} y1={py(7)} x2={px(5)} y2={py(9)} />
          <line x1={px(5)} y1={py(7)} x2={px(3)} y2={py(9)} />
        </g>
      )}

      {/* 棋子 */}
      {Array.from({ length: geo.rows }).map((_, y) =>
        Array.from({ length: geo.cols }).map((_, x) => {
          const color = position.cells[y * geo.cols + x] ?? 0
          if (color === 0) return null
          const cx = px(x)
          const cy = py(y)
          return (
            <XiangqiOrStone
              key={`p-${x}-${y}`}
              kind={position.kind}
              color={color}
              piece={position.pieces?.[y * geo.cols + x] ?? 0}
              cx={cx}
              cy={cy}
              radius={radius}
            />
          )
        }),
      )}

      {/* 选中提示（象棋：起点已选） */}
      {selected && (
        <circle
          cx={px(selected.x)}
          cy={py(selected.y)}
          r={radius * 1.15}
          fill="none"
          className="stroke-brand"
          strokeWidth="0.6"
          strokeDasharray="1.5 1"
        />
      )}

      {/* 最后一手标记：红框，与后端图片上的标记保持同一语义 */}
      {lastTarget && inBoard(lastTarget.x, lastTarget.y) && (
        <rect
          x={px(lastTarget.x) - radius * 1.15}
          y={py(lastTarget.y) - radius * 1.15}
          width={radius * 2.3}
          height={radius * 2.3}
          fill="none"
          className="stroke-err"
          strokeWidth="0.6"
        />
      )}
    </svg>
  )
}

/** 按棋种绘制一个棋子。 */
function XiangqiOrStone({
  kind,
  color,
  piece,
  cx,
  cy,
  radius,
}: {
  kind: string
  color: number
  piece: number
  cx: number
  cy: number
  radius: number
}) {
  // 象棋：浅色圆盘 + 汉字，红黑两方用不同字色（与后端 PNG 同一套映射）
  if (kind === 'xiangqi') {
    const glyph = XIANGQI_GLYPH[piece]
    const text = color === 1 ? glyph?.black : glyph?.red
    const ink = color === 1 ? 'fill-ink' : 'fill-err'
    return (
      <g>
        <circle cx={cx} cy={cy} r={radius} className="fill-card" />
        <circle
          cx={cx}
          cy={cy}
          r={radius}
          fill="none"
          className={color === 1 ? 'stroke-ink' : 'stroke-err'}
          strokeWidth="0.5"
        />
        <circle
          cx={cx}
          cy={cy}
          r={radius * 0.78}
          fill="none"
          className={color === 1 ? 'stroke-ink' : 'stroke-err'}
          strokeWidth="0.2"
        />
        {text && (
          <text
            x={cx}
            y={cy}
            textAnchor="middle"
            dominantBaseline="central"
            className={ink}
            style={{ fontSize: `${radius * 1.15}px`, fontWeight: 600 }}
          >
            {text}
          </text>
        )}
      </g>
    )
  }

  // 五子棋 / 围棋：实心圆。白子描边，否则在浅色棋盘上看不见边界。
  return color === 1 ? (
    <circle cx={cx} cy={cy} r={radius} className="fill-ink" />
  ) : (
    <circle cx={cx} cy={cy} r={radius} className="fill-card stroke-ink" strokeWidth="0.4" />
  )
}
