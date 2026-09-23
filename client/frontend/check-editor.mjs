import assert from 'node:assert/strict'
import { createServer } from 'vite'

const server = await createServer({ server: { middlewareMode: true } })
try {
  const { Text, ChangeSet } = await server.ssrLoadModule('@codemirror/state')
  const { buildSpanReplacement, cursorRowIndex, textChange, changedRange } = await server.ssrLoadModule('/src/projection.ts')
  const { buildVisualRows } = await server.ssrLoadModule('/src/layout.ts')
  const snapshot = {
    article: { id: 'article', title: '光标位置' },
    lines: [{ id: 'anchor', content: '上下文' }, { id: 'next', content: '后续正文' }],
    disputes: [{ id: 'alice-claim', realLine: 'anchor', action: '插在后面', person: 'alice', content: ['甲的第一行', '甲的第二行'], followers: [] }],
    people: [], cursors: [], suspended: [],
  }
  const rows = buildVisualRows(snapshot, 'me')
  assert.equal(rows[cursorRowIndex(rows, 'anchor', 'alice-claim', 1)].content, '甲的第二行')
  assert.equal(rows[cursorRowIndex(rows, 'anchor', '', 0, 'alice')].content, '上下文')
  assert.equal(rows.filter((r) => r.lineId === 'next').length, 1)
  const beforeSnapshot = { ...snapshot, disputes: [
    { ...snapshot.disputes[0], action: '插在前面' },
    { id: 'my-before', realLine: 'anchor', action: '插在前面', person: 'me', content: ['我新增的一行'], followers: [] },
  ] }
  const beforeRows = buildVisualRows(beforeSnapshot, 'me')
  const myInsert = beforeRows.findIndex((r) => r.disputeId === 'my-before')
  assert.equal(beforeRows[myInsert].content, '我新增的一行')
  assert.equal(beforeRows[myInsert + 1].content, '上下文')
  assert.equal(beforeRows[myInsert + 1].contextAction, '插在前面')
  assert.ok(beforeRows[myInsert].showLineNo)
  assert.equal(beforeRows.filter((r) => r.lineId === 'next').length, 1)
  const withOwnBody = buildVisualRows({ ...beforeSnapshot, disputes: [...beforeSnapshot.disputes,
    { id: 'my-body', realLine: 'anchor', action: '改这行', person: 'me', content: ['上下文'], followers: [] }],
    suspended: ['my-body'] }, 'me')
  assert.equal(withOwnBody.filter((r) => r.isSelf && r.content === '上下文').length, 1)
  // 选中“被选”后回车，必须替换选区；跨行替换保留未选中的首尾。
  assert.deepEqual(buildSpanReplacement(Text.of(['甲被选乙']), 1, 3, '\n'), ['甲', '乙'])
  assert.deepEqual(buildSpanReplacement(Text.of(['甲乙', '丙丁']), 1, 4, '新\n文'), ['甲新', '文丁'])
  const before = '甲的文字\n乙的文字'
  const after = '甲的文字\n乙的新文字'
  const change = textChange(before, after)
  assert.equal(before.slice(0, change.from) + change.insert + before.slice(change.to), after)
  assert.ok(change.from >= '甲的文字\n'.length, '远端第二行变化不能重写第一行撤销范围')
  const emptyLines = Text.of(['', '', '正文'])
  const removeFirst = ChangeSet.of({ from: 0, to: 1 }, emptyLines.length)
  assert.deepEqual(changedRange(removeFirst, removeFirst.apply(emptyLines)), { from: 0, to: 1, insert: '' })
  console.log('编辑器回归检查通过：选区替换、多行光标、前后插入上下文、局部同步')
} finally {
  await server.close()
}
