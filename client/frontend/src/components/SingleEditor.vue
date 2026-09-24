<script lang="ts" setup>
import { defaultKeymap, history, redo, undo } from '@codemirror/commands'
import {
  Annotation,
  EditorSelection,
  EditorState,
  StateEffect,
  StateField,
  Transaction,
  type Extension,
  type Text,
  type TransactionSpec,
  type StateCommand,
} from '@codemirror/state'
import {
  Decoration,
  EditorView,
  gutter,
  GutterMarker,
  keymap,
  type DecorationSet,
  type ViewUpdate,
  WidgetType,
} from '@codemirror/view'
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import ModalDialog from './ModalDialog.vue'
import {
  DeleteLine,
  MergeUp,
  MoveCaretRange,
  RejectClaim,
  RequestFollow,
  SubmitEdit,
  SubmitEditClaim,
  SubmitInsert,
  SubmitInsertBefore,
  SubmitPaste,
  SubmitSpanEdit,
  Suspend,
} from '../../wailsjs/go/main/App'
import {
  buildSpanReplacement,
  caretAfterSpanInsert,
  caretPosInRows,
  changedRange,
  cursorRowIndex,
  enterAnchorsBefore,
  insertEdgeCaretTarget,
  isValidFormalSpan,
  keyOffsetFromPos,
  posFromKeyOffset,
  rowsToDoc,
  spanSelectionPrefix,
  touchedLineIndexes,
  textChange,
  unitKey,
  unitLineIndices,
} from '../projection'
import {
  ACTION_DELETE,
  ACTION_EDIT,
  ACTION_INSERT,
  ACTION_INSERT_BEFORE,
  Cursor,
  VisualRow,
  colorFor,
  isInsertAction,
} from '../types'

const props = defineProps<{
  rows: VisualRow[]
  me: string
  cursors: Cursor[]
  initialLineId?: string
}>()

const emit = defineEmits<{
  (e: 'error', err: unknown): void
}>()

const host = ref<HTMLElement | null>(null)
const followTarget = ref('')
const following = ref(false)

const syncAnn = Annotation.define<boolean>()
const CROSS_MSG = '无法跨争议区或他人候选做整段修改'

const setRowsEffect = StateEffect.define<VisualRow[]>()
const setCursorsEffect = StateEffect.define<Cursor[]>()

const rowsField = StateField.define<VisualRow[]>({
  create: () => [],
  update(value, tr) {
    for (const e of tr.effects) if (e.is(setRowsEffect)) return e.value
    return value
  },
})

const cursorsField = StateField.define<Cursor[]>({
  create: () => [],
  update(value, tr) {
    for (const e of tr.effects) if (e.is(setCursorsEffect)) return e.value
    return value
  },
})

let view: EditorView | null = null
let rowsRef: VisualRow[] = []
/** 最近一次 props/服务端 rows；乐观跨度失败时回滚到这里 */
let serverRows: VisualRow[] = []
let cursorsRef: Cursor[] = []
let meRef = ''
let composing = false
let deferredRows: VisualRow[] | null = null
let focusKey = ''
let suspendTimer: number | null = null
let suspendedLocal = false
let lastCaretKey = ''
let lastCaretOff = 0
let lastCaretEnd = 0
let lastAnchorKey = ''
let suppressSubmit = false
let structuralCaret: { row: VisualRow; part: number; offset: number; insert: boolean; ready: boolean; minRows: number; text: string } | null = null
const pendingSubmissions = new Set<Promise<void>>()

function trackSubmit(promise: Promise<void>): Promise<void> {
  pendingSubmissions.add(promise)
  const settled = () => {
    pendingSubmissions.delete(promise)
    if (!pendingSubmissions.size) flushDeferredRows()
  }
  void promise.then(settled, settled)
  return promise
}

function flushDeferredRows() {
  requestAnimationFrame(() => {
    if (!view || composing || pendingSubmissions.size || !deferredRows) return
    const next = deferredRows
    deferredRows = null
    applyRows(next, true)
  })
}
/** 串行 SubmitSpanEdit，连打不丢、不乱序 */
let spanTail: Promise<void> = Promise.resolve()

function showError(e: unknown) {
  emit('error', e)
}

function enqueueSpanEdit(
  startLineID: string,
  endLineID: string,
  replacement: string[],
): Promise<void> {
  const run = spanTail.then(() => SubmitSpanEdit(startLineID, endLineID, replacement))
  spanTail = run.catch(() => {})
  return trackSubmit(run)
}

function buildOptimisticSpanRows(
  rows: VisualRow[],
  idxs: number[],
  replacement: string[],
): VisualRow[] {
  const head = rows[idxs[0]]
  // 已有跨度候选保留原 BaseIDs；正式行选区才用触及行 lineId
  const baseIDs =
    head.spanBaseIDs && head.spanBaseIDs.length >= 2
      ? head.spanBaseIDs
      : idxs.map((i) => rows[i].lineId)
  const parts = replacement.map((text, partIndex) => ({
    ...head,
    key:
      replacement.length <= 1
        ? `line:${baseIDs[0]}`
        : `span:${baseIDs[0]}:${partIndex}`,
    lineId: baseIDs[0],
    content: text,
    partIndex,
    partCount: replacement.length,
    disputeId: '',
    followId: '',
    action: ACTION_EDIT,
    spanBaseIDs: baseIDs,
    showLineNo: partIndex === 0 && head.showLineNo,
    blockStart: partIndex === 0,
    separatorBefore: false,
    editable: true,
    isSelf: true,
    isContext: false,
    phantom: false,
    gutterDots: [] as string[],
  }))
  return [
    ...rows.slice(0, idxs[0]),
    ...parts,
    ...rows.slice(idxs[idxs.length - 1] + 1),
  ]
}

