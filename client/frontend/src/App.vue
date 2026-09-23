<script lang="ts" setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  AnswerFollow, CreateArticle, DismissUnsynced, GetCachedArticle, GetSaveWarning,
  GetServer, Join, ListArticles, ListUnsynced, PersonID, SetServer,
} from '../wailsjs/go/main/App'
import { main } from '../wailsjs/go/models'
import { ClipboardSetText, EventsOn } from '../wailsjs/runtime/runtime'
import SingleEditor from './components/SingleEditor.vue'
import ModalDialog from './components/ModalDialog.vue'
import { formatUserError } from './errors'
import { buildVisualRows } from './layout'
import type { Cursor, FollowAsk, FollowResult, Snapshot } from './types'

const me = ref('')
const name = ref('')
const title = ref('')
const articles = ref<main.ArticleBrief[]>([])
const cached = ref<main.CachedArticle | null>(null)
const unsynced = ref<main.UnsyncedItem[]>([])
const showUnsynced = ref(false)
const offline = ref(false)
const joined = ref(false)
const loading = ref(false)
const busy = ref(false)
const bridgeMissing = ref(false)
const errorText = ref('')
const notice = ref('')
const saveWarning = ref('')
const snap = ref<Snapshot | null>(null)
const cursors = ref<Cursor[]>([])
const activeID = ref('')
const serverAddress = ref('')
const serverDraft = ref('')
const showServer = ref(false)
const serverError = ref('')
const asks = ref<FollowAsk[]>([])
const showAsk = ref(false)
const discard = ref<main.UnsyncedItem | null>(null)
const editor = ref<InstanceType<typeof SingleEditor> | null>(null)
const listHeading = ref<HTMLElement | null>(null)
const rows = computed(() => buildVisualRows(snap.value, me.value))
const ask = computed(() => asks.value[0])
const unsubscribers: (() => void)[] = []
let listRequest = 0

function showError(e: unknown) { errorText.value = formatUserError(e) }

async function loadArticles() {
  const request = ++listRequest
  loading.value = true
  try {
    const result = await ListArticles()
    if (request === listRequest) articles.value = result || []
  } catch (e) {
    if (request === listRequest) showError(e)
  } finally {
    if (request === listRequest) loading.value = false
  }
}
async function loadCached() {
  try {
    cached.value = await GetCachedArticle()
    if (cached.value?.name && !name.value) name.value = cached.value.name
  } catch (e) { showError(e) }
}
async function loadUnsynced() {
  try { unsynced.value = (await ListUnsynced()) || [] }
  catch (e) { showError(e) }
}
async function enter(id: string) {
  if (busy.value) return
  busy.value = true
  activeID.value = id
  errorText.value = ''
  notice.value = ''
  snap.value = null
  cursors.value = []
  asks.value = []
  showAsk.value = false
  try {
    await Join(id, name.value.trim() || '未命名')
    joined.value = true
    cached.value = null
    await loadUnsynced()
  } catch (e) {
    activeID.value = ''
    showError(e)
  } finally { busy.value = false }
}
async function onCreate() {
  if (busy.value) return
  busy.value = true
  errorText.value = ''
  let id: string
  try { id = await CreateArticle(title.value) }
  catch (e) { showError(e); return }
  finally { busy.value = false }
  title.value = ''
  await enter(id)
}
async function backToList() {
  if (busy.value) return
  busy.value = true
  try {
    await editor.value?.prepareLeave()
    joined.value = false
    activeID.value = ''
    showUnsynced.value = false
    showAsk.value = false
    asks.value = []
    errorText.value = ''
    notice.value = ''
    await Promise.all([loadArticles(), loadCached()])
  } catch (e) { showError(e) }
  finally { busy.value = false }
}
async function afterPageEnter() {
  await nextTick()
  if (joined.value) editor.value?.focus()
  else listHeading.value?.focus()
}
function editServer() {
  serverDraft.value = serverAddress.value
  serverError.value = ''
  showServer.value = true
}
async function saveServer() {
  if (busy.value) return
  busy.value = true
  serverError.value = ''
  try {
    await SetServer(serverDraft.value)
    serverAddress.value = await GetServer()
    ++listRequest
    articles.value = []
    cached.value = null
    snap.value = null
    activeID.value = ''
    errorText.value = ''
    showServer.value = false
    await Promise.all([loadArticles(), loadCached()])
  } catch (e) { serverError.value = formatUserError(e) }
  finally { busy.value = false }
}
async function copyUnsynced(item: main.UnsyncedItem) {
  try {
    const runtime = (window as unknown as { runtime?: { ClipboardSetText?: unknown } }).runtime
    if (typeof runtime?.ClipboardSetText === 'function') {
      if (!(await ClipboardSetText(item.text || ''))) throw new Error('复制失败，请选中原文手动复制')
    } else {
      await navigator.clipboard.writeText(item.text || '')
    }
    notice.value = '原文已复制'
  } catch (e) { showError(e) }
}
async function dismissUnsynced() {
  if (!discard.value || busy.value) return
  busy.value = true
  try {
    await DismissUnsynced(discard.value.id)
    discard.value = null
    await loadUnsynced()
  } catch (e) { showError(e) }
  finally { busy.value = false }
}
async function answerFollow(accept: boolean) {
  const current = ask.value
  if (!current || busy.value) return
  busy.value = true
  try {
    await AnswerFollow(current.fromId, current.disputeId, accept)
    asks.value = asks.value.filter((a) => a !== current)
    showAsk.value = asks.value.length > 0
    notice.value = accept ? '已同意追随' : '已拒绝追随'
  } catch (e) { showError(e) }
  finally { busy.value = false }
}

