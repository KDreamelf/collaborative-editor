import {
  ACTION_DELETE,
  ACTION_EDIT,
  ACTION_INSERT,
  ACTION_INSERT_BEFORE,
  DELETE_LABEL,
  Dispute,
  Snapshot,
  VisualRow,
} from './types'

function isBodyAction(action: string): boolean {
  return action === ACTION_EDIT || action === ACTION_DELETE
}

/** 本机第一次看见的争议者顺序，只增不改。 */
const seenOrder = new Map<string, string[]>()

function orderKey(lineId: string, action: string): string {
  return lineId + '\0' + action
}

function remember(lineId: string, action: string, personId: string) {
  const k = orderKey(lineId, action)
  let list = seenOrder.get(k)
  if (!list) {
    list = []
    seenOrder.set(k, list)
  }
  if (!list.includes(personId)) {
    list.push(personId)
  }
}

function sortBySeen(lineId: string, action: string, people: string[]): string[] {
  const k = orderKey(lineId, action)
  const list = seenOrder.get(k) || []
  return [...people].sort((a, b) => {
    const ia = list.indexOf(a)
    const ib = list.indexOf(b)
    return (ia < 0 ? 9999 : ia) - (ib < 0 ? 9999 : ib)
  })
}

function followingSet(group: Dispute[]): Set<string> {
  const s = new Set<string>()
  for (const d of group) {
    for (const f of d.followers || []) {
      s.add(f)
    }
  }
  return s
}

function visibleClaims(group: Dispute[], me: string): Dispute[] {
  const followed = followingSet(group)
  return group.filter((d) => d.person === me || !followed.has(d.person))
}

/** 到达序交替上/下/上…，自己居中。 */
function stackAroundSelf(others: Dispute[], self: Dispute): Dispute[] {
  const above: Dispute[] = []
  const below: Dispute[] = []
  others.forEach((d, i) => {
    if (i % 2 === 0) above.unshift(d)
    else below.push(d)
  })
  return [...above, self, ...below]
}

function arrange(lineId: string, action: string, group: Dispute[], me: string): Dispute[] {
  for (const d of group) {
    remember(lineId, action, d.person)
  }
  const vis = visibleClaims(group, me)
  const self = vis.find((d) => d.person === me)
  const others = sortBySeen(
    lineId,
    action,
    vis.filter((d) => d.person !== me).map((d) => d.person),
  )
    .map((pid) => vis.find((d) => d.person === pid)!)
    .filter(Boolean)

  if (!self && others.length === 0) {
    return []
  }
  if (!self) {
    return others
  }
  return stackAroundSelf(others, self)
}

function dotIds(d: Dispute): string[] {
  const out = [d.person]
  for (const f of d.followers || []) {
    if (f && !out.includes(f)) out.push(f)
  }
  return out
}

function pushBodyRows(
  out: VisualRow[],
  lineId: string,
  lineIndex: number,
  lineNo: number,
  ordered: Dispute[],
  me: string,
  suspended: Set<string>,
  zebra: number,
  spanBaseIDs?: string[],
) {
  const selfIdx = ordered.findIndex((d) => d.person === me)
  const lineNoOwner = selfIdx >= 0 ? selfIdx : 0
  const span =
    spanBaseIDs && spanBaseIDs.length >= 2 ? spanBaseIDs : undefined

  ordered.forEach((d, claimIdx) => {
    const isDelete = d.action === ACTION_DELETE
    const parts = isDelete ? [DELETE_LABEL] : d.content && d.content.length > 0 ? d.content : ['']
    const phantom = !d.id
    const isSelf = d.person === me
    const rowSpan =
      span ||
      (d.baseIDs && d.baseIDs.length >= 2 ? d.baseIDs : undefined)
    parts.forEach((text, partIndex) => {
      const head = partIndex === 0
      out.push({
        key: phantom ? `phantom:${lineId}:${partIndex}` : `${d.id}:${partIndex}`,
        lineId,
        lineIndex,
        action: d.action || ACTION_EDIT,
        disputeId: d.id,
        followId: phantom || isSelf ? '' : d.id,
        personId: d.person,
        content: text,
        partIndex,
        partCount: parts.length,
        isSelf,
        isContext: false,
        showLineNo: claimIdx === lineNoOwner && head,
        lineNo,
        followerCount: (d.followers || []).length,
        gutterDots: !isSelf && head && !phantom ? dotIds(d) : [],
        suspended: !phantom && suspended.has(d.id),
        editable: isSelf && !isDelete,
        zebra,
        phantom,
        blockStart: head,
        separatorBefore: claimIdx > 0 && head,
        spanBaseIDs: rowSpan,
      })
    })
  })
}

