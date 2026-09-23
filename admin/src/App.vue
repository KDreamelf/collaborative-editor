<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
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
  const saved = localStorage.getItem(STORAGE_KEY)
  baseUrl.value = saved && saved.trim() ? saved : DEFAULT_BASE
}

function persistBase() {
  const next = normalizeBase(baseUrl.value) || DEFAULT_BASE
  baseUrl.value = next
  localStorage.setItem(STORAGE_KEY, next)
}

async function apiGet<T>(path: string): Promise<T> {
  const url = `${normalizeBase(baseUrl.value)}${path}`
  let res: Response
  try {
    res = await fetch(url)
  } catch (e) {
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
  persistBase()
  error.value = ''
  loading.value = true
  try {
    if (selectedId.value) {
      detail.value = await apiGet<ArticleDetail>(`/api/articles/${selectedId.value}`)
      articles.value = await apiGet<ArticleSummary[]>('/api/articles')
    } else {
      articles.value = await apiGet<ArticleSummary[]>('/api/articles')
      detail.value = null
    }
  } catch (e) {
    error.value = formatUserError(e)
  } finally {
    loading.value = false
  }
}

async function openArticle(id: string) {
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

watch(baseUrl, (v) => {
  localStorage.setItem(STORAGE_KEY, normalizeBase(v) || DEFAULT_BASE)
})

onMounted(async () => {
  loadBase()
  await refresh()
})
</script>

<template>
  <header class="topbar">
    <label>
      服务器
      <input v-model="baseUrl" type="url" spellcheck="false" @change="persistBase" />
    </label>
    <button type="button" :disabled="loading" @click="refresh">
      {{ loading ? '刷新中…' : '刷新' }}
    </button>
  </header>

  <p v-if="error" class="error">{{ error }}</p>

  <template v-if="!selectedId">
    <p v-if="!loading && articles.length === 0 && !error" class="empty-state">暂无文章</p>
    <ul class="list">
      <li v-for="a in articles" :key="a.id">
        <button type="button" @click="openArticle(a.id)">
          {{ a.title || '（无标题）' }}
        </button>
      </li>
    </ul>
  </template>

  <template v-else>
    <div class="toolbar">
      <button type="button" @click="backToList">返回列表</button>
      <h1>{{ detail?.article.title || '（无标题）' }}</h1>
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
            v-for="d in lineDisputes(line.id)"
            :key="d.id"
            class="dispute"
            :class="{ suspended: suspendedSet.has(d.id) }"
          >
            <div class="meta">
              {{ d.person }} · {{ d.action }} · 追随者 {{ (d.followers ?? []).length }}
              <template v-if="suspendedSet.has(d.id)"> · 已挂起</template>
            </div>
            <div class="content">{{ disputeContent(d) }}</div>
          </div>
        </div>
      </div>
      <p v-if="detail.lines.length === 0" class="empty-state">无正式行</p>
    </div>
  </template>
</template>
