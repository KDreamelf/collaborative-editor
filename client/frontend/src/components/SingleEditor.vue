<script lang="ts" setup>
import { defaultKeymap, history, historyKeymap } from '@codemirror/commands'
import {
  Annotation,
  EditorSelection,
  EditorState,
  StateEffect,
  StateField,
  type Extension,
  type Text,
  type TransactionSpec,
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
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  DeleteLine,
  MergeUp,
  MoveCaret,
  RequestFollow,
  SubmitEdit,
  SubmitInsert,
  SubmitPaste,
  SubmitSpanEdit,
  Suspend,
} from '../../wailsjs/go/main/App'
import {
  buildSpanReplacement,
  caretAfterSpanInsert,
  caretPosInRows,
  isValidFormalSpan,
  keyOffsetFromPos,
  posFromKeyOffset,
  rowsToDoc,
  spanSelectionPrefix,
  touchedLineIndexes,
  unitKey,
  unitLineIndices,
} from '../projection'
import {
  ACTION_DELETE,
  ACTION_EDIT,
  ACTION_INSERT,
  Cursor,
  VisualRow,
  colorFor,
} from '../types'

const props = defineProps<{
  rows: VisualRow[]
  me: string
  cursors: Cursor[]
}>()

const emit = defineEmits<{
  (e: 'error', err: unknown): void
}>()

const host = ref<HTMLElement | null>(null)

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
let pendingRows: VisualRow[] | null = null
let focusKey = ''
let suspendTimer: number | null = null
let suspendedLocal = false
let lastCaretKey = ''
let lastCaretOff = 0
let lastCaretEnd = 0
let suppressSubmit = false
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
  return run
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
  docLen: number,
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
  enqueueSpanEdit(base[0], base[base.length - 1], replacement).catch((e) => {
    showError(e)
    revertDoc()
  })
  return {
    changes: { from: 0, to: docLen, insert: rowsToDoc(next) },
    selection: EditorSelection.cursor(caretPosInRows(next, partIdx, caret.offset)),
    annotations: syncAnn.of(true),
    effects: [setRowsEffect.of(next), setCursorsEffect.of(cursorsRef)],
  }
}

function commitSpanFromView(
  v: EditorView,
  idxs: number[],
  fromA: number,
  toA: number,
  inserted: string,
) {
  const spec = applySpanLocally(
    v.state.doc,
    v.state.doc.length,
    idxs,
    fromA,
    toA,
    inserted,
  )
  suppressSubmit = true
  v.dispatch(spec)
  suppressSubmit = false
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
    changes: { from: 0, to: v.state.doc.length, insert: rowsToDoc(next) },
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
    annotations: syncAnn.of(true),
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
    Suspend(row.lineId, row.action, true).catch(showError)
  }, 60_000)
}

async function resumeSuspend(row: VisualRow) {
  if (suspendedLocal && row.isSelf) {
    suspendedLocal = false
    try {
      await Suspend(row.lineId, row.action, false)
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
      await Suspend(row.lineId, row.action, true)
    } catch (e) {
      showError(e)
    }
  }
}

function submitClaim(row: VisualRow, lines: string[]) {
  const span = row.spanBaseIDs
  if (span && span.length >= 2) {
    return enqueueSpanEdit(span[0], span[span.length - 1], lines)
  }
  // 上下文行只改正式锚点，绝不进 SubmitInsert
  if (row.isContext) {
    return lines.length > 1 ? SubmitPaste(row.lineId, lines) : SubmitEdit(row.lineId, lines[0] ?? '')
  }
  if (row.action === ACTION_INSERT) {
    return SubmitInsert(row.lineId, lines)
  }
  return lines.length > 1 ? SubmitPaste(row.lineId, lines) : SubmitEdit(row.lineId, lines[0] ?? '')
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
  lastCaretEnd = b && b.key === a.key ? b.offset : a.offset
}

