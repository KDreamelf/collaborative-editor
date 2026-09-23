<script lang="ts" setup>
import { computed, onMounted, ref } from 'vue'
import {
  AnswerFollow,
  CreateArticle,
  DismissUnsynced,
  GetCachedArticle,
  Join,
  ListArticles,
  ListUnsynced,
  PersonID,
} from '../wailsjs/go/main/App'
import { main } from '../wailsjs/go/models'
import { ClipboardSetText, EventsOn } from '../wailsjs/runtime/runtime'
import SingleEditor from './components/SingleEditor.vue'
import { formatUserError } from './errors'
import { buildVisualRows } from './layout'
import { Cursor, FollowAsk, Snapshot } from './types'

type ArticleBrief = main.ArticleBrief
type CachedArticle = main.CachedArticle
type UnsyncedItem = main.UnsyncedItem

const me = ref('')
const name = ref('')
const title = ref('')
const articles = ref<ArticleBrief[]>([])
const cached = ref<CachedArticle | null>(null)
const unsynced = ref<UnsyncedItem[]>([])
const showUnsynced = ref(false)
const offline = ref(false)
const joined = ref(false)
const errorText = ref('')
const snap = ref<Snapshot | null>(null)
const cursors = ref<Cursor[]>([])

const rows = computed(() => buildVisualRows(snap.value, me.value))

function showError(e: unknown) {
  errorText.value = formatUserError(e)
}

function loadArticles() {
  ListArticles()
    .then((list) => {
      articles.value = list || []
    })
    .catch(showError)
}

function loadCached() {
  GetCachedArticle()
    .then((c) => {
      cached.value = c || null
      if (c && c.name && !name.value) name.value = c.name
    })
    .catch(() => {
      cached.value = null
    })
}

function loadUnsynced() {
  ListUnsynced()
    .then((list) => {
      unsynced.value = list || []
    })
    .catch(() => {
      unsynced.value = []
    })
}

async function resumeCached() {
  if (!cached.value) return
  if (cached.value.name) name.value = cached.value.name
  await enter(cached.value.id)
}

async function copyUnsynced(item: UnsyncedItem) {
  const text = item.text || ''
  try {
    const hasWailsClip =
      typeof (window as unknown as { runtime?: { ClipboardSetText?: unknown } }).runtime
        ?.ClipboardSetText === 'function'
    if (hasWailsClip) {
      const ok = await ClipboardSetText(text)
      if (!ok) throw new Error('复制失败')
      return
    }
    await navigator.clipboard.writeText(text)
  } catch (e) {
    showError(e)
  }
}

async function dismissUnsynced(id: string) {
  if (!window.confirm('删除这份本地副本后将无法找回，确定删除？')) return
  try {
    await DismissUnsynced(id)
    loadUnsynced()
  } catch (e) {
    showError(e)
  }
}

async function onCreate() {
  try {
    const id = await CreateArticle(title.value)
    await enter(id)
  } catch (e) {
    showError(e)
  }
}

async function enter(id: string) {
  try {
    await Join(id, name.value || '未命名')
    joined.value = true
    errorText.value = ''
    cached.value = null
    loadUnsynced()
  } catch (e) {
    showError(e)
  }
}

