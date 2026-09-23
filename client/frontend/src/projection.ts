import type { Text } from '@codemirror/state'
import { ACTION_DELETE, ACTION_INSERT, type VisualRow } from './types'

/** CM doc = 各 VisualRow.content 用换行拼成；逻辑行号 0-based ↔ rows[i] */
export function rowsToDoc(rows: VisualRow[]): string {
  return rows.map((r) => r.content ?? '').join('\n')
}

/** 同一正式行或同一份多行主张视为一个可写单元 */
export function unitKey(row: VisualRow): string {
  if (row.disputeId) return `d:${row.disputeId}`
  // phantom / 乐观跨度：无 disputeId，靠 spanBaseIDs 成组
  if (row.spanBaseIDs && row.spanBaseIDs.length >= 2) {
    return `s:${row.spanBaseIDs[0]}`
  }
  return `l:${row.lineId}`
}

/** 可参与 SubmitSpanEdit 的连续普通正式行（无争议块/插入上下文/他人候选） */
export function isPlainFormalRow(row: VisualRow | undefined): boolean {
  if (!row) return false
  return (
    row.editable &&
    row.isSelf &&
    !row.isContext &&
    !row.disputeId &&
    !row.phantom &&
    !row.spanBaseIDs?.length &&
    row.action !== ACTION_INSERT &&
    row.action !== ACTION_DELETE &&
    row.partCount === 1 &&
    row.partIndex === 0
  )
}

/** 触及行是否构成合法整段跨度（视觉连续 + 正式行号连续） */
export function isValidFormalSpan(rows: VisualRow[], idxs: number[]): boolean {
  if (idxs.length < 2) return false
  for (let i = 1; i < idxs.length; i++) {
    if (idxs[i] !== idxs[i - 1] + 1) return false
  }
  const spanRows = idxs.map((i) => rows[i])
  if (!spanRows.every(isPlainFormalRow)) return false
  for (let i = 1; i < spanRows.length; i++) {
    if (spanRows[i].lineIndex !== spanRows[i - 1].lineIndex + 1) return false
  }
  return true
}

/** 旧 doc 选区起点行内前缀（未含插入） */
export function spanSelectionPrefix(doc: Text, from: number, to: number): string {
  const a = Math.min(from, to)
  const startLine = doc.lineAt(a)
  return startLine.text.slice(0, a - startLine.from)
}

/** 旧 doc 选区 + 插入文本 → 整段 replacement[] */
export function buildSpanReplacement(
  doc: Text,
  from: number,
  to: number,
  insertText: string,
): string[] {
  const a = Math.min(from, to)
  const b = Math.max(from, to)
  const startLine = doc.lineAt(a)
  const endLine = doc.lineAt(Math.min(b, doc.length))
  const prefix = startLine.text.slice(0, a - startLine.from)
  const suffix = endLine.text.slice(Math.max(0, b - endLine.from))
  return (prefix + insertText + suffix).split('\n')
}

/** 跨度替换后光标落在 prefix+insert 末尾对应 part */
export function caretAfterSpanInsert(
  prefix: string,
  insertText: string,
): { part: number; offset: number } {
  const bits = (prefix + insertText).split('\n')
  return { part: bits.length - 1, offset: bits[bits.length - 1].length }
}

/** rows 文档坐标：第 rowIndex 行 offset 处的绝对 pos */
export function caretPosInRows(
  rows: VisualRow[],
  rowIndex: number,
  offset: number,
): number {
  let pos = 0
  for (let i = 0; i < rowIndex && i < rows.length; i++) {
    pos += (rows[i].content ?? '').length + 1
  }
  const line = rows[rowIndex]
  const len = line ? (line.content ?? '').length : 0
  return pos + Math.min(Math.max(0, offset), len)
}

/** 某主张在 rows 里的全部 part 下标（按 partIndex 升序） */
export function unitLineIndices(rows: VisualRow[], rowIndex: number): number[] {
  const row = rows[rowIndex]
  if (!row) return []
  const key = unitKey(row)
  const idxs: { i: number; part: number }[] = []
  for (let i = 0; i < rows.length; i++) {
    if (unitKey(rows[i]) === key) idxs.push({ i, part: rows[i].partIndex })
  }
  idxs.sort((a, b) => a.part - b.part)
  return idxs.map((x) => x.i)
}

export function keyOffsetFromPos(
  doc: Text,
  rows: VisualRow[],
  pos: number,
): { key: string; offset: number } | null {
  if (rows.length === 0) return null
  const line = doc.lineAt(Math.max(0, Math.min(pos, doc.length)))
  const row = rows[line.number - 1]
  if (!row) return null
  return { key: row.key, offset: Math.max(0, pos - line.from) }
}

export function posFromKeyOffset(
  doc: Text,
  rows: VisualRow[],
  key: string,
  offset: number,
): number {
  const idx = rows.findIndex((r) => r.key === key)
  if (idx < 0) {
    return Math.min(offset, doc.length)
  }
  if (idx >= doc.lines) return doc.length
  const line = doc.line(idx + 1)
  return line.from + Math.min(Math.max(0, offset), line.length)
}

/** 变更触及的旧文档行号（0-based）。删到换行时两行都算触及。 */
export function touchedLineIndexes(
  doc: Text,
  fromA: number,
  toA: number,
): number[] {
  const start = doc.lineAt(fromA).number - 1
  let end = start
  if (toA > fromA) {
    end = doc.lineAt(Math.min(toA - 1, doc.length)).number - 1
    if (doc.sliceString(fromA, toA).includes('\n')) {
      end = Math.max(end, doc.lineAt(Math.min(toA, doc.length)).number - 1)
    }
  } else {
    end = doc.lineAt(Math.min(toA, doc.length)).number - 1
  }
  const out: number[] = []
  for (let i = start; i <= end; i++) out.push(i)
  if (out.length === 0) out.push(start)
  return out
}