function emitCaretFromView(v: EditorView) {
  const sel = v.state.selection.main
  const info = keyOffsetFromPos(v.state.doc, rowsRef, sel.head)
  if (!info) return
  const row = rowsRef.find((r) => r.key === info.key)
  if (!row) return
  const endInfo = keyOffsetFromPos(v.state.doc, rowsRef, sel.anchor)
  const offset = info.offset
  const selEnd = endInfo && endInfo.key === info.key ? endInfo.offset : offset
  lastCaretKey = info.key
  lastCaretOff = offset
  lastCaretEnd = selEnd
  focusKey = info.key
  // 上下文：本人=正式行；他人假 disputeId 不上传，避免远端画错行
  const disputeId = row.isContext ? '' : row.disputeId || ''
  if (row.isContext && !row.isSelf) return
  MoveCaret(row.lineId, disputeId, offset, selEnd).catch(() => {})
}

function applyRows(next: VisualRow[], force = false) {
  if (!view) return
  if (composing && !force) {
    pendingRows = next
    return
  }
  pendingRows = null
  saveCaret(view)
  const doc = rowsToDoc(next)
  const same = doc === view.state.doc.toString()
  serverRows = next
  rowsRef = next
  if (same) {
    view.dispatch({
      effects: [setRowsEffect.of(next), setCursorsEffect.of(cursorsRef)],
    })
    return
  }
  suppressSubmit = true
  view.dispatch({
    changes: { from: 0, to: view.state.doc.length, insert: doc },
    annotations: syncAnn.of(true),
    effects: [setRowsEffect.of(next), setCursorsEffect.of(cursorsRef)],
  })
  const from = posFromKeyOffset(view.state.doc, next, lastCaretKey, lastCaretOff)
  const to = posFromKeyOffset(view.state.doc, next, lastCaretKey, lastCaretEnd)
  view.dispatch({
    selection: EditorSelection.range(Math.min(from, to), Math.max(from, to)),
    annotations: syncAnn.of(true),
  })
  suppressSubmit = false
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
  if (row.action === ACTION_INSERT) parts.push('cm-insert')
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
      const idx = rows.findIndex(
        (r) =>
          r.lineId === c.lineId &&
          (r.disputeId || '') === (c.disputeId || '') &&
          !r.isContext &&
          r.partIndex === 0,
      )
      if (idx < 0 || idx >= tr.state.doc.lines) continue
      const line = tr.state.doc.line(idx + 1)
      const a = line.from + Math.max(0, Math.min(c.offset, line.length))
      const b = line.from + Math.max(0, Math.min(c.selEnd ?? c.offset, line.length))
      const from = Math.min(a, b)
      const to = Math.max(a, b)
      const col = colorFor(c.personId)
      if (from !== to) {
        marks.push({
          from,
          to,
          value: Decoration.mark({
            attributes: { style: `background:${col}55` },
          }),
        })
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

function afterUserEdit(v: EditorView) {
  if (suppressSubmit || composing) return
  const rows = rowsRef
  if (!rows.length) return
  const head = v.state.selection.main.head
  const line = v.state.doc.lineAt(head)
  const idx = line.number - 1
  const row = rows[idx]
  if (!row || !row.editable) return
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
  submitClaim(row, payload).catch((e) => {
    showError(e)
    revertDoc()
  })
  emitCaretFromView(v)
  armSuspend(row)
}

function handleEnter(v: EditorView): boolean {
  if (composing) return true
  const sel = v.state.selection.main
  if (!sel.empty) {
    const verdict = changeAllowed(rowsRef, v.state.doc, sel.from, sel.to)
    if (verdict === 'cross') {
      showError(CROSS_MSG)
      return true
    }
    if (verdict === 'protected') return true
    if (verdict === 'span') {
      const idxs = touchedLineIndexes(v.state.doc, sel.from, sel.to)
      commitSpanFromView(v, idxs, sel.from, sel.to, '\n')
      return true
    }
  }
  const line = v.state.doc.lineAt(sel.head)
  const idx = line.number - 1
  const row = rowsRef[idx]
  if (!row || !row.editable) return true
  const offset = sel.head - line.from
  const val = line.text
  void (async () => {
    try {
      if (offset >= val.length && row.partIndex === row.partCount - 1) {
        if (!row.isContext && row.action === ACTION_INSERT) {
          const lines = payloadLines(rowsRef, idx, val)
          lines.push('')
          await SubmitInsert(row.lineId, lines)
        } else {
          // 上下文/正式行末尾 Enter：新开插入主张
          await SubmitInsert(row.lineId, [''])
        }
        return
      }
      const left = val.slice(0, offset)
      const right = val.slice(offset)
      if (row.isContext) {
        await submitClaim(row, [left, right])
        return
      }
      const lines = payloadLines(rowsRef, idx, left)
      lines.splice(row.partIndex + 1, 0, right)
      lines[row.partIndex] = left
      if (row.spanBaseIDs && row.spanBaseIDs.length >= 2) {
        commitSpanClaim(v, spanClaimIdxs(idx), lines, row.partIndex + 1, 0)
        return
      }
      await submitClaim(row, lines)
    } catch (e) {
      showError(e)
      revertDoc()
    }
  })()
  return true
}

function handleBackspace(v: EditorView): boolean {
  if (composing) return false
  const sel = v.state.selection.main
  if (!sel.empty) return false
  const line = v.state.doc.lineAt(sel.head)
  if (sel.head - line.from !== 0) return false
  const idx = line.number - 1
  const row = rowsRef[idx]
  if (!row || !row.editable) return true
  void (async () => {
    try {
      if (row.isContext) {
        if (line.text.length === 0) await DeleteLine(row.lineId)
        else await MergeUp(row.lineId)
        return
      }
      if (row.partIndex > 0) {
        const lines = payloadLines(rowsRef, idx, line.text)
        const joinAt = (lines[row.partIndex - 1] || '').length
        lines[row.partIndex - 1] = (lines[row.partIndex - 1] || '') + (lines[row.partIndex] || '')
        lines.splice(row.partIndex, 1)
        if (row.spanBaseIDs && row.spanBaseIDs.length >= 2) {
          commitSpanClaim(v, spanClaimIdxs(idx), lines, row.partIndex - 1, joinAt)
          return
        }
        await submitClaim(row, lines)
        return
      }
      if (row.action === ACTION_INSERT || row.action === ACTION_DELETE) return
      if (line.text.length === 0) await DeleteLine(row.lineId)
      else await MergeUp(row.lineId)
    } catch (e) {
      showError(e)
      revertDoc()
    }
  })()
  return true
}

function buildExtensions(): Extension {
  return [
    rowsField,
    cursorsField,
    lineDecoField,
    remoteCursorField,
    rowGutter,
    history(),
    EditorView.lineWrapping,
    keymap.of([
      { key: 'Enter', run: handleEnter },
      { key: 'Backspace', run: handleBackspace },
    ]),
    keymap.of(historyKeymap),
    keymap.of(defaultKeymap.filter((b) => b.key !== 'Enter')),
    // 跨度：改写为本笔乐观折叠；保护/跨争议直接丢弃
    EditorState.transactionFilter.of((tr) => {
      if (!tr.docChanged || tr.annotation(syncAnn)) return tr
      const rows = rowsRef
      const ranges: { fromA: number; toA: number }[] = []
      let protectedHit = false
      let crossHit = false
      let spanHit = false
      tr.changes.iterChangedRanges((fromA, toA) => {
        ranges.push({ fromA, toA })
        const v = changeAllowed(rows, tr.startState.doc, fromA, toA)
        if (v === 'protected') protectedHit = true
        if (v === 'cross') crossHit = true
        if (v === 'span') spanHit = true
      })
      if (protectedHit) return []
      if (crossHit || (spanHit && ranges.length !== 1)) {
        queueMicrotask(() => showError(CROSS_MSG))
        return []
      }
      if (!spanHit) return tr
      const { fromA, toA } = ranges[0]
      let inserted = ''
      tr.changes.iterChanges((f, t, _fb, _tb, ins) => {
        if (f === fromA && t === toA) inserted = ins.toString()
      })
      const idxs = touchedLineIndexes(tr.startState.doc, fromA, toA)
      return [
        applySpanLocally(
          tr.startState.doc,
          tr.startState.doc.length,
          idxs,
          fromA,
          toA,
          inserted,
        ),
      ]
    }),
    EditorView.updateListener.of((vu: ViewUpdate) => {
      if (vu.selectionSet) emitCaretFromView(vu.view)
      if (vu.docChanged && !vu.transactions.some((t) => t.annotation(syncAnn))) {
        afterUserEdit(vu.view)
      }
    }),
    EditorView.domEventHandlers({
      compositionstart: () => {
        composing = true
        return false
      },
      compositionend: (_e, v) => {
        composing = false
        if (pendingRows) {
          const p = pendingRows
          pendingRows = null
          applyRows(p, true)
        }
        afterUserEdit(v)
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
        const sel = v.state.selection.main
        const verdict = changeAllowed(rowsRef, v.state.doc, sel.from, sel.to)
        if (verdict === 'span') {
          e.preventDefault()
          const normalized = text.replace(/\r\n/g, '\n').replace(/\r/g, '\n')
          const idxs = touchedLineIndexes(v.state.doc, sel.from, sel.to)
          commitSpanFromView(v, idxs, sel.from, sel.to, normalized)
          return true
        }
        if (!text.includes('\n')) return false
        e.preventDefault()
        const line = v.state.doc.lineAt(sel.head)
        const idx = line.number - 1
        const row = rowsRef[idx]
        if (!row || !row.editable) return true
        if (verdict === 'cross') {
          showError(CROSS_MSG)
          return true
        }
        if (verdict === 'protected') return true
        if (v.state.doc.lineAt(sel.from).number !== v.state.doc.lineAt(sel.to).number) {
          showError(CROSS_MSG)
          return true
        }
        const start = Math.min(sel.from, sel.to) - line.from
        const end = Math.max(sel.from, sel.to) - line.from
        const before = line.text.slice(0, start)
        const after = line.text.slice(end)
        const chunks = text.replace(/\r\n/g, '\n').replace(/\r/g, '\n').split('\n')
        chunks[0] = before + chunks[0]
        chunks[chunks.length - 1] = chunks[chunks.length - 1] + after
        if (row.isContext) {
          submitClaim(row, chunks).catch((err) => {
            showError(err)
            revertDoc()
          })
          return true
        }
        const lines = payloadLines(rowsRef, idx, chunks[0])
        lines.splice(row.partIndex, 1, ...chunks)
        if (row.spanBaseIDs && row.spanBaseIDs.length >= 2) {
          const caretPart = row.partIndex + chunks.length - 1
          const caretOff = chunks[chunks.length - 1].length - after.length
          commitSpanClaim(v, spanClaimIdxs(idx), lines, caretPart, caretOff)
          return true
        }
        submitClaim(row, lines).catch((err) => {
          showError(err)
          revertDoc()
        })
        return true
      },
      click: (e, v) => {
        const pos = v.posAtCoords({ x: e.clientX, y: e.clientY })
        if (pos == null) return false
        const line = v.state.doc.lineAt(pos)
        const row = rowsRef[line.number - 1]
        const follow = row?.followId || ''
        if (!row || row.isSelf || row.phantom || !follow) return false
        if (!window.confirm('接受他的，放弃我的？')) return true
        RequestFollow(follow).catch(showError)
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