/** 本地立刻折叠跨度行，再串行提交；CM doc 与 rowsRef 同步 */
function applySpanLocally(
  doc: Text,
  idxs: number[],
  fromA: number,
  toA: number,
  inserted: string,
): TransactionSpec {
  const replacement = buildSpanReplacement(doc, fromA, toA, inserted)
  const prefix = spanSelectionPrefix(doc, fromA, toA)
  const caret = caretAfterSpanInsert(prefix, inserted)
  const next = buildOptimisticSpanRows(rowsRef, idxs, replacement)
  const partIdx = Math.min(idxs[0] + caret.part, next.length - 1)
  rowsRef = next
  lastCaretKey = next[partIdx]?.key ?? next[idxs[0]].key
  lastCaretOff = caret.offset
  lastCaretEnd = caret.offset
  focusKey = lastCaretKey
  const base = next[idxs[0]].spanBaseIDs!
  if (!composing) enqueueSpanEdit(base[0], base[base.length - 1], replacement).catch((e) => {
    showError(e)
    revertDoc()
  })
  return {
    changes: textChange(doc.toString(), rowsToDoc(next)),
    selection: EditorSelection.cursor(caretPosInRows(next, partIdx, caret.offset)),
    annotations: syncAnn.of(true),
    effects: [setRowsEffect.of(next), setCursorsEffect.of(cursorsRef)],
  }
}

/** 已有跨度候选内改 parts：本地立刻换成完整整段，再 enqueue 一份 */
function commitSpanClaim(
  v: EditorView,
  idxs: number[],
  replacement: string[],
  caretPart: number,
  caretOff: number,
) {
  const next = buildOptimisticSpanRows(rowsRef, idxs, replacement)
  const partIdx = Math.min(idxs[0] + caretPart, next.length - 1)
  rowsRef = next
  lastCaretKey = next[partIdx]?.key ?? next[idxs[0]].key
  lastCaretOff = caretOff
  lastCaretEnd = caretOff
  focusKey = lastCaretKey
  const base = next[idxs[0]].spanBaseIDs!
  enqueueSpanEdit(base[0], base[base.length - 1], replacement).catch((e) => {
    showError(e)
    revertDoc()
  })
  suppressSubmit = true
  v.dispatch({
    changes: textChange(v.state.doc.toString(), rowsToDoc(next)),
    selection: EditorSelection.cursor(caretPosInRows(next, partIdx, caretOff)),
    annotations: syncAnn.of(true),
    effects: [setRowsEffect.of(next), setCursorsEffect.of(cursorsRef)],
  })
  suppressSubmit = false
}

function spanClaimIdxs(rowIndex: number): number[] {
  return unitLineIndices(rowsRef, rowIndex).filter((i) => !rowsRef[i]?.isContext)
}

/** 提交失败时拉回服务端/props 正文 */
function revertDoc() {
  if (!view) return
  const want = rowsToDoc(serverRows)
  rowsRef = serverRows
  if (view.state.doc.toString() === want) {
    view.dispatch({ effects: [setRowsEffect.of(serverRows)] })
    return
  }
  suppressSubmit = true
  view.dispatch({
    changes: { from: 0, to: view.state.doc.length, insert: want },
    annotations: [syncAnn.of(true), Transaction.addToHistory.of(false)],
    effects: [setRowsEffect.of(serverRows), setCursorsEffect.of(cursorsRef)],
  })
  suppressSubmit = false
}

function clearSuspendTimer() {
  if (suspendTimer != null) {
    window.clearTimeout(suspendTimer)
    suspendTimer = null
  }
}

function armSuspend(row: VisualRow) {
  clearSuspendTimer()
  if (!row.editable || !row.isSelf) return
  suspendTimer = window.setTimeout(() => {
    if (focusKey !== row.key) return
    suspendedLocal = true
    Suspend(row.lineId, attentionAction(row), true).catch(showError)
  }, 60_000)
}

async function resumeSuspend(row: VisualRow) {
  if (suspendedLocal && row.isSelf) {
    suspendedLocal = false
    try {
      await Suspend(row.lineId, attentionAction(row), false)
    } catch (e) {
      showError(e)
    }
  }
  armSuspend(row)
}

async function blurSuspend(row: VisualRow) {
  clearSuspendTimer()
  if (row.isSelf && !suspendedLocal) {
    suspendedLocal = true
    try {
      await Suspend(row.lineId, attentionAction(row), true)
    } catch (e) {
      showError(e)
    }
  }
}

function attentionAction(row: VisualRow): string {
  return row.contextAction || row.action
}

function submitClaim(row: VisualRow, lines: string[]): Promise<void> {
  const span = row.spanBaseIDs
  if (span && span.length >= 2) {
    return enqueueSpanEdit(span[0], span[span.length - 1], lines)
  }
  // 上下文行只改正式锚点，绝不进 SubmitInsert
  if (row.isContext) {
    return trackSubmit(lines.length > 1 ? SubmitPaste(row.lineId, lines) : SubmitEdit(row.lineId, lines[0] ?? ''))
  }
  if (row.action === ACTION_INSERT) {
    return trackSubmit(SubmitInsert(row.lineId, lines))
  }
  if (row.action === ACTION_INSERT_BEFORE) return trackSubmit(SubmitInsertBefore(row.lineId, lines))
  if (row.disputeId || row.partCount > 1) return trackSubmit(SubmitEditClaim(row.lineId, lines))
  return trackSubmit(lines.length > 1 ? SubmitPaste(row.lineId, lines) : SubmitEdit(row.lineId, lines[0] ?? ''))
}