onMounted(async () => {
  if (import.meta.env.DEV && new URLSearchParams(location.search).get('mock') === '1') {
    const m = (await import('./devMock')).loadDevMock()
    me.value = m.me
    joined.value = true
    activeID.value = m.snap.article.id
    snap.value = m.snap
    cursors.value = m.cursors
    return
  }
  // 先订阅，再读取状态，避免漏掉初始化期间的离线或保存事件。
  if (!(window as unknown as { go?: { main?: { App?: unknown } } }).go?.main?.App) {
    bridgeMissing.value = true
    return
  }
  unsubscribers.push(
    EventsOn('snapshot', (s: Snapshot) => {
      if (s.article.id !== activeID.value) return
      snap.value = s
      cursors.value = s.cursors || []
      asks.value = asks.value.filter((a) => s.disputes.some((d) => d.id === a.disputeId &&
        d.pendingConfirm?.some((p) => p.from === a.fromId && p.to === me.value)))
      if (!asks.value.length) showAsk.value = false
    }),
    EventsOn('cursors', (cs: Cursor[]) => { if (activeID.value) cursors.value = cs || [] }),
    EventsOn('unsynced', (list: main.UnsyncedItem[]) => { unsynced.value = list || [] }),
    EventsOn('offline', (on: boolean) => { offline.value = !!on }),
    EventsOn('saveWarning', (warning: string) => { saveWarning.value = warning || '' }),
    EventsOn('followAsk', (incoming: FollowAsk) => {
      if (!activeID.value || !snap.value?.disputes.some((d) => d.id === incoming.disputeId)) return
      if (!asks.value.some((a) => a.fromId === incoming.fromId && a.disputeId === incoming.disputeId)) asks.value.push(incoming)
    }),
    EventsOn('followResult', (result: FollowResult) => {
      notice.value = ({ pending: '追随请求已送出，等待对方确认', applied: '追随已生效',
        denied: '对方未接受追随，你的主张仍保留', lost: '对方的追随先发出，已同步结果，请重新选择' } as Record<string, string>)[result.status] || ''
    }),
    EventsOn('error', showError),
  )
  try {
    me.value = await PersonID()
    serverAddress.value = await GetServer()
    saveWarning.value = await GetSaveWarning()
    await Promise.all([loadArticles(), loadCached(), loadUnsynced()])
  } catch (e) { showError(e) }
})
onBeforeUnmount(() => { ++listRequest; unsubscribers.forEach((off) => off()) })
</script>

