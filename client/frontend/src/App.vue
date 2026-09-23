<script lang="ts" setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import {
  AnswerFollow,
  CreateArticle,
  DeleteLine,
  Join,
  ListArticles,
  MergeUp,
  MoveCaret,
  PersonID,
  RequestFollow,
  SubmitEdit,
  SubmitInsert,
  SubmitPaste,
  Suspend,
} from '../wailsjs/go/main/App'
import type { ArticleBrief } from '../wailsjs/go/main/App'
import { EventsOn } from '../wailsjs/runtime/runtime'
import { buildVisualRows } from './layout'
import {
  ACTION_EDIT,
  Cursor,
  FollowAsk,
  Snapshot,
  VisualRow,
  colorFor,
} from './types'

const ESTIMATE = 28
const OVERSCAN = 8

const me = ref('')
const name = ref('')
const title = ref('')
const articles = ref<ArticleBrief[]>([])
const joined = ref(false)
const errorText = ref('')
const snap = ref<Snapshot | null>(null)
const cursors = ref<Cursor[]>([])

const viewport = ref<HTMLElement | null>(null)
const scrollTop = ref(0)
const viewH = ref(600)
const heights = ref<Record<string, number>>({})

const composing = ref(false)
const focusKey = ref('')
const suspendTimer = ref<number | null>(null)
const suspendedLocal = ref(false)

const rows = computed(() => buildVisualRows(snap.value, me.value))

const offsets = computed(() => {
  const list = rows.value
  const off: number[] = new Array(list.length + 1)
  off[0] = 0
  for (let i = 0; i < list.length; i++) {
    off[i + 1] = off[i] + (heights.value[list[i].key] || ESTIMATE)
  }
  return off
})

const totalH = computed(() => offsets.value[offsets.value.length - 1] || 0)

const range = computed(() => {
  const list = rows.value
  const off = offsets.value
  let start = 0
  while (start < list.length && off[start + 1] < scrollTop.value - OVERSCAN * ESTIMATE) {
    start++
  }
  let end = start
  const bottom = scrollTop.value + viewH.value + OVERSCAN * ESTIMATE
  while (end < list.length && off[end] < bottom) {
    end++
  }
  return { start, end }
})

const visibleRows = computed(() => rows.value.slice(range.value.start, range.value.end))
const padTop = computed(() => offsets.value[range.value.start] || 0)
const padBottom = computed(() => Math.max(0, totalH.value - (offsets.value[range.value.end] || 0)))

function loadArticles() {
  ListArticles()
    .then((list) => {
      articles.value = list || []
    })
    .catch((e) => {
      errorText.value = String(e)
    })
}

async function onCreate() {
  try {
    const id = await CreateArticle(title.value)
    await enter(id)
  } catch (e) {
    errorText.value = String(e)
  }
}

async function enter(id: string) {
  try {
    await Join(id, name.value || '未命名')
    joined.value = true
    errorText.value = ''
  } catch (e) {
    errorText.value = String(e)
  }
}

function onScroll() {
  if (!viewport.value) return
  scrollTop.value = viewport.value.scrollTop
}

function measure(el: Element | null, key: string) {
  if (!el || !(el instanceof HTMLElement)) return
  const h = el.offsetHeight
  if (h > 0 && heights.value[key] !== h) {
    heights.value = { ...heights.value, [key]: h }
  }
}

function setRowRef(key: string, el: Element | null) {
  measure(el, key)
}

function autosize(el: HTMLTextAreaElement | null) {
  if (!el) return
  el.style.height = '0px'
  el.style.height = el.scrollHeight + 'px'
}

function clearSuspendTimer() {
  if (suspendTimer.value != null) {
    window.clearTimeout(suspendTimer.value)
    suspendTimer.value = null
  }
}

function armSuspend(row: VisualRow) {
  clearSuspendTimer()
  if (!row.editable || !row.isSelf) return
  suspendTimer.value = window.setTimeout(() => {
    if (focusKey.value !== row.key) return
    suspendedLocal.value = true
    Suspend(row.lineId, row.action, true).catch((e) => (errorText.value = String(e)))
  }, 60_000)
}

async function onFocus(row: VisualRow) {
  focusKey.value = row.key
  if (suspendedLocal.value && row.isSelf) {
    suspendedLocal.value = false
    try {
      await Suspend(row.lineId, row.action, false)
    } catch (e) {
      errorText.value = String(e)
    }
  }
  armSuspend(row)
  emitCaret(row)
}

async function onBlur(row: VisualRow) {
  if (focusKey.value === row.key) {
    focusKey.value = ''
    clearSuspendTimer()
    if (row.isSelf && !suspendedLocal.value) {
      suspendedLocal.value = true
      try {
        await Suspend(row.lineId, row.action, true)
      } catch (e) {
        errorText.value = String(e)
      }
    }
  }
}