/** 收集主张 parts；优先读当前 doc，避免先前击键后 rowsRef 滞后 */
function payloadLines(rows: VisualRow[], rowIndex: number, partText: string): string[] {
  const row = rows[rowIndex]
  if (!row || row.isContext) return [partText]
  if (row.partCount <= 1) return [partText]
  const key = unitKey(row)
  const parts: string[] = new Array(row.partCount).fill('')
  const doc = view?.state.doc
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i]
    if (r.isContext || unitKey(r) !== key) continue
    if (r.partIndex < 0 || r.partIndex >= parts.length) continue
    const fromDoc = doc && i < doc.lines ? doc.line(i + 1).text : r.content ?? ''
    parts[r.partIndex] = i === rowIndex ? partText : fromDoc
  }
  parts[row.partIndex] = partText
  return parts
}

function saveCaret(v: EditorView) {
  const sel = v.state.selection.main
  const a = keyOffsetFromPos(v.state.doc, rowsRef, sel.head)
  const b = keyOffsetFromPos(v.state.doc, rowsRef, sel.anchor)
  if (!a) return
  lastCaretKey = a.key
  lastCaretOff = a.offset
  lastAnchorKey = b?.key || a.key
  lastCaretEnd = b?.offset ?? a.offset
}

function emitCaretFromView(v: EditorView) {
  if (suppressSubmit || composing || !v.hasFocus) return
  const sel = v.state.selection.main
  const info = keyOffsetFromPos(v.state.doc, rowsRef, sel.head)
  if (!info) return
  const row = rowsRef.find((r) => r.key === info.key)
  if (!row) return
  const endInfo = keyOffsetFromPos(v.state.doc, rowsRef, sel.anchor)
  const offset = info.offset
  const endRow = rowsRef.find((r) => r.key === endInfo?.key) || row
  const selEnd = endInfo?.offset ?? offset
  const previous = rowsRef.find((r) => r.key === focusKey)
  if (previous && (previous.lineId !== row.lineId || attentionAction(previous) !== attentionAction(row))) {
    void blurSuspend(previous)
  }
  lastCaretKey = info.key
  lastCaretOff = offset
  lastCaretEnd = selEnd
  lastAnchorKey = endInfo?.key || info.key
  focusKey = info.key
  // 上下文：本人=正式行；他人假 disputeId 不上传，避免远端画错行
  const disputeId = row.isContext ? '' : row.disputeId || ''
  if (row.isContext && !row.isSelf) return
  MoveCaretRange(row.lineId, disputeId, row.partIndex, offset,
    endRow.lineId, endRow.isContext ? '' : endRow.disputeId || '', endRow.partIndex, selEnd).catch(() => {})
  void resumeSuspend(row)
}

function applyRows(next: VisualRow[], force = false) {
  if (!view) return
  if (!force && (composing || pendingSubmissions.size > 0)) {
    deferredRows = next
    return
  }
  deferredRows = null
  saveCaret(view)
  const headRow = rowsRef.find((r) => r.key === lastCaretKey)
  const anchorRow = rowsRef.find((r) => r.key === (lastAnchorKey || lastCaretKey))
  const doc = rowsToDoc(next)
  const same = doc === view.state.doc.toString()
  serverRows = next
  rowsRef = next
  if (same) {
    view.dispatch({
      effects: [setRowsEffect.of(next), setCursorsEffect.of(cursorsRef)],
    })
    placeStructuralCaret()
    return
  }
  suppressSubmit = true
  view.dispatch({
    changes: textChange(view.state.doc.toString(), doc),
    annotations: [syncAnn.of(true), Transaction.addToHistory.of(false)],
    effects: [setRowsEffect.of(next), setCursorsEffect.of(cursorsRef)],
  })
  const from = posFromKeyOffset(view.state.doc, next, lastCaretKey, lastCaretOff, headRow)
  const to = posFromKeyOffset(view.state.doc, next, lastAnchorKey || lastCaretKey, lastCaretEnd, anchorRow)
  view.dispatch({
    selection: EditorSelection.range(to, from),
    annotations: syncAnn.of(true),
  })
  suppressSubmit = false
  placeStructuralCaret()
  if (structuralCaret?.ready) structuralCaret = null
}

class DotMarker extends GutterMarker {
  constructor(
    readonly lineNo: number | null,
    readonly dots: string[],
  ) {
    super()
  }
  eq(other: DotMarker) {
    return (
      this.lineNo === other.lineNo &&
      this.dots.length === other.dots.length &&
      this.dots.every((c, i) => c === other.dots[i])
    )
  }
  toDOM() {
    const wrap = document.createElement('div')
    wrap.className = 'cm-row-gutter'
    const n = document.createElement('span')
    n.className = 'cm-lineno'
    n.textContent = this.lineNo != null ? String(this.lineNo) : ''
    wrap.appendChild(n)
    const dots = document.createElement('span')
    dots.className = 'cm-dots'
    if (this.dots.length) {
      dots.setAttribute('role', 'img')
      dots.setAttribute('aria-label', `${this.dots.length} 位主张或追随者`)
    }
    for (const c of this.dots) {
      const i = document.createElement('i')
      i.style.background = c
      dots.appendChild(i)
    }
    wrap.appendChild(dots)
    return wrap
  }
}