/**
 * 插入争议：每份候选 = 锚点正文（共同上下文）+ 该人插入段。
 * 无本人插入时仍占中位自我块（仅锚点行），行号只在自我块首行。
 */
function pushInsertBlocks(
  out: VisualRow[],
  lineId: string,
  lineIndex: number,
  lineNo: number,
  lineContent: string,
  inserts: Dispute[],
  me: string,
  suspended: Set<string>,
  zebra: number,
  action = ACTION_INSERT,
) {
  for (const d of inserts) {
    remember(lineId, action, d.person)
  }
  const vis = visibleClaims(inserts, me)
  let self = vis.find((d) => d.person === me)
  const others = sortBySeen(
    lineId,
    action,
    vis.filter((d) => d.person !== me).map((d) => d.person),
  )
    .map((pid) => vis.find((d) => d.person === pid)!)
    .filter(Boolean)

  if (!self) {
    self = {
      id: '',
      realLine: lineId,
      action,
      person: me,
      content: [],
      followers: [],
    }
  }
  const ordered = stackAroundSelf(others, self)

  ordered.forEach((d, claimIdx) => {
    const blockStart = out.length
    const isSelf = d.person === me
    const phantom = !d.id
    const insertParts = phantom ? [] : d.content && d.content.length > 0 ? d.content : ['']

    // 共同首行：正式锚点正文
    out.push({
      key: `insert-ctx:${action}:${lineId}:${d.person}`,
      lineId,
      lineIndex,
      action: ACTION_EDIT,
      // 他人上下文用假 id，避免与插入主张 parts 按 disputeId 混组
      disputeId: isSelf ? '' : `ctx-${d.id}`,
      followId: isSelf || phantom ? '' : d.id,
      personId: d.person,
      content: lineContent,
      partIndex: 0,
      partCount: 1,
      isSelf,
      isContext: true,
      contextAction: action,
      showLineNo: isSelf,
      lineNo,
      followerCount: (d.followers || []).length,
      gutterDots: isSelf || phantom ? [] : dotIds(d),
      suspended: !phantom && !!d.id && suspended.has(d.id),
      editable: isSelf,
      zebra,
      phantom: phantom && isSelf,
      blockStart: true,
      separatorBefore: claimIdx > 0,
    })

    insertParts.forEach((text, partIndex) => {
      out.push({
        key: `${d.id}:${partIndex}`,
        lineId,
        lineIndex,
        action,
        disputeId: d.id,
        followId: isSelf ? '' : d.id,
        personId: d.person,
        content: text,
        partIndex,
        partCount: insertParts.length,
        isSelf,
        isContext: false,
        showLineNo: false,
        lineNo,
        followerCount: (d.followers || []).length,
        gutterDots: [],
        suspended: suspended.has(d.id),
        editable: isSelf,
        zebra,
        phantom: false,
        blockStart: false,
        separatorBefore: false,
      })
    })
    if (action === ACTION_INSERT_BEFORE && insertParts.length) {
      const block = out.splice(blockStart)
      const context = block.shift()!
      block.push(context)
      block.forEach((row, index) => {
        row.blockStart = index === 0
        row.separatorBefore = index === 0 && claimIdx > 0
        row.showLineNo = index === 0 && isSelf
        row.gutterDots = index === 0 && !isSelf ? dotIds(d) : []
      })
      out.push(...block)
    }
  })
}