function emitCaret(row: VisualRow, el?: HTMLTextAreaElement | null) {
  const ta = el || (document.activeElement as HTMLTextAreaElement | null)
  let offset = 0
  let selEnd = 0
  if (ta && ta.tagName === 'TEXTAREA') {
    offset = ta.selectionStart || 0
    selEnd = ta.selectionEnd || offset
  }
  MoveCaret(row.lineId, row.disputeId || '', offset, selEnd).catch(() => {})
}

function claimLines(row: VisualRow, partText: string): string[] {
  if (row.partCount <= 1 || !row.disputeId || !snap.value) {
    return [partText]
  }
  const d = (snap.value.disputes || []).find((x) => x.id === row.disputeId)
  const base = d && d.content && d.content.length > 0 ? [...d.content] : Array(row.partCount).fill('')
  while (base.length < row.partCount) base.push('')
  base[row.partIndex] = partText
  return base as string[]
}

function onInput(row: VisualRow, ev: Event) {
  if (composing.value) return
  const ta = ev.target as HTMLTextAreaElement
  autosize(ta)
  measure(ta.closest('.row'), row.key)
  armSuspend(row)
  const lines = claimLines(row, ta.value)
  const job =
    lines.length > 1 ? SubmitPaste(row.lineId, lines) : SubmitEdit(row.lineId, lines[0] ?? '')
  job.catch((e) => (errorText.value = String(e)))
  emitCaret(row, ta)
}

function onCompositionStart() {
  composing.value = true
}

function onCompositionEnd(row: VisualRow, ev: Event) {
  composing.value = false
  onInput(row, ev)
}

async function onKeydown(row: VisualRow, ev: KeyboardEvent) {
  const ta = ev.target as HTMLTextAreaElement
  if (ev.key === 'Enter' && !ev.shiftKey && !composing.value) {
    ev.preventDefault()
    const start = ta.selectionStart || 0
    const val = ta.value
    if (start >= val.length && row.partIndex === row.partCount - 1) {
      try {
        await SubmitInsert(row.lineId, [''])
      } catch (e) {
        errorText.value = String(e)
      }
      return
    }
    const left = val.slice(0, start)
    const right = val.slice(start)
    const lines = claimLines(row, left)
    lines.splice(row.partIndex + 1, 0, right)
    // claimLines 已写入 left 到 partIndex；右侧作为新行插入
    lines[row.partIndex] = left
    try {
      await SubmitPaste(row.lineId, lines)
    } catch (e) {
      errorText.value = String(e)
    }
    return
  }
  if (ev.key === 'Backspace' && !composing.value) {
    const start = ta.selectionStart || 0
    const end = ta.selectionEnd || 0
    if (start === 0 && end === 0) {
      ev.preventDefault()
      try {
        if (row.partIndex > 0) {
          const lines = claimLines(row, ta.value)
          lines[row.partIndex - 1] = (lines[row.partIndex - 1] || '') + (lines[row.partIndex] || '')
          lines.splice(row.partIndex, 1)
          await SubmitPaste(row.lineId, lines)
          return
        }
        if (ta.value.length === 0) {
          await DeleteLine(row.lineId)
        } else {
          await MergeUp(row.lineId)
        }
      } catch (e) {
        errorText.value = String(e)
      }
    }
  }
}

async function onPaste(row: VisualRow, ev: ClipboardEvent) {
  const text = ev.clipboardData?.getData('text/plain')
  if (!text || !text.includes('\n')) return
  ev.preventDefault()
  const ta = ev.target as HTMLTextAreaElement
  const start = ta.selectionStart || 0
  const end = ta.selectionEnd || 0
  const before = ta.value.slice(0, start)
  const after = ta.value.slice(end)
  const chunks = text.replace(/\r\n/g, '\n').replace(/\r/g, '\n').split('\n')
  chunks[0] = before + chunks[0]
  chunks[chunks.length - 1] = chunks[chunks.length - 1] + after
  const lines = claimLines(row, chunks[0])
  lines.splice(row.partIndex, 1, ...chunks)
  try {
    await SubmitPaste(row.lineId, lines)
  } catch (e) {
    errorText.value = String(e)
  }
}

async function onClaimClick(row: VisualRow) {
  if (row.isSelf || row.phantom || !row.disputeId) return
  if (!window.confirm('接受他的，放弃我的？')) return
  try {
    await RequestFollow(row.disputeId)
  } catch (e) {
    errorText.value = String(e)
  }
}