const rowGutter = gutter({
  class: 'cm-row-gutter-col',
  lineMarker(view, line) {
    const rows = view.state.field(rowsField)
    const idx = view.state.doc.lineAt(line.from).number - 1
    const r = rows[idx]
    if (!r) return null
    const colors = (r.gutterDots || []).map((id) => colorFor(id))
    return new DotMarker(r.showLineNo ? r.lineNo : null, colors)
  },
  initialSpacer: () => new DotMarker(99, ['#000']),
})

function lineClass(row: VisualRow): string {
  const parts = ['cm-vis-row', row.zebra === 0 ? 'cm-zebra0' : 'cm-zebra1']
  if (row.blockStart) parts.push('cm-block-start')
  if (row.separatorBefore) parts.push('cm-sep-before')
  if (row.isContext) parts.push('cm-context')
  if (row.disputeId || row.phantom || row.isContext) parts.push('cm-dispute')
  if (row.isSelf) parts.push('cm-self')
  else parts.push('cm-other')
  if (row.suspended) parts.push('cm-faded')
  if (isInsertAction(row.action)) parts.push('cm-insert')
  if (row.action === ACTION_DELETE) parts.push('cm-delete')
  return parts.join(' ')
}

const lineDecoField = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(_deco, tr) {
    if (!tr.docChanged && !tr.effects.some((e) => e.is(setRowsEffect))) return _deco
    const rows = tr.state.field(rowsField)
    const builder: { from: number; to: number; value: Decoration }[] = []
    for (let i = 0; i < rows.length && i < tr.state.doc.lines; i++) {
      const line = tr.state.doc.line(i + 1)
      builder.push({
        from: line.from,
        to: line.from,
        value: Decoration.line({ class: lineClass(rows[i]) }),
      })
    }
    return Decoration.set(builder)
  },
  provide: (f) => EditorView.decorations.from(f),
})

class CaretWidget extends WidgetType {
  constructor(
    readonly color: string,
    readonly title: string,
  ) {
    super()
  }
  eq(other: CaretWidget) {
    return this.color === other.color && this.title === other.title
  }
  toDOM() {
    const s = document.createElement('span')
    s.className = 'cm-remote-caret'
    s.style.background = this.color
    s.title = this.title
    return s
  }
  ignoreEvent() {
    return true
  }
}

const remoteCursorField = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(_, tr) {
    if (
      !tr.docChanged &&
      !tr.effects.some((e) => e.is(setRowsEffect) || e.is(setCursorsEffect))
    ) {
      return _
    }
    const rows = tr.state.field(rowsField)
    const cursors = tr.state.field(cursorsField)
    const marks: { from: number; to: number; value: Decoration }[] = []
    for (const c of cursors) {
      if (c.personId === meRef) continue
      const idx = cursorRowIndex(rows, c.lineId, c.disputeId, c.partIndex, c.personId)
      if (idx < 0 || idx >= tr.state.doc.lines) continue
      const line = tr.state.doc.line(idx + 1)
      const a = line.from + Math.max(0, Math.min(c.offset, line.length))
      const endIdx = cursorRowIndex(rows, c.selEndLineId || c.lineId,
        c.selEndLineId ? c.selEndDisputeId || '' : c.disputeId,
        c.selEndLineId ? c.selEndPartIndex ?? 0 : c.partIndex, c.personId)
      const endLine = endIdx >= 0 && endIdx < tr.state.doc.lines ? tr.state.doc.line(endIdx + 1) : line
      const b = endLine.from + Math.max(0, Math.min(c.selEnd ?? c.offset, endLine.length))
      const from = Math.min(a, b)
      const to = Math.max(a, b)
      const col = colorFor(c.personId)
      if (from !== to) {
        for (let i = Math.min(idx, endIdx < 0 ? idx : endIdx); i <= Math.max(idx, endIdx); i++) {
          const row = rows[i]
          if (row.disputeId && row.disputeId !== c.disputeId && row.disputeId !== c.selEndDisputeId) continue
          if (row.isContext && i !== idx && i !== endIdx) continue
          const part = tr.state.doc.line(i + 1)
          const start = Math.max(from, part.from)
          const end = Math.min(to, part.to)
          if (start < end) marks.push({ from: start, to: end,
            value: Decoration.mark({ attributes: { style: `background:${col}55` } }) })
        }
      }
      marks.push({
        from: a,
        to: a,
        value: Decoration.widget({
          widget: new CaretWidget(col, c.name || ''),
          side: 1,
        }),
      })
    }
    marks.sort((x, y) => x.from - y.from || (x.to - y.to))
    return Decoration.set(marks, true)
  },
  provide: (f) => EditorView.decorations.from(f),
})

function changeAllowed(
  rows: VisualRow[],
  doc: Text,
  fromA: number,
  toA: number,
): 'ok' | 'protected' | 'cross' | 'span' {
  const idxs = touchedLineIndexes(doc, fromA, toA)
  for (const i of idxs) {
    const r = rows[i]
    if (!r || !r.editable) return 'protected'
  }
  const keys = new Set<string>()
  for (const i of idxs) {
    const r = rows[i]
    if (r) keys.add(unitKey(r))
  }
  if (keys.size <= 1) return 'ok'
  if (isValidFormalSpan(rows, idxs)) return 'span'
  return 'cross'
}