function spanCoveredIDs(disputes: Dispute[]): Set<string> {
  const covered = new Set<string>()
  for (const d of disputes) {
    const ids = d.baseIDs
    if (!ids || ids.length < 2) continue
    for (let i = 1; i < ids.length; i++) covered.add(ids[i])
  }
  return covered
}

function lineContentByID(snap: Snapshot, id: string): string {
  const ln = snap.lines.find((l) => l.id === id)
  return ln?.content || ''
}

export function buildVisualRows(snap: Snapshot | null, me: string): VisualRow[] {
  if (!snap || !snap.lines) {
    return []
  }
  const suspended = new Set(snap.suspended || [])
  const disputes = snap.disputes || []
  const covered = spanCoveredIDs(disputes)
  const out: VisualRow[] = []

  snap.lines.forEach((line, lineIndex) => {
    // 被整段跨度覆盖的后续正式行不独立渲染
    if (covered.has(line.id)) return

    const lineNo = lineIndex + 1
    const zebra = lineIndex % 2
    let body = disputes.filter((d) => d.realLine === line.id && isBodyAction(d.action))
    const inserts = disputes.filter((d) => d.realLine === line.id && d.action === ACTION_INSERT)
    const before = disputes.filter((d) => d.realLine === line.id && d.action === ACTION_INSERT_BEFORE)
    // 本人挂起且等于正文的单份编辑已包含在插入上下文里，不再重复画一遍。
    if ((inserts.length || before.length) && body.length === 1 && body[0].person === me &&
      body[0].action === ACTION_EDIT && body[0].content.length === 1 && body[0].content[0] === line.content) body = []
    const spanBaseIDs = body.find((d) => d.baseIDs && d.baseIDs.length >= 2)?.baseIDs

    if (before.length > 0) {
      pushInsertBlocks(out, line.id, lineIndex, lineNo, line.content || '', before, me, suspended, zebra, ACTION_INSERT_BEFORE)
    }
    if (body.length > 0) {
      let ordered = arrange(line.id, ACTION_EDIT, body, me)
      if (!ordered.some((d) => d.person === me)) {
        const phantomContent =
          spanBaseIDs && spanBaseIDs.length >= 2
            ? spanBaseIDs.map((id) => lineContentByID(snap, id))
            : [line.content || '']
        const phantom: Dispute = {
          id: '',
          realLine: line.id,
          action: ACTION_EDIT,
          person: me,
          content: phantomContent,
          baseIDs: spanBaseIDs,
          followers: [],
        }
        const others = ordered
        ordered = stackAroundSelf(others, phantom)
      }
      pushBodyRows(
        out,
        line.id,
        lineIndex,
        lineNo,
        ordered,
        me,
        suspended,
        zebra,
        spanBaseIDs,
      )
    } else if (inserts.length === 0 && before.length === 0) {
      out.push({
        key: `line:${line.id}`,
        lineId: line.id,
        lineIndex,
        action: ACTION_EDIT,
        disputeId: '',
        followId: '',
        personId: me,
        content: line.content || '',
        partIndex: 0,
        partCount: 1,
        isSelf: true,
        isContext: false,
        showLineNo: true,
        lineNo,
        followerCount: 0,
        gutterDots: [],
        suspended: false,
        editable: true,
        zebra,
        phantom: false,
        blockStart: false,
        separatorBefore: false,
      })
    }
    // inserts>0 且无 body：正式行并入各插入候选块，不单独渲染

    if (inserts.length > 0) {
      pushInsertBlocks(
        out,
        line.id,
        lineIndex,
        lineNo,
        line.content || '',
        inserts,
        me,
        suspended,
        zebra,
      )
    }
  })

  return out
}