function dots(n: number, color: string) {
  return Array.from({ length: Math.max(0, n) }, (_, i) => ({ i, color }))
}

function rowCursors(row: VisualRow): Cursor[] {
  return cursors.value.filter(
    (c) =>
      c.personId !== me.value &&
      c.lineId === row.lineId &&
      (c.disputeId || '') === (row.disputeId || '') &&
      row.partIndex === 0,
  )
}

function sliceText(row: VisualRow, from: number, to: number): string {
  const s = row.content || ''
  const a = Math.max(0, Math.min(from, s.length))
  const b = Math.max(a, Math.min(to, s.length))
  return s.slice(a, b)
}

onMounted(async () => {
  me.value = await PersonID()
  loadArticles()
  EventsOn('snapshot', (s: Snapshot) => {
    snap.value = s
    if (s && s.cursors) cursors.value = s.cursors
  })
  EventsOn('cursors', (cs: Cursor[]) => {
    cursors.value = cs || []
  })
  EventsOn('followAsk', async (ask: FollowAsk) => {
    const ok = window.confirm(`${ask.fromName || '有人'}想追随你的主张，点头？`)
    try {
      await AnswerFollow(ask.fromId, ask.disputeId, ok)
    } catch (e) {
      errorText.value = String(e)
    }
  })
  EventsOn('followResult', () => {})
  EventsOn('error', (msg: string) => {
    errorText.value = msg
  })
  const ro = new ResizeObserver(() => {
    if (viewport.value) viewH.value = viewport.value.clientHeight
  })
  watch(
    viewport,
    (el, _, onCleanup) => {
      if (!el) return
      viewH.value = el.clientHeight
      ro.observe(el)
      onCleanup(() => ro.unobserve(el))
    },
    { immediate: true },
  )
  await nextTick()
})

onUnmounted(() => {
  clearSuspendTimer()
})

watch(rows, async () => {
  await nextTick()
  document.querySelectorAll('.row').forEach((el) => {
    const key = (el as HTMLElement).dataset.key
    if (key) measure(el, key)
  })
})
</script>

<template>
  <div v-if="!joined" class="lobby">
    <h1>协同编辑器</h1>
    <label>
      名字
      <input v-model="name" placeholder="未命名" autocomplete="off" />
    </label>
    <div class="create">
      <input v-model="title" placeholder="新文档标题" autocomplete="off" />
      <button type="button" @click="onCreate">新建</button>
    </div>
    <h2>已有文档</h2>
    <ul class="alist">
      <li v-for="a in articles" :key="a.id">
        <button type="button" class="link" @click="enter(a.id)">{{ a.title || a.id }}</button>
      </li>
    </ul>
    <p v-if="errorText" class="err">{{ errorText }}</p>
  </div>

  <div v-else class="editor-shell">
    <header class="bar">
      <span>{{ snap?.article?.title || '文档' }}</span>
      <span v-if="errorText" class="err">{{ errorText }}</span>
    </header>
    <div ref="viewport" class="viewport" @scroll="onScroll">
      <div class="pad" :style="{ height: padTop + 'px' }" />
      <div
        v-for="row in visibleRows"
        :key="row.key"
        class="row"
        :class="{
          zebra0: row.zebra === 0,
          zebra1: row.zebra === 1,
          dispute: !!row.disputeId || row.phantom,
          self: row.isSelf,
          other: !row.isSelf,
          faded: row.suspended,
          insert: row.action !== ACTION_EDIT,
        }"
        :data-key="row.key"
        :ref="(el) => setRowRef(row.key, el as Element | null)"
        @click="!row.isSelf && onClaimClick(row)"
      >
        <div class="gutter">
          <span v-if="row.showLineNo" class="lineno">{{ row.lineNo }}</span>
          <span class="dots">
            <i
              v-for="d in dots(row.followerCount, colorFor(row.personId))"
              :key="d.i"
              :style="{ background: d.color }"
            />
          </span>
        </div>
        <div class="body">
          <textarea
            v-if="row.editable"
            class="cell"
            :value="row.content"
            rows="1"
            spellcheck="false"
            @focus="onFocus(row)"
            @blur="onBlur(row)"
            @input="onInput(row, $event)"
            @keydown="onKeydown(row, $event)"
            @paste="onPaste(row, $event)"
            @compositionstart="onCompositionStart"
            @compositionend="onCompositionEnd(row, $event)"
            @select="emitCaret(row, $event.target as HTMLTextAreaElement)"
            @keyup="emitCaret(row, $event.target as HTMLTextAreaElement)"
            @click.stop="emitCaret(row, $event.target as HTMLTextAreaElement)"
            :ref="(el) => autosize(el as HTMLTextAreaElement | null)"
          />
          <div v-else class="cell readonly">{{ row.content }}</div>
          <div
            v-for="c in rowCursors(row)"
            :key="c.personId + ':' + c.offset"
            class="remote-layer"
            :title="c.name"
          >
            <span class="mirror">{{ sliceText(row, 0, Math.min(c.offset, c.selEnd)) }}</span>
            <span
              v-if="c.selEnd !== c.offset"
              class="remote-sel"
              :style="{ background: colorFor(c.personId) + '55' }"
              >{{ sliceText(row, Math.min(c.offset, c.selEnd), Math.max(c.offset, c.selEnd)) }}</span
            >
            <span class="remote-caret" :style="{ background: colorFor(c.personId) }" />
          </div>
        </div>
      </div>
      <div class="pad" :style="{ height: padBottom + 'px' }" />
    </div>
  </div>
