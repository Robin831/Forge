import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { ExternalLink, Loader2, Maximize2, RotateCcw, X } from 'lucide-react'
import { Link } from 'react-router-dom'
import { ApiError, fetchBeadDeps, type BeadBrief, type BeadDepsResponse } from '../api'
import { useAuth } from '../auth'
import { priorityClasses, priorityLabel } from '../lib/format'

const STATUS_BADGE: Record<string, string> = {
  done: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
  timeout: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  running: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  pending: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
  open: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  in_progress: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  closed: 'bg-slate-700/60 text-slate-300 border-slate-600/60',
}

function badgeClass(s: string): string {
  return STATUS_BADGE[s] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

// MAX_DEPTH matches the backend's maxDepDepth on /api/bead/{id}/deps. The
// endpoint clamps higher values, so requesting depth=3 yields three hops in
// each direction (blocks downstream, blocked_by upstream).
const MAX_DEPTH = 3

export interface DepsGraphViewProps {
  open: boolean
  root: BeadBrief | null
  onClose: () => void
}

interface GraphNode {
  id: string // unique per-occurrence key for React rendering and DOM refs
  beadID: string
  anvil?: string
  title: string
  status: string
  priority: number
  level: number
  isRoot: boolean
  // outermost flags whether this node sits at the depth-3 ring in either
  // direction. Used to render the "Expand" affordance because anything past
  // the limit is not in the current response.
  outermost: boolean
}

interface GraphEdge {
  fromKey: string
  toKey: string
}

interface EdgeCoord {
  key: string
  x1: number
  y1: number
  x2: number
  y2: number
}

// buildGraph walks the deps tree from the API into a level-keyed map and a
// flat edge list. Negative levels are upstream (blocked_by), positive levels
// are downstream (blocks); level 0 is the root. Nodes are assigned a unique
// per-occurrence id so that the same bead appearing in multiple branches of a
// diamond graph renders at its correct hop/level rather than being collapsed
// to whichever branch was visited first.
function buildGraph(
  root: BeadBrief,
  deps: BeadDepsResponse | null,
): { levels: Map<number, GraphNode[]>; edges: GraphEdge[] } {
  const levels = new Map<number, GraphNode[]>()
  const edges: GraphEdge[] = []
  let seq = 0

  const push = (n: Omit<GraphNode, 'id'>): string => {
    const id = `${n.beadID}|${n.anvil ?? ''}|${seq++}`
    const node: GraphNode = { ...n, id }
    const arr = levels.get(n.level) ?? []
    arr.push(node)
    levels.set(n.level, arr)
    return id
  }

  const rootId = push({
    beadID: root.bead_id,
    anvil: root.anvil,
    title: root.title,
    status: root.status,
    priority: root.priority,
    level: 0,
    isRoot: true,
    outermost: false,
  })

  const walkDown = (parentId: string, kids: BeadBrief[] | undefined, level: number) => {
    if (!kids || level > MAX_DEPTH) return
    for (const k of kids) {
      const childId = push({
        beadID: k.bead_id,
        anvil: k.anvil,
        title: k.title,
        status: k.status,
        priority: k.priority,
        level,
        isRoot: false,
        outermost: level === MAX_DEPTH,
      })
      edges.push({ fromKey: parentId, toKey: childId })
      walkDown(childId, k.blocks, level + 1)
    }
  }

  const walkUp = (parentId: string, kids: BeadBrief[] | undefined, level: number) => {
    if (!kids || level < -MAX_DEPTH) return
    for (const k of kids) {
      const childId = push({
        beadID: k.bead_id,
        anvil: k.anvil,
        title: k.title,
        status: k.status,
        priority: k.priority,
        level,
        isRoot: false,
        outermost: level === -MAX_DEPTH,
      })
      edges.push({ fromKey: parentId, toKey: childId })
      walkUp(childId, k.blocked_by, level - 1)
    }
  }

  walkDown(rootId, deps?.blocks, 1)
  walkUp(rootId, deps?.blocked_by, -1)

  return { levels, edges }
}

// DepsGraphView renders a layered tree of a bead's dependency graph, three
// hops in each direction. The root sits at level 0; blocked_by (upstream)
// dependencies stack above, blocks (downstream) dependents stack below. Edges
// are drawn as straight lines on an absolute-positioned <svg> overlay that
// re-measures after layout and on resize. Clicking any non-root node re-roots
// the graph there so the user can pan further than three hops one ring at a
// time, with a "Reset" button returning to the original focus.
export default function DepsGraphView({ open, root, onClose }: DepsGraphViewProps) {
  const [currentRoot, setCurrentRoot] = useState<BeadBrief | null>(root)
  const [data, setData] = useState<BeadDepsResponse | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const { logout } = useAuth()

  const modalRef = useRef<HTMLDivElement | null>(null)
  const closeRef = useRef<HTMLButtonElement | null>(null)
  const canvasRef = useRef<HTMLDivElement | null>(null)
  const nodeRefs = useRef<Map<string, HTMLDivElement | null>>(new Map())
  const [edgeCoords, setEdgeCoords] = useState<EdgeCoord[]>([])
  const [svgSize, setSvgSize] = useState<{ width: number; height: number }>({ width: 0, height: 0 })

  useEffect(() => {
    if (open) {
      setCurrentRoot(root)
      setData(null)
      setError(null)
    } else {
      setCurrentRoot(null)
      setData(null)
      setError(null)
    }
    // Depend on bead identity (bead_id/anvil) only — not on the full root
    // object — so polling-driven metadata updates (title/status/priority) do
    // not reset graph state or override a user's re-root mid-session.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, root?.bead_id, root?.anvil])

  useEffect(() => {
    if (!open || !currentRoot) return
    const controller = new AbortController()
    setLoading(true)
    setError(null)
    fetchBeadDeps(currentRoot.bead_id, MAX_DEPTH, controller.signal)
      .then((d) => {
        setData(d)
        setLoading(false)
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return
        if (err instanceof ApiError && err.status === 401) {
          void logout()
          return
        }
        const msg =
          err instanceof ApiError ? err.message : (err as Error)?.message || 'failed to load'
        setError(msg)
        setLoading(false)
      })
    return () => controller.abort()
  }, [open, currentRoot, logout])

  useEffect(() => {
    if (!open) return
    const t = window.setTimeout(() => {
      closeRef.current?.focus()
    }, 10)
    return () => window.clearTimeout(t)
  }, [open])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose()
        return
      }
      if (e.key === 'Tab' && modalRef.current) {
        const focusable = modalRef.current.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
        )
        const elements = Array.from(focusable)
        if (elements.length === 0) return
        const first = elements[0]
        const last = elements[elements.length - 1]
        if (e.shiftKey) {
          if (document.activeElement === first) {
            e.preventDefault()
            last.focus()
          }
        } else {
          if (document.activeElement === last) {
            e.preventDefault()
            first.focus()
          }
        }
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  const graph = useMemo(() => {
    if (!currentRoot) return { levels: new Map<number, GraphNode[]>(), edges: [] as GraphEdge[] }
    return buildGraph(currentRoot, data)
  }, [currentRoot, data])

  const sortedLevels = useMemo(() => {
    return Array.from(graph.levels.keys()).sort((a, b) => a - b)
  }, [graph.levels])

  // recomputeEdges projects each rendered node's bounding rect into the
  // canvas-relative coordinate space, then derives one straight line per edge
  // from the bottom-center of the upper node to the top-center of the lower
  // one. Edge keys are stable so React can reconcile without remounting <line>
  // elements during normal redraws.
  const recomputeEdges = useCallback(() => {
    const canvas = canvasRef.current
    if (!canvas) return
    const cRect = canvas.getBoundingClientRect()
    const coords: EdgeCoord[] = []
    for (const e of graph.edges) {
      const fromEl = nodeRefs.current.get(e.fromKey)
      const toEl = nodeRefs.current.get(e.toKey)
      if (!fromEl || !toEl) continue
      const fr = fromEl.getBoundingClientRect()
      const tr = toEl.getBoundingClientRect()
      const fromIsAbove = fr.top < tr.top
      const upper = fromIsAbove ? fr : tr
      const lower = fromIsAbove ? tr : fr
      const x1 = upper.left + upper.width / 2 - cRect.left + canvas.scrollLeft
      const y1 = upper.bottom - cRect.top + canvas.scrollTop
      const x2 = lower.left + lower.width / 2 - cRect.left + canvas.scrollLeft
      const y2 = lower.top - cRect.top + canvas.scrollTop
      coords.push({ key: `${e.fromKey}->${e.toKey}`, x1, y1, x2, y2 })
    }
    setEdgeCoords(coords)
    setSvgSize({ width: canvas.scrollWidth, height: canvas.scrollHeight })
  }, [graph.edges])

  useLayoutEffect(() => {
    recomputeEdges()
  }, [recomputeEdges, graph.levels])

  useEffect(() => {
    if (!open) return
    const canvas = canvasRef.current
    if (!canvas) return
    const ro = new ResizeObserver(() => recomputeEdges())
    ro.observe(canvas)
    for (const el of nodeRefs.current.values()) {
      if (el) ro.observe(el)
    }
    return () => ro.disconnect()
  }, [open, recomputeEdges, graph.levels])

  const reRoot = useCallback((n: GraphNode) => {
    setCurrentRoot({
      bead_id: n.beadID,
      anvil: n.anvil,
      title: n.title,
      status: n.status,
      priority: n.priority,
    })
    setData(null)
    setError(null)
    nodeRefs.current = new Map()
  }, [])

  const resetRoot = useCallback(() => {
    if (!root) return
    setCurrentRoot(root)
    setData(null)
    setError(null)
    nodeRefs.current = new Map()
  }, [root])

  if (!open || !root || !currentRoot) return null

  const isReRooted =
    currentRoot.bead_id !== root.bead_id ||
    (currentRoot.anvil ?? '') !== (root.anvil ?? '')

  const totalNodes = Array.from(graph.levels.values()).reduce((acc, arr) => acc + arr.length, 0)

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="deps-graph-title"
      className="fixed inset-0 z-50 flex items-stretch justify-center bg-slate-950/80 p-0 sm:p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
      ref={modalRef}
    >
      <div className="flex h-full w-full flex-col rounded-none border-0 bg-slate-900 shadow-xl sm:h-[90vh] sm:max-h-[90vh] sm:w-[min(95vw,1400px)] sm:rounded-xl sm:border sm:border-slate-800">
        <header className="flex items-start gap-3 border-b border-slate-800 px-4 py-3 sm:px-5 sm:py-4">
          <div className="min-w-0 flex-1">
            <h2 id="deps-graph-title" className="truncate text-base font-semibold text-slate-100">
              Dependency graph
            </h2>
            <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
              <span>Centered on</span>
              <span className="font-mono text-slate-300">{currentRoot.bead_id}</span>
              {currentRoot.title && (
                <>
                  <span aria-hidden>·</span>
                  <span className="truncate text-slate-400">{currentRoot.title}</span>
                </>
              )}
              <span aria-hidden>·</span>
              <span>
                {totalNodes} node{totalNodes === 1 ? '' : 's'} · up to {MAX_DEPTH} hops
              </span>
            </p>
          </div>
          {isReRooted && (
            <button
              type="button"
              onClick={resetRoot}
              className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-slate-700 bg-slate-800 px-2.5 py-1 text-xs text-slate-200 hover:border-amber-400/40 hover:text-amber-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300"
            >
              <RotateCcw size={12} aria-hidden /> Reset
            </button>
          )}
          <button
            ref={closeRef}
            type="button"
            onClick={onClose}
            aria-label="Close dependency graph"
            className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-slate-700 bg-slate-800 px-2.5 py-1 text-xs text-slate-200 hover:bg-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
          >
            <X size={12} aria-hidden /> Close
          </button>
        </header>

        <div
          ref={canvasRef}
          className="relative flex-1 overflow-auto bg-slate-950/40 px-4 py-6 sm:px-8 sm:py-8"
        >
          {loading && !data && (
            <div className="flex items-center gap-2 text-xs text-slate-500">
              <Loader2 size={14} className="animate-spin" aria-label="Loading" />
              Loading graph…
            </div>
          )}
          {error && (
            <p className="rounded-md border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-200">
              {error}
            </p>
          )}
          {!loading && !error && totalNodes === 0 && (
            <p className="text-sm text-slate-500">No dependencies to graph.</p>
          )}

          {totalNodes > 0 && (
            <>
              <svg
                width={svgSize.width || '100%'}
                height={svgSize.height || '100%'}
                className="pointer-events-none absolute inset-0"
                aria-hidden
              >
                {edgeCoords.map((c) => (
                  <line
                    key={c.key}
                    x1={c.x1}
                    y1={c.y1}
                    x2={c.x2}
                    y2={c.y2}
                    stroke="rgb(71, 85, 105)"
                    strokeWidth={1.5}
                  />
                ))}
              </svg>
              <div className="relative flex flex-col gap-10">
                {sortedLevels.map((lvl) => {
                  const nodes = graph.levels.get(lvl) ?? []
                  return (
                    <div key={lvl} className="flex flex-col items-center gap-2">
                      <LevelLabel level={lvl} />
                      <div className="flex flex-wrap items-stretch justify-center gap-4">
                        {nodes.map((n) => (
                          <BeadNodeCard
                            key={n.id}
                            node={n}
                            onReRoot={reRoot}
                            registerRef={(el) => {
                              if (el) {
                                nodeRefs.current.set(n.id, el)
                              } else {
                                nodeRefs.current.delete(n.id)
                              }
                            }}
                          />
                        ))}
                      </div>
                    </div>
                  )
                })}
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  )
}

interface LevelLabelProps {
  level: number
}

function LevelLabel({ level }: LevelLabelProps) {
  if (level === 0) {
    return <span className="text-[10px] font-semibold uppercase tracking-wide text-amber-400">Focus</span>
  }
  const direction = level < 0 ? 'Blocked by' : 'Blocks'
  const abs = Math.abs(level)
  return (
    <span className="text-[10px] font-semibold uppercase tracking-wide text-slate-500">
      {direction} · hop {abs}
    </span>
  )
}

interface BeadNodeCardProps {
  node: GraphNode
  onReRoot: (n: GraphNode) => void
  registerRef: (el: HTMLDivElement | null) => void
}

// BeadNodeCard mirrors the dep-list row styling used by the Dependencies
// panel and BeadDepModal so the graph and list views share visual primitives.
// The root node is rendered with a distinct amber border to anchor the user.
// Outer-ring leaves expose an Expand affordance that re-roots the graph at
// that bead, letting users pan further than the API's three-hop limit.
function BeadNodeCard({ node, onReRoot, registerRef }: BeadNodeCardProps) {
  const ringClass = node.isRoot
    ? 'border-amber-400/60 bg-slate-900 shadow-[0_0_0_1px_rgba(251,191,36,0.25)]'
    : 'border-slate-800 bg-slate-950/60 hover:border-amber-400/40 hover:bg-slate-900'
  const fullPagePath = node.anvil
    ? `/bead/${encodeURIComponent(node.beadID)}?anvil=${encodeURIComponent(node.anvil)}`
    : `/bead/${encodeURIComponent(node.beadID)}`
  return (
    <div
      ref={registerRef}
      className={`relative w-56 max-w-[16rem] rounded-md border px-2.5 py-2 text-left text-xs transition-colors ${ringClass}`}
    >
      <div className="flex flex-wrap items-baseline gap-1.5">
        <span className="font-mono text-slate-300">{node.beadID}</span>
        <span
          className={`shrink-0 rounded-md border px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wide ${priorityClasses(node.priority)}`}
        >
          {priorityLabel(node.priority)}
        </span>
        {node.status && (
          <span
            className={`shrink-0 rounded-md border px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wide ${badgeClass(node.status)}`}
          >
            {node.status}
          </span>
        )}
      </div>
      <p className="mt-1 line-clamp-2 break-words text-slate-200">{node.title || node.beadID}</p>
      <div className="mt-2 flex flex-wrap items-center gap-1.5">
        {!node.isRoot && (
          <button
            type="button"
            onClick={() => onReRoot(node)}
            className="inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-800 px-1.5 py-0.5 text-[10px] text-slate-200 hover:border-amber-400/40 hover:text-amber-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300"
          >
            <Maximize2 size={10} aria-hidden />
            {node.outermost ? 'Expand' : 'Focus'}
          </button>
        )}
        <Link
          to={fullPagePath}
          className="inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-800 px-1.5 py-0.5 text-[10px] text-slate-200 hover:border-amber-400/40 hover:text-amber-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300"
        >
          <ExternalLink size={10} aria-hidden /> Open
        </Link>
      </div>
    </div>
  )
}