function afterUserEdit(v: EditorView): Promise<void> {
  if (suppressSubmit || composing) return Promise.resolve()
  const rows = rowsRef
  if (!rows.length) return Promise.resolve()
  const head = v.state.selection.main.head
  const line = v.state.doc.lineAt(head)
  const idx = line.number - 1
  const row = rows[idx]
  if (!row || !row.editable) return Promise.resolve()
  const idxs = unitLineIndices(rows, idx).filter((i) => !rows[i]?.isContext)
  let payload: string[]
  if (row.isContext) {
    payload = [line.text]
  } else if (row.partCount > 1 && idxs.length === row.partCount) {
    payload = idxs.map((i) => (i < v.state.doc.lines ? v.state.doc.line(i + 1).text : ''))
  } else if (row.partCount > 1) {
    payload = payloadLines(rows, idx, line.text)
  } else {
    payload = [line.text]
  }
  const submission = submitClaim(row, payload).catch((e) => {
    showError(e)
    revertDoc()
  })
  emitCaretFromView(v)
  armSuspend(row)
  return submission
}

function placeStructuralCaret() {
  if (!view || !structuralCaret?.ready || rowsRef.length < structuralCaret.minRows) return
  const { row, part, offset, insert } = structuralCaret
  let index: number
  // insert=true 仅「插在后面」；前插走 action 匹配，避免 before 误入 after 的 anchor+1。
  if (insert) {
    const candidate = rowsRef.findIndex((r) => r.lineId === row.lineId &&
      r.action === ACTION_INSERT && r.isSelf && !r.isContext)
    const anchor = rowsRef.findIndex((r) => r.lineId === row.lineId && r.isSelf)
    index = candidate >= 0 ? candidate + part : anchor >= 0 ? anchor + 1 + part : -1
  } else {
    const head = rowsRef.findIndex((r) => r.lineId === row.lineId && r.isSelf &&
      (row.isContext ? r.isContext && r.contextAction === row.contextAction : !r.isContext) && r.partIndex === 0 &&
      r.action === row.action)
    index = head >= 0 ? head + part : -1
    if (index < 0 && row.action === ACTION_INSERT_BEFORE) {
      const anchor = rowsRef.findIndex((r) => r.isSelf && r.lineId === row.lineId)
      if (anchor >= 0) index = Math.max(0, anchor - row.partCount) + part
    }
  }
  if (index < 0 || index >= rowsRef.length || index >= view.state.doc.lines) return
  if (rowsRef[index].content !== structuralCaret.text) return
  const pos = caretPosInRows(rowsRef, index, offset)
  suppressSubmit = true
  view.dispatch({ selection: EditorSelection.cursor(pos), scrollIntoView: true, annotations: syncAnn.of(true) })
  suppressSubmit = false
  saveCaret(view)
  structuralCaret = null
}

/** 保留原生事务和撤销信息，提交方式由同一过滤入口决定。 */
function replaceSelection(v: EditorView, inserted: string): boolean {
  const { from, to } = v.state.selection.main
  v.dispatch({
    changes: { from, to, insert: inserted },
    selection: EditorSelection.cursor(from + inserted.length),
    userEvent: 'input',
    scrollIntoView: true,
  })
  return true
}

/** 行首锚定下方原行，行尾锚定上方原行；已有候选继续保留全文。空行走 after。 */
function applyInsertChange(rowIndex: number, before: boolean): TransactionSpec {
  const row = rowsRef[rowIndex]
  const action = before ? ACTION_INSERT_BEFORE : ACTION_INSERT
  const existing = rowsRef.map((r, i) => ({ r, i })).filter(({ r }) => r.lineId === row.lineId && r.action === action && r.isSelf && !r.isContext)
  const texts = existing.map(({ r, i }) => view?.state.doc.line(i + 1).text ?? r.content)
  const content = before ? [...texts, ''] : ['', ...texts]
  const at = rowIndex + (before ? 0 : 1)
  const disputeId = existing[0]?.r.disputeId || ''
  const optimistic = !disputeId
  const newLineNo = before ? row.lineNo : row.lineNo + 1
  const inserted: VisualRow = {
    ...row,
    key: 'insert:' + action + ':' + row.lineId + ':' + texts.length,
    content: '',
    action,
    contextAction: undefined,
    disputeId,
    followId: '',
    isContext: false,
    partIndex: before ? texts.length : 0,
    partCount: content.length,
    showLineNo: optimistic,
    lineNo: newLineNo,
    zebra: (newLineNo - 1) % 2,
    suspended: false,
    gutterDots: [],
    blockStart: false,
    separatorBefore: false,
  }
  const previous = rowsRef.map((r) => r.lineId === row.lineId && r.action === action && r.isSelf && !r.isContext
    ? { ...r, partIndex: r.partIndex + (before ? 0 : 1), partCount: content.length }
    : r)
  let next = [...previous.slice(0, at), inserted, ...previous.slice(at)]
  if (optimistic) {
    next = next.map((r, i) => {
      if (i === at) return r
      if (before && i > at && r.lineNo >= row.lineNo) {
        return { ...r, lineNo: r.lineNo + 1, zebra: r.lineNo % 2 }
      }
      if (!before && i > at && r.lineNo > row.lineNo) {
        return { ...r, lineNo: r.lineNo + 1, zebra: r.lineNo % 2 }
      }
      return r
    })
  }
  rowsRef = next
  // 初次 before：结构光标留下方原正式行，对齐 CM 行首 Enter（from+1）；勿锚上方新空行。
  const edge = insertEdgeCaretTarget(before, row.content)
  const target = {
    row,
    part: edge.part,
    offset: 0,
    insert: edge.insert,
    ready: false,
    minRows: next.length,
    text: edge.text,
  }
  structuralCaret = target
  void trackSubmit(before ? SubmitInsertBefore(row.lineId, content) : SubmitInsert(row.lineId, content)).then(async () => {
    target.ready = true
    await nextTick()
    placeStructuralCaret()
  }).catch((e) => { structuralCaret = null; showError(e); revertDoc() })
  return { effects: setRowsEffect.of(next), annotations: syncAnn.of(true) }
}