onMounted(async () => {
  // 生产包剔除整段；仅 DEV + ?mock=1 动态拉演示数据
  if (import.meta.env.DEV && /(?:\?|&)mock=1(?:&|$)/.test(location.search)) {
    const { loadDevMock } = await import('./devMock')
    const m = loadDevMock()
    me.value = m.me
    joined.value = true
    snap.value = m.snap
    cursors.value = m.cursors
    return
  }
  me.value = await PersonID()
  loadArticles()
  loadCached()
  loadUnsynced()
  EventsOn('snapshot', (s: Snapshot) => {
    snap.value = s
    if (s && s.cursors) cursors.value = s.cursors
  })
  EventsOn('cursors', (cs: Cursor[]) => {
    cursors.value = cs || []
  })
  EventsOn('unsynced', (list: UnsyncedItem[]) => {
    unsynced.value = list || []
  })
  EventsOn('offline', (on: boolean) => {
    offline.value = !!on
  })
  EventsOn('followAsk', async (ask: FollowAsk) => {
    const ok = window.confirm(`${ask.fromName || '有人'}想追随你的主张，点头？`)
    try {
      await AnswerFollow(ask.fromId, ask.disputeId, ok)
    } catch (e) {
      showError(e)
    }
  })
  EventsOn('followResult', () => {})
  EventsOn('error', (msg: string) => {
    showError(msg)
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
    <div v-if="cached" class="resume">
      <p>
        上次的「{{ cached.title }}」还留在本机
        <template v-if="cached.pendingCount">，有 {{ cached.pendingCount }} 处待发送</template>
        <template v-if="cached.unsyncedCount">，有 {{ cached.unsyncedCount }} 处未能同步</template>
      </p>
      <button type="button" @click="resumeCached">继续编辑</button>
    </div>
    <div class="create">
      <input v-model="title" placeholder="新文档标题" autocomplete="off" />
      <button type="button" @click="onCreate">新建</button>
    </div>
    <h2>已有文档</h2>
    <ul class="alist">
      <li v-for="a in articles" :key="a.id">
        <button type="button" class="link" @click="enter(a.id)">{{ a.title || '未命名文档' }}</button>
      </li>
    </ul>
    <p v-if="errorText" class="err">{{ errorText }}</p>
  </div>

  <div v-else class="editor-shell">
    <header class="bar">
      <span>{{ snap?.article?.title || cached?.title || '文档' }}</span>
      <span v-if="offline" class="offline-hint">当前离线，改动会在连上后自动发送</span>
      <button
        v-if="unsynced.length"
        type="button"
        class="unsync-btn"
        @click="showUnsynced = !showUnsynced"
      >
        {{ unsynced.length }} 处未能同步
      </button>
      <span v-if="errorText" class="err">{{ errorText }}</span>
    </header>
    <div v-if="showUnsynced && unsynced.length" class="unsync-panel">
      <div v-for="u in unsynced" :key="u.id" class="unsync-item">
        <p class="unsync-sum">{{ u.summary }}</p>
        <pre class="unsync-text">{{ u.text || '（无文字）' }}</pre>
        <div class="unsync-actions">
          <button type="button" @click="copyUnsynced(u)">复制原文</button>
          <button type="button" class="ghost" @click="dismissUnsynced(u.id)">删除本地副本</button>
        </div>
      </div>
    </div>
    <SingleEditor :rows="rows" :me="me" :cursors="cursors" @error="showError" />
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
.resume {
  margin-bottom: 16px;
  padding: 10px 12px;
  border: 1px solid #ddd6c8;
  border-radius: 4px;
  background: #fff8e8;
}
.resume p {
  margin: 0 0 8px;
  font-size: 0.95rem;
}
.resume button,
.unsync-btn,
.unsync-actions button {
  border: none;
  border-radius: 4px;
  padding: 6px 10px;
  background: #3d7eff;
  color: #fff;
  cursor: pointer;
}
.unsync-btn {
  margin-left: 12px;
  background: #c47b00;
  font-size: 0.85rem;
}
.offline-hint {
  margin-left: 12px;
  color: #c47b00;
  font-size: 0.85rem;
}
.unsync-panel {
  padding: 8px 12px;
  background: #fff8e8;
  border-bottom: 1px solid #e6d9b8;
  max-height: 40vh;
  overflow: auto;
}
.unsync-item {
  margin-bottom: 10px;
}
.unsync-sum {
  margin: 0 0 4px;
  font-size: 0.9rem;
}
.unsync-text {
  margin: 0 0 6px;
  padding: 8px;
  background: #fff;
  border: 1px solid #eee;
  border-radius: 4px;
  white-space: pre-wrap;
  word-break: break-word;
  font: inherit;
  color: #1a1a1a;
}
.unsync-actions {
  display: flex;
  gap: 8px;
}
.unsync-actions .ghost {
  background: transparent;
  color: #666;
  border: 1px solid #ccc;
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
</style>