</template>

<style scoped>
.lobby {
  max-width: 420px;
  margin: 40px auto;
  padding: 16px;
  text-align: left;
  color: #1a1a1a;
}
.lobby h1 {
  font-size: 1.4rem;
  margin: 0 0 16px;
}
.lobby label {
  display: block;
  margin-bottom: 12px;
}
.lobby input {
  display: block;
  width: 100%;
  margin-top: 4px;
  box-sizing: border-box;
  padding: 8px 10px;
  border: 1px solid #ddd6c8;
  border-radius: 4px;
  background: #fff;
  color: #1a1a1a;
}
.create {
  display: flex;
  gap: 8px;
  margin-bottom: 20px;
}
.create input {
  flex: 1;
  margin-top: 0;
}
.create button,
.alist .link {
  border: none;
  border-radius: 4px;
  padding: 8px 12px;
  background: #3d7eff;
  color: #fff;
  cursor: pointer;
}
.alist {
  list-style: none;
  padding: 0;
  margin: 0;
}
.alist li {
  margin: 6px 0;
}
.alist .link {
  background: transparent;
  color: #9ec1ff;
  padding: 4px 0;
}
.err {
  color: #ff8a80;
  font-size: 0.9rem;
}
.editor-shell {
  display: flex;
  flex-direction: column;
  height: 100vh;
  text-align: left;
  color: #1a1a1a;
  background: #f7f5f0;
}
.bar {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  padding: 8px 12px;
  background: #ece8df;
  border-bottom: 1px solid #ddd6c8;
  font-size: 0.9rem;
}
.viewport {
  flex: 1;
  overflow: auto;
  position: relative;
}
.pad {
  width: 100%;
  pointer-events: none;
}
.row {
  display: flex;
  align-items: stretch;
  min-height: 28px;
  margin: 0;
  padding: 0;
  border: 0;
  line-height: 1.5;
}
.row.zebra0 {
  background: #f7f5f0;
}
.row.zebra1 {
  background: #efebe3;
}
.row.dispute.zebra0 {
  background: #e7eef8;
}
.row.dispute.zebra1 {
  background: #dde7f4;
}
.row.dispute.self {
  background: #d2e3fc;
}
.row.dispute.other {
  cursor: pointer;
}
.row.faded {
  opacity: 0.45;
}
.gutter {
  flex: 0 0 56px;
  display: flex;
  align-items: flex-start;
  justify-content: flex-end;
  gap: 4px;
  padding: 4px 6px 4px 4px;
  user-select: none;
  color: #8a8490;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}
.lineno {
  min-width: 1.5em;
  text-align: right;
}
.dots {
  display: inline-flex;
  gap: 2px;
  align-items: center;
  min-height: 20px;
}
.dots i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  display: inline-block;
}
.body {
  flex: 1;
  position: relative;
  min-width: 0;
}
.cell {
  display: block;
  width: 100%;
  box-sizing: border-box;
  margin: 0;
  padding: 4px 12px 4px 4px;
  border: none;
  outline: none;
  resize: none;
  overflow: hidden;
  background: transparent;
  color: inherit;
  font: inherit;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-word;
  min-height: 28px;
}
.cell.readonly {
  cursor: pointer;
}
.remote-layer {
  position: absolute;
  inset: 0;
  padding: 4px 12px 4px 4px;
  pointer-events: none;
  white-space: pre-wrap;
  word-break: break-word;
  line-height: 1.5;
  font: inherit;
  overflow: hidden;
  color: transparent;
}
.mirror {
  white-space: pre-wrap;
}
.remote-sel {
  white-space: pre-wrap;
  border-radius: 2px;
}
.remote-caret {
  display: inline-block;
  width: 2px;
  height: 1.2em;
  vertical-align: text-bottom;
  opacity: 0.9;
}
</style>