function handleEnter(v: EditorView): boolean {
  if (composing || v.composing) return false
  return replaceSelection(v, '\n')
}

/** 候选内删换行、撤销和重做也作为一整份主张提交，不能把下方正文误收进来。 */
function applyUnitChange(doc: Text, from: number, to: number, inserted: string): TransactionSpec {
  const start = doc.lineAt(from)
  const end = doc.lineAt(to)
  const rowIndex = start.number - 1
  const row = rowsRef[rowIndex]
  const indices = row.isContext ? [rowIndex] : unitLineIndices(rowsRef, rowIndex).filter((i) => !rowsRef[i].isContext)
  const parts = indices.map((i) => doc.line(i + 1).text)
  const replacement = buildSpanReplacement(doc, from, to, inserted)
  parts.splice(row.partIndex, end.number - start.number + 1, ...replacement)
  const caret = caretAfterSpanInsert(start.text.slice(0, from - start.from), inserted)
  const first = indices[0]
  const head = rowsRef[first]
  // 本人乐观插入（尚无 disputeId）：各临时行立刻占行号/斑马，避免闪无号。
  const optimisticInsert = isInsertAction(head.action) && head.isSelf && !head.disputeId
  const baseNo = head.lineNo
  const grew = parts.length - indices.length
  let next = [...rowsRef.slice(0, first), ...parts.map((content, partIndex) => ({
    ...head, content, partIndex, partCount: parts.length,
    key: partIndex === 0 ? head.key : head.key + ':part:' + partIndex,
    showLineNo: optimisticInsert ? true : partIndex === 0 && head.showLineNo,
    lineNo: optimisticInsert ? baseNo + partIndex : head.lineNo,
    zebra: optimisticInsert ? (baseNo + partIndex - 1) % 2 : head.zebra,
    blockStart: partIndex === 0 && head.blockStart,
    separatorBefore: partIndex === 0 && head.separatorBefore,
  })), ...rowsRef.slice(indices[indices.length - 1] + 1)]
  if (grew > 0 && optimisticInsert) {
    const after = first + parts.length
    next = next.map((r, i) => {
      if (i < after || r.lineNo < baseNo + indices.length) return r
      return { ...r, lineNo: r.lineNo + grew, zebra: (r.lineNo + grew - 1) % 2 }
    })
  }
  rowsRef = next
  if (!composing) {
    const target = {
      row: { ...head, partCount: parts.length },
      part: row.partIndex + caret.part,
      offset: caret.offset,
      // 仅 after 走 insert 分支；before 用 action 匹配，避免锚到 anchor+1。
      insert: head.action === ACTION_INSERT,
      ready: false,
      minRows: next.length,
      text: parts[row.partIndex + caret.part],
    }
    structuralCaret = target
    void submitClaim(row, parts).then(async () => {
      target.ready = true
      await nextTick()
      placeStructuralCaret()
    }).catch((e) => { structuralCaret = null; showError(e); revertDoc() })
  }
  return {
    changes: textChange(doc.toString(), rowsToDoc(next)),
    selection: EditorSelection.cursor(caretPosInRows(next, first + row.partIndex + caret.part, caret.offset)),
    effects: setRowsEffect.of(next), annotations: syncAnn.of(true),
  }
}

function handleBackspace(v: EditorView): boolean {
  if (composing || v.composing) return false
  const sel = v.state.selection.main
  if (!sel.empty) return false
  const line = v.state.doc.lineAt(sel.head)
  if (sel.head !== line.from) return false
  const idx = line.number - 1
  const row = rowsRef[idx]
  if (!row?.editable) return true
  if (row.partIndex > 0 && !row.isContext) {
    const lines = payloadLines(rowsRef, idx, line.text)
    const joinAt = lines[row.partIndex - 1].length
    lines[row.partIndex - 1] += lines[row.partIndex]
    lines.splice(row.partIndex, 1)
    if (row.spanBaseIDs?.length) {
      commitSpanClaim(v, spanClaimIdxs(idx), lines, row.partIndex - 1, joinAt)
      return true
    }
    const target = { row, part: row.partIndex - 1, offset: joinAt, insert: false, ready: false,
      minRows: rowsRef.length - 1, text: lines[row.partIndex - 1] }
    structuralCaret = target
    void submitClaim(row, lines).then(async () => {
      target.ready = true
      await nextTick()
      placeStructuralCaret()
    }).catch((e) => { structuralCaret = null; showError(e); revertDoc() })
    return true
  }
  if (isInsertAction(row.action) || row.action === ACTION_DELETE) return true
  const previous = rowsRef[idx - 1]
  if (previous && previous.isSelf) {
    structuralCaret = { row: previous, part: previous.partIndex, offset: previous.content.length, insert: false,
      ready: false, minRows: rowsRef.length - 1, text: previous.content + line.text }
  }
  void trackSubmit(line.text.length === 0 ? DeleteLine(row.lineId) : MergeUp(row.lineId))
    .then(async () => { if (structuralCaret) structuralCaret.ready = true; await nextTick(); placeStructuralCaret() })
    .catch((e) => { structuralCaret = null; showError(e); revertDoc() })
  return true
}

async function prepareLeave() {
  if (composing) throw new Error('请先完成当前输入，再切换文档')
  await Promise.all([...pendingSubmissions])
  if (view) {
    const row = rowsRef.find((r) => r.key === focusKey)
    if (row) await blurSuspend(row)
  }
}