<template>
  <Transition name="page" mode="out-in" @after-enter="afterPageEnter">
    <main v-if="bridgeMissing" key="preview" class="lobby">
      <h1>协同编辑器</h1><p class="muted">请从桌面客户端打开编辑器，以连接文档并保存修改。</p>
    </main>
    <main v-else-if="!joined" key="lobby" class="lobby">
      <header class="lobby-heading">
        <h1 ref="listHeading" tabindex="-1">协同编辑器</h1>
        <button class="quiet" :disabled="busy" @click="editServer">连接设置</button>
      </header>
      <label class="field">名字<input v-model="name" placeholder="未命名" autocomplete="nickname" :disabled="busy" /></label>
      <section v-if="cached" class="resume">
        <p>继续「{{ cached.title || '未命名文档' }}」</p>
        <p v-if="cached.pendingCount || cached.unsyncedCount" class="muted">{{ cached.pendingCount || 0 }} 处待发送，{{ cached.unsyncedCount || 0 }} 处未能同步</p>
        <button :disabled="busy" @click="enter(cached.id)">继续编辑</button>
      </section>
      <form class="create" @submit.prevent="onCreate">
        <input v-model="title" aria-label="新文档标题" placeholder="新文档标题" :disabled="busy" />
        <button :disabled="busy">{{ busy ? '请稍候…' : '新建' }}</button>
      </form>
      <div class="list-heading"><h2>已有文档</h2><button class="quiet" :disabled="loading || busy" @click="errorText = ''; loadArticles()">刷新</button></div>
      <p v-if="loading" class="muted" role="status">正在加载文档…</p>
      <p v-else-if="!articles.length && !errorText" class="muted">还没有文档，可以从新建开始。</p>
      <ul class="article-list" :aria-busy="loading">
        <li v-for="a in articles" :key="a.id"><button class="article-link" :disabled="busy" @click="enter(a.id)">{{ a.title || '未命名文档' }}<span aria-hidden="true">↗</span></button></li>
      </ul>
      <p v-if="errorText" class="error" role="alert">{{ errorText }}</p>
      <p v-if="saveWarning" class="warning" role="status">{{ saveWarning }}</p>
    </main>
    <main v-else key="editor" class="editor-shell">
      <header class="bar">
        <button class="quiet" :disabled="busy" @click="backToList">返回文档</button>
        <strong class="document-title">{{ snap?.article.title || '正在打开文档…' }}</strong>
        <span v-if="offline" class="muted" role="status">当前离线，连接后自动发送</span>
        <button v-if="asks.length" @click="showAsk = true">{{ asks.length }} 个追随请求</button>
        <button v-if="unsynced.length" class="quiet" :aria-expanded="showUnsynced" @click="showUnsynced = !showUnsynced">{{ unsynced.length }} 处未能同步</button>
      </header>
      <p v-if="saveWarning" class="banner warning" role="status">{{ saveWarning }}</p>
      <p v-if="errorText" class="banner error" role="alert">{{ errorText }}<button class="quiet" aria-label="关闭提示" @click="errorText = ''">×</button></p>
      <p v-if="notice" class="banner muted" role="status">{{ notice }}<button class="quiet" aria-label="关闭通知" @click="notice = ''">×</button></p>
      <Transition name="panel">
        <section v-if="showUnsynced && unsynced.length" class="unsynced-panel" aria-label="未同步修改">
          <article v-for="u in unsynced" :key="u.id">
            <p>{{ u.summary }}</p><pre>{{ u.text || '（无文字）' }}</pre>
            <div class="actions"><button @click="copyUnsynced(u)">复制原文</button><button class="quiet" @click="discard = u">删除本地副本</button></div>
          </article>
        </section>
      </Transition>
      <SingleEditor v-if="snap" ref="editor" :key="activeID" :rows="rows" :me="me" :cursors="cursors" :initial-line-id="snap.yourLine" @error="showError" />
      <p v-else class="loading" role="status">正在打开文档…</p>
    </main>
  </Transition>
  <ModalDialog v-model="showServer" title="连接设置" :busy="busy">
    <form @submit.prevent="saveServer">
      <label class="field">服务器地址<input v-model="serverDraft" type="url" required autofocus placeholder="http://127.0.0.1:8787" :disabled="busy" /></label>
      <p v-if="serverError" class="error" role="alert">{{ serverError }}</p>
      <div class="actions"><button :disabled="busy">{{ busy ? '正在保存…' : '保存' }}</button><button type="button" class="quiet" :disabled="busy" @click="showServer = false">取消</button></div>
    </form>
  </ModalDialog>
  <ModalDialog v-model="showAsk" title="追随请求" :busy="busy">
    <template v-if="ask"><p>{{ ask.fromName || '有人' }} 希望接受你的主张，是否同意？</p><div class="actions"><button :disabled="busy" @click="answerFollow(true)">同意</button><button class="quiet" :disabled="busy" @click="answerFollow(false)">拒绝</button><button class="quiet" :disabled="busy" @click="showAsk = false">稍后</button></div></template>
  </ModalDialog>
  <ModalDialog :model-value="!!discard" title="删除本地副本" :busy="busy" @update:model-value="discard = null">
    <p>删除后将无法找回，请先确认已复制需要的内容。</p><div class="actions"><button class="quiet" :disabled="busy" autofocus @click="discard = null">保留</button><button :disabled="busy" @click="dismissUnsynced">删除副本</button></div>
  </ModalDialog>
