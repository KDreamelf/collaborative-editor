<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { formatUserError, httpFailureMessage, newLogId } from './errors'

const STORAGE_KEY = 'admin-server-base'
const DEFAULT_BASE = 'http://127.0.0.1:8787'

type ArticleSummary = { id: string; title: string }

type Line = {
  id: string
  prev: string
  next: string
  content: string
}

type Dispute = {
  id: string
  realLine: string
  action: string
  person: string
  content: string[]
  followers: string[]
}

type ArticleDetail = {
  article: { id: string; title: string }
  lines: Line[]
  disputes: Dispute[]
  suspended: string[]
}

const baseUrl = ref(DEFAULT_BASE)
const error = ref('')
const loading = ref(false)
const articles = ref<ArticleSummary[]>([])
const selectedId = ref<string | null>(null)
const detail = ref<ArticleDetail | null>(null)
const pageHeading = ref<HTMLElement | null>(null)
let currentBase = DEFAULT_BASE
let request = 0
let controller: AbortController | null = null

const suspendedSet = computed(() => new Set(detail.value?.suspended ?? []))

const disputesByLine = computed(() => {
  const map = new Map<string, Dispute[]>()
  for (const d of detail.value?.disputes ?? []) {
    const list = map.get(d.realLine)
    if (list) list.push(d)
    else map.set(d.realLine, [d])
  }
  return map
})

function normalizeBase(raw: string): string {
  return raw.trim().replace(/\/+$/, '')
}

function loadBase() {
  try { baseUrl.value = localStorage.getItem(STORAGE_KEY) || DEFAULT_BASE }
  catch { baseUrl.value = DEFAULT_BASE }
}

function persistBase(): boolean {
  const next = normalizeBase(baseUrl.value) || DEFAULT_BASE
  try {
    const url = new URL(next)
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash || url.pathname !== '/') throw new Error()
  } catch {
    error.value = '请输入有效的服务器地址，例如 http://127.0.0.1:8787'
    return false
  }
  if (next !== currentBase) {
    selectedId.value = null
    detail.value = null
    articles.value = []
  }
  currentBase = next
  baseUrl.value = next
  try { localStorage.setItem(STORAGE_KEY, next) } catch { /* 禁用本地存储不影响本次连接 */ }
  return true
}

async function apiGet<T>(path: string, signal: AbortSignal): Promise<T> {
  const url = `${currentBase}${path}`
  let res: Response
  try {
    res = await fetch(url, { signal })
  } catch (e) {
    if (signal.aborted) throw e
    const id = newLogId()
    console.error(`[${id}]`, e)
    throw new Error('无法连接服务器')
  }
  const text = await res.text()
  if (!res.ok) {
    throw new Error(httpFailureMessage(res.status, text))
  }
  try {
    return JSON.parse(text) as T
  } catch (e) {
    const id = newLogId()
    console.error(`[${id}]`, text, e)
    throw new Error(`响应不是 JSON，日志编号 ${id}`)
  }
}

async function refresh() {
  controller?.abort()
  const thisRequest = ++request
  if (!persistBase()) { loading.value = false; return }
  const abort = new AbortController()
  controller = abort
  const timeout = window.setTimeout(() => {
    abort.abort()
    if (thisRequest === request) error.value = '请求超时，请重试'
  }, 10_000)
  const selected = selectedId.value
  error.value = ''
  loading.value = true
  try {
    const [list, article] = await Promise.all([
      apiGet<ArticleSummary[]>('/api/articles', abort.signal),
      selected ? apiGet<ArticleDetail>(`/api/articles/${selected}`, abort.signal) : Promise.resolve(null),
    ])
    if (thisRequest !== request) return
    articles.value = list || []
    detail.value = article
  } catch (e) {
    if (thisRequest === request && !abort.signal.aborted) error.value = formatUserError(e)
  } finally {
    window.clearTimeout(timeout)
    if (thisRequest === request) loading.value = false
  }
}

async function openArticle(id: string) {
  detail.value = null
  selectedId.value = id
  await refresh()
}

async function backToList() {
  selectedId.value = null
  detail.value = null
  await refresh()
}

function lineDisputes(lineId: string): Dispute[] {
  return disputesByLine.value.get(lineId) ?? []
}

function disputeContent(d: Dispute): string {
  return (d.content ?? []).join('\n')
}

async function focusHeading() { await nextTick(); pageHeading.value?.focus() }
onBeforeUnmount(() => { ++request; controller?.abort() })

onMounted(async () => {
  loadBase()
  await refresh()
})
</script>

<template>
  <header class="topbar">
    <label>
      服务器
      <input v-model="baseUrl" type="url" spellcheck="false" @keydown.enter="refresh" />
    </label>
    <button type="button" :disabled="loading" @click="refresh">
      {{ loading ? '刷新中…' : '刷新' }}
    </button>
  </header>

  <p v-if="error" class="error" role="alert">{{ error }}</p>

  <Transition name="view" mode="out-in" @after-enter="focusHeading">
  <section v-if="!selectedId" key="list" :aria-busy="loading">
    <h1 ref="pageHeading" class="list-title" tabindex="-1">文档查看</h1>
    <p v-if="loading" class="empty-state" role="status">正在加载文档…</p>
    <p v-if="!loading && articles.length === 0 && !error" class="empty-state">暂无文章</p>
    <ul class="list">
      <li v-for="a in articles" :key="a.id">
        <button type="button" @click="openArticle(a.id)">
          {{ a.title || '（无标题）' }}
        </button>
      </li>
    </ul>
  </section>

  <section v-else :key="selectedId" :aria-busy="loading">
    <div class="toolbar">
      <button type="button" @click="backToList">返回列表</button>
      <h1 ref="pageHeading" tabindex="-1">{{ detail?.article.title || (loading ? '正在打开文档…' : '文档暂不可用') }}</h1>
    </div>

    <div v-if="detail" class="article">
      <div v-for="(line, i) in detail.lines" :key="line.id" class="line-block">
        <div class="line">
          <span class="num">{{ i + 1 }}</span>
          <span :class="{ empty: line.content === '' }">{{
            line.content === '' ? '（空行）' : line.content
          }}</span>
        </div>

        <div v-if="lineDisputes(line.id).length" class="disputes">
          <div
            v-for="(d, candidate) in lineDisputes(line.id)"
            :key="d.id"
            class="dispute"
            :class="{ suspended: suspendedSet.has(d.id) }"
          >
            <div class="meta">
              主张 {{ candidate + 1 }} · {{ d.action }}
              <template v-if="d.followers?.length"> · {{ d.followers.length }} 人追随</template>
              <template v-if="suspendedSet.has(d.id)"> · 已挂起</template>
            </div>
            <div class="content">{{ disputeContent(d) }}</div>
          </div>
        </div>
      </div>
      <p v-if="detail.lines.length === 0" class="empty-state">这篇文档还没有内容</p>
    </div>
  </section>
  </Transition>
</template>