async function confirmFollow() {
  if (!followTarget.value || following.value) return
  following.value = true
  try {
    await prepareLeave()
    await trackSubmit(RequestFollow(followTarget.value))
    followTarget.value = ''
  } catch (e) { showError(e) }
  finally { following.value = false }
}

async function confirmReject() {
  if (!followTarget.value || following.value) return
  following.value = true
  try {
    await prepareLeave()
    await trackSubmit(RejectClaim(followTarget.value))
    followTarget.value = ''
  } catch (e) { showError(e) }
  finally { following.value = false }
}

defineExpose({ prepareLeave, focus: () => view?.focus() })

function runHistory(v: EditorView, command: StateCommand): boolean {
  return command({ state: v.state, dispatch: (tr) => {
    const change = changedRange(tr.changes, tr.newDoc)
    const verdict = changeAllowed(tr.startState.field(rowsField), tr.startState.doc, change.from, change.to)
    if (verdict === 'protected' || verdict === 'cross') { showError(CROSS_MSG); return }
    v.dispatch(tr)
  } })
}

function buildExtensions(): Extension {
  return [
    rowsField,
    cursorsField,
    lineDecoField,
    remoteCursorField,
    rowGutter,
    EditorView.contentAttributes.of({ 'aria-label': '文档内容', 'aria-multiline': 'true' }),
    EditorView.domEventHandlers({ beforeinput: (event, editor) => {
      if (event.inputType !== 'historyUndo' && event.inputType !== 'historyRedo') return false
      event.preventDefault()
      runHistory(editor, event.inputType === 'historyUndo' ? undo : redo)
      return true
    } }),
    history(),
    EditorView.lineWrapping,
    keymap.of([
      { key: 'Enter', run: handleEnter },
      { key: 'Backspace', run: handleBackspace },
    ]),
    keymap.of([
      { key: 'Mod-z', run: (v) => runHistory(v, undo), shift: (v) => runHistory(v, redo), preventDefault: true },
      { key: 'Mod-y', run: (v) => runHistory(v, redo), preventDefault: true },
    ]),
    keymap.of(defaultKeymap.filter((b) => b.key !== 'Enter')),
    EditorView.domEventObservers({ compositionstart: () => { composing = true } }),
    // CodeMirror 原生历史使用 filter:false；extender 仍会执行，且不丢历史注解。
    EditorState.transactionExtender.of((tr) => {
      if (!tr.docChanged || (!tr.isUserEvent('undo') && !tr.isUserEvent('redo'))) return null
      const { from, to, insert } = changedRange(tr.changes, tr.newDoc)
      const verdict = changeAllowed(tr.startState.field(rowsField), tr.startState.doc, from, to)
      if (verdict === 'protected' || verdict === 'cross') return null
      const spec = verdict === 'span' ? applySpanLocally(tr.startState.doc,
        touchedLineIndexes(tr.startState.doc, from, to), from, to, insert) :
        applyUnitChange(tr.startState.doc, from, to, insert)
      return { effects: spec.effects, annotations: syncAnn.of(true) }
    }),
    // 原生撤销可能含多个相邻变更，按实际改动的首尾统一计算一次主张。
    EditorState.transactionFilter.of((tr) => {
      if (!tr.docChanged || tr.annotation(syncAnn)) return tr
      const { from, to, insert } = changedRange(tr.changes, tr.newDoc)
      const verdict = changeAllowed(rowsRef, tr.startState.doc, from, to)
      if (verdict === 'protected' || verdict === 'cross') {
        queueMicrotask(() => showError(CROSS_MSG))
        return []
      }
      let spec: TransactionSpec
      if (verdict === 'span') {
        spec = applySpanLocally(tr.startState.doc,
          touchedLineIndexes(tr.startState.doc, from, to), from, to, insert)
      } else if (tr.startState.doc.sliceString(from, to).includes('\n') || insert.includes('\n')) {
        const line = tr.startState.doc.lineAt(from)
        const row = rowsRef[line.number - 1]
        const insertAtEdge = insert === '\n' && from === to && (from === line.to || from === line.from) &&
          row.action === ACTION_EDIT && row.partCount === 1 && !row.spanBaseIDs?.length &&
          !tr.isUserEvent('undo') && !tr.isUserEvent('redo')
        const before = enterAnchorsBefore(line.from === line.to, from === line.from)
        spec = insertAtEdge ? applyInsertChange(line.number - 1, before) :
          applyUnitChange(tr.startState.doc, from, to, insert)
      } else {
        return tr
      }
      // 保留原事务中的历史注解；只补充投影，避免撤销变成一次全新的输入。
      return [tr, { effects: spec.effects, annotations: syncAnn.of(true) }]
    }),
    EditorView.updateListener.of((vu: ViewUpdate) => {
      if (vu.selectionSet) emitCaretFromView(vu.view)
      if (vu.docChanged && !vu.transactions.some((t) => t.annotation(syncAnn))) {
        void afterUserEdit(vu.view)
      }
    }),
    EditorView.domEventHandlers({
      compositionstart: () => {
        composing = true
        return false
      },
      compositionend: (_e, v) => {
        // 先提交上屏文字，再采用最新快照；旧快照不能覆盖输入法刚提交的内容。
        queueMicrotask(async () => {
          composing = false
          await afterUserEdit(v)
          flushDeferredRows()
        })
        return false
      },
      focus: (_e, v) => {
        const info = keyOffsetFromPos(v.state.doc, rowsRef, v.state.selection.main.head)
        if (info) {
          const row = rowsRef.find((r) => r.key === info.key)
          if (row) {
            focusKey = row.key
            void resumeSuspend(row)
          }
        }
        return false
      },
      blur: () => {
        const row = rowsRef.find((r) => r.key === focusKey)
        focusKey = ''
        if (row) void blurSuspend(row)
        return false
      },
      paste: (e, v) => {
        const text = e.clipboardData?.getData('text/plain')
        if (!text) return false
        const normalized = text.replace(/\r\n/g, '\n').replace(/\r/g, '\n')
        if (!normalized.includes('\n') && v.state.doc.lineAt(v.state.selection.main.from).number ===
          v.state.doc.lineAt(v.state.selection.main.to).number) return false
        e.preventDefault()
        return replaceSelection(v, normalized)
      },
      click: (e, v) => {
        const pos = v.posAtCoords({ x: e.clientX, y: e.clientY })
        if (pos == null) return false
        const line = v.state.doc.lineAt(pos)
        const row = rowsRef[line.number - 1]
        const follow = row?.followId || ''
        if (!row || row.isSelf || row.phantom || !follow) return false
        followTarget.value = follow
        return true
      },
    }),
    EditorView.theme({
      '&': { height: '100%', fontSize: 'inherit', background: 'transparent' },
      '.cm-scroller': { fontFamily: 'inherit', lineHeight: '1.5', overflow: 'auto' },
      '.cm-content': { padding: '0', caretColor: '#1a1a1a' },
      '.cm-line': { padding: '4px 12px 4px 4px' },
      '.cm-gutters': { background: 'transparent', border: 'none', color: '#8a8490' },
      '.cm-row-gutter-col': { width: '56px', minWidth: '56px' },
      '.cm-row-gutter': {
        display: 'flex',
        alignItems: 'flex-start',
        justifyContent: 'flex-end',
        gap: '4px',
        padding: '4px 6px 4px 4px',
        fontSize: '12px',
        fontVariantNumeric: 'tabular-nums',
        userSelect: 'none',
      },
      '.cm-lineno': { minWidth: '1.5em', textAlign: 'right' },
      '.cm-dots': {
        display: 'inline-flex',
        gap: '2px',
        alignItems: 'center',
        minHeight: '20px',
      },
      '.cm-dots i': {
        width: '6px',
        height: '6px',
        borderRadius: '50%',
        display: 'inline-block',
      },
      // 争议与正文同中性斑马底，不加彩色块底
      '.cm-zebra0': { backgroundColor: '#f7f5f0' },
      '.cm-zebra1': { backgroundColor: '#efebe3' },
      // 自己候选整块略调明暗（含上下文/续行）；正式行无 cm-dispute，他人仍原 zebra
      '.cm-dispute.cm-self.cm-zebra0': { backgroundColor: '#fcfaf6' },
      '.cm-dispute.cm-self.cm-zebra1': { backgroundColor: '#e6e0d4' },
      '.cm-block-start.cm-self': {
        boxShadow: 'inset 3px 0 0 rgba(0,0,0,0.14)',
      },
      // 同组候选之间一条淡线；组顶/底不画，避免与邻接正文/邻组叠双线
      '.cm-sep-before': {
        borderTop: '1px solid rgba(0,0,0,0.12)',
        paddingTop: '4px',
      },
      '.cm-context.cm-self': { fontWeight: '500' },
      '.cm-other': { cursor: 'pointer' },
      '.cm-delete': { fontStyle: 'italic', color: '#8a8490' },
      '.cm-faded': { opacity: '0.45' },
      '.cm-remote-caret': {
        display: 'inline-block',
        width: '2px',
        height: '1.2em',
        verticalAlign: 'text-bottom',
        opacity: '0.9',
        pointerEvents: 'none',
      },
    }),
  ]
}