</template>

<style scoped>
.lobby { max-width: 620px; margin: 0 auto; padding: 48px 24px; }
.lobby-heading, .list-heading { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
h1 { font-size: 1.6rem; margin: 0; letter-spacing: -.03em; }
h2 { font-size: 1rem; margin: 0; }
.lobby-heading { margin-bottom: 32px; }
.resume { margin: 20px 0; padding: 18px; background: #eeebe4; border-radius: 8px; }
.resume p:first-child { margin-top: 0; }
.create { display: flex; gap: 10px; margin: 24px 0 32px; }
.create input { flex: 1; min-width: 0; }
.article-list { list-style: none; padding: 0; margin: 12px 0; }
.article-link { display: flex; justify-content: space-between; gap: 16px; width: 100%; padding: 14px 0; border: 0; border-bottom: 1px solid #e4dfd5; border-radius: 0; text-align: left; background: transparent; color: inherit; overflow-wrap: anywhere; }
.article-link:hover { background: #eeebe4; }
.article-link span { color: #888; }
.editor-shell { display: flex; flex-direction: column; height: 100%; }
.bar { display: flex; align-items: center; flex-wrap: wrap; gap: 12px; padding: 10px 16px; background: #ece8df; border-bottom: 1px solid #ddd6c8; }
.document-title { flex: 1; min-width: 120px; overflow-wrap: anywhere; font-size: .95rem; }
.banner { margin: 0; padding: 8px 16px; display: flex; gap: 12px; align-items: center; justify-content: space-between; background: #f0ece3; font-size: .9rem; }
.unsynced-panel { padding: 16px 24px; background: #eeebe4; max-height: 40vh; overflow: auto; }
.unsynced-panel article + article { margin-top: 24px; padding-top: 12px; border-top: 1px solid #d8d2c7; }
pre { padding: 12px; background: #faf8f3; white-space: pre-wrap; overflow-wrap: anywhere; font: inherit; }
.loading { padding: 24px; }
@media (max-width: 540px) { .lobby { padding: 24px 16px; } .bar { gap: 8px; } .document-title { order: -1; flex-basis: 100%; } }
</style>
