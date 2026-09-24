import type { Cursor, Dispute, Snapshot } from './types'

function installMockBindings() {
  ;(window as unknown as { __submitLog?: unknown[] }).__submitLog = []
  const log = (window as unknown as { __submitLog: unknown[] }).__submitLog
  const g = window as unknown as {
    go?: { main?: { App?: Record<string, (...args: unknown[]) => Promise<unknown>> } }
  }
  g.go = {
    main: {
      App: {
        PersonID: async () => 'me',
        MoveCaret: async () => {},
        MoveCaretRange: async (...args: unknown[]) => { log.push(['MoveCaretRange', ...args]) },
        GetServer: async () => 'http://127.0.0.1:8787',
        SetServer: async () => {},
        GetSaveWarning: async () => '',
        GetCachedArticle: async () => null,
        ListArticles: async () => [],
        ListUnsynced: async () => [],
        Suspend: async () => {},
        SubmitEdit: async (...args: unknown[]) => {
          log.push(['SubmitEdit', ...args])
        },
        SubmitEditClaim: async (...args: unknown[]) => {
          log.push(['SubmitEditClaim', ...args])
        },
        SubmitInsert: async (...args: unknown[]) => {
          log.push(['SubmitInsert', ...args])
        },
        SubmitInsertBefore: async (...args: unknown[]) => {
          log.push(['SubmitInsertBefore', ...args])
        },
        SubmitPaste: async (...args: unknown[]) => {
          log.push(['SubmitPaste', ...args])
        },
        SubmitSpanEdit: async (...args: unknown[]) => {
          log.push(['SubmitSpanEdit', ...args])
        },
        DeleteLine: async (...args: unknown[]) => {
          log.push(['DeleteLine', ...args])
        },
        MergeUp: async (...args: unknown[]) => {
          log.push(['MergeUp', ...args])
        },
        RejectClaim: async (...args: unknown[]) => {
          log.push(['RejectClaim', ...args])
        },
        RequestFollow: async (...args: unknown[]) => {
          log.push(['RequestFollow', ...args])
        },
      },
    },
  }
}

/**
 * DEV-only：
 * ?mock=1 三条正式行插入争议；selfInsert=0 无本人插入。
 * ?mock=1&span=1 整段跨度 fixture（与默认文档结构分离）。
 * selfSpan=0 仅作用于 span fixture：无本人跨度候选。
 */
function mockInsertSnapshot(noSelf: boolean): Snapshot {
  const insertDisputes: Dispute[] = [
    {
      id: 'Di-a',
      realLine: 'L2',
      action: '插在后面',
      person: 'alice',
      content: ['Alice 插入第一行', 'Alice 插入第二行'],
      followers: ['f1'],
    },
    {
      id: 'Di-b',
      realLine: 'L2',
      action: '插在后面',
      person: 'bob',
      content: ['Bob 单行插入'],
      followers: [],
    },
    {
      id: 'Di-c',
      realLine: 'L2',
      action: '插在后面',
      person: 'cara',
      content: ['Cara 插入'],
      followers: ['f2', 'f3'],
    },
  ]
  if (!noSelf) {
    insertDisputes.unshift({
      id: 'Di-me',
      realLine: 'L2',
      action: '插在后面',
      person: 'me',
      content: ['我的插入段甲', '我的插入段乙'],
      followers: [],
    })
  }
  return {
    article: { id: 'a1', title: noSelf ? 'mock·无本人插入' : 'mock·有本人插入' },
    lines: [
      { id: 'L1', content: '第一行正文' },
      { id: 'L2', content: '第二行正文' },
      { id: 'L3', content: '正式第三行只出现一次' },
    ],
    disputes: [
      ...insertDisputes,
      {
        id: 'De-other',
        realLine: 'L3',
        action: '改这行',
        person: 'other',
        content: ['别人的改行主张'],
        followers: [],
      },
    ],
    suspended: [],
    people: [
      { id: 'me', name: '我' },
      { id: 'alice', name: 'Alice' },
      { id: 'bob', name: 'Bob' },
      { id: 'cara', name: 'Cara' },
      { id: 'other', name: '他' },
    ],
    cursors: [
      {
        personId: 'alice',
        name: 'Alice',
        lineId: 'L2',
        disputeId: 'Di-a',
        partIndex: 1,
        offset: 1,
        selEnd: 1,
      },
      {
        personId: 'other',
        name: '他',
        lineId: 'L1',
        offset: 2,
        selEnd: 2,
      },
    ],
  }
}

function mockSpanSnapshot(noSelfSpan: boolean): Snapshot {
  const spanDisputes: Dispute[] = [
    {
      id: 'Ds-other',
      realLine: 'L3',
      action: '改这行',
      person: 'other',
      content: ['他人跨度甲', '他人跨度乙'],
      baseIDs: ['L3', 'L4'],
      followers: [],
    },
  ]
  if (!noSelfSpan) {
    spanDisputes.unshift({
      id: 'Ds-me',
      realLine: 'L3',
      action: '改这行',
      person: 'me',
      content: ['我的跨度甲', '我的跨度乙'],
      baseIDs: ['L3', 'L4'],
      followers: [],
    })
  }
  return {
    article: {
      id: 'a1',
      title: noSelfSpan ? 'mock·跨度·无本人' : 'mock·跨度·有本人',
    },
    lines: [
      { id: 'L1', content: '跨选第一行' },
      { id: 'L2', content: '跨选第二行' },
      { id: 'L3', content: '跨度基准甲' },
      { id: 'L4', content: '跨度基准乙' },
      { id: 'L5', content: '跨度后正式行只出现一次' },
    ],
    disputes: spanDisputes,
    suspended: [],
    people: [
      { id: 'me', name: '我' },
      { id: 'other', name: '他' },
    ],
    cursors: [
      {
        personId: 'other',
        name: '他',
        lineId: 'L1',
        offset: 2,
        selEnd: 2,
      },
    ],
  }
}

/** DEV + ?mock=1：装桩、装 snapshot；调用方写回 refs */
export function loadDevMock(): { me: string; snap: Snapshot; cursors: Cursor[] } {
  const noSelf = /(?:\?|&)selfInsert=0(?:&|$)/.test(location.search)
  const noSelfSpan = /(?:\?|&)selfSpan=0(?:&|$)/.test(location.search)
  const spanFix = /(?:\?|&)span=1(?:&|$)/.test(location.search)
  const snap = spanFix ? mockSpanSnapshot(noSelfSpan) : mockInsertSnapshot(noSelf)
  if (!spanFix && new URLSearchParams(location.search).get('before') === '1') {
    snap.article.title = 'mock·行首插入'
    snap.disputes = snap.disputes.map((d) => d.action === '插在后面' ? { ...d, action: '插在前面' } : d)
  }
  installMockBindings()
  return { me: 'me', snap, cursors: snap.cursors || [] }
}