onMounted(() => {
  if (!host.value) return
  rowsRef = props.rows
  serverRows = props.rows
  cursorsRef = props.cursors
  meRef = props.me
  view = new EditorView({
    parent: host.value,
    state: EditorState.create({
      doc: rowsToDoc(props.rows),
      extensions: buildExtensions(),
    }),
  })
  view.dispatch({
    effects: [setRowsEffect.of(props.rows), setCursorsEffect.of(props.cursors)],
  })
  const initial = props.rows.findIndex((r) => r.lineId === props.initialLineId && r.isSelf)
  if (initial >= 0) view.dispatch({ selection: EditorSelection.cursor(caretPosInRows(props.rows, initial, 0)) })
  view.focus()
})

onBeforeUnmount(() => {
  clearSuspendTimer()
  view?.destroy()
  view = null
})

watch(
  () => props.rows,
  (next) => applyRows(next),
)

watch(
  () => props.cursors,
  (cs) => {
    cursorsRef = cs || []
    view?.dispatch({ effects: setCursorsEffect.of(cursorsRef) })
  },
)

watch(
  () => props.me,
  (m) => {
    meRef = m
    view?.dispatch({ effects: setCursorsEffect.of(cursorsRef) })
  },
)
</script>

<template>
  <div ref="host" class="single-editor" />
  <ModalDialog :model-value="!!followTarget" title="接受这份主张" :busy="following" @update:model-value="followTarget = ''">
    <p>接受他的主张，放弃我的？</p>
    <div class="actions"><button :disabled="following" @click="confirmFollow">接受</button><button class="quiet" :disabled="following" @click="confirmReject">拒绝</button><button class="quiet" :disabled="following" autofocus @click="followTarget = ''">取消</button></div>
  </ModalDialog>
</template>

<style scoped>
.single-editor {
  flex: 1;
  min-height: 0;
  height: 100%;
  overflow: hidden;
  text-align: left;
}
.single-editor :deep(.cm-editor) {
  height: 100%;
}
</style>
