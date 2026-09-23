import {
  ACTION_EDIT,
  ACTION_INSERT,
  Dispute,
  Snapshot,
  VisualRow,
} from './types'

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
  const above: Dispute[] = []
  const below: Dispute[] = []
  others.forEach((d, i) => {
    if (i % 2 === 0) {
      above.unshift(d)
    } else {
      below.push(d)
    }
  })
  return [...above, self, ...below]
}

function pushClaimRows(
  out: VisualRow[],
  lineId: string,
  lineIndex: number,
  lineNo: number,
  action: string,
  ordered: Dispute[],
  me: string,
  suspended: Set<string>,
  zebra: number,
) {
  const selfIdx = ordered.findIndex((d) => d.person === me)
  const lineNoOwner = selfIdx >= 0 ? selfIdx : 0

  ordered.forEach((d, claimIdx) => {
    const parts = d.content && d.content.length > 0 ? d.content : ['']
    parts.forEach((text, partIndex) => {
      const isSelf = d.person === me
      out.push({
        key: `${d.id}:${partIndex}`,
        lineId,
        lineIndex,
        action,
        disputeId: d.id,
        personId: d.person,
        content: text,
        partIndex,
        partCount: parts.length,
        isSelf,
        showLineNo: claimIdx === lineNoOwner && partIndex === 0,
        lineNo,
        followerCount: (d.followers || []).length,
        suspended: suspended.has(d.id),
        editable: isSelf,
        zebra,
        phantom: false,
      })
    })
  })
}

export function buildVisualRows(snap: Snapshot | null, me: string): VisualRow[] {
  if (!snap || !snap.lines) {
    return []
  }
  const suspended = new Set(snap.suspended || [])
  const disputes = snap.disputes || []
  const out: VisualRow[] = []

  snap.lines.forEach((line, lineIndex) => {
    const lineNo = lineIndex + 1
    const zebra = lineIndex % 2
    const edits = disputes.filter((d) => d.realLine === line.id && d.action === ACTION_EDIT)
    const inserts = disputes.filter((d) => d.realLine === line.id && d.action === ACTION_INSERT)

    if (edits.length === 0) {
      out.push({
        key: `line:${line.id}`,
        lineId: line.id,
        lineIndex,
        action: ACTION_EDIT,
        disputeId: '',
        personId: me,
        content: line.content || '',
        partIndex: 0,
        partCount: 1,
        isSelf: true,
        showLineNo: true,
        lineNo,
        followerCount: 0,
        suspended: false,
        editable: true,
        zebra,
        phantom: false,
      })
    } else {
      let ordered = arrange(line.id, ACTION_EDIT, edits, me)
      if (!ordered.some((d) => d.person === me)) {
        const phantom: Dispute = {
          id: '',
          realLine: line.id,
          action: ACTION_EDIT,
          person: me,
          content: [line.content || ''],
          followers: [],
        }
        const others = ordered
        const above: Dispute[] = []
        const below: Dispute[] = []
        others.forEach((d, i) => {
          if (i % 2 === 0) above.unshift(d)
          else below.push(d)
        })
        ordered = [...above, phantom, ...below]
      }
      // phantom 行
      const selfIdx = ordered.findIndex((d) => d.person === me)
      const lineNoOwner = selfIdx >= 0 ? selfIdx : 0
      ordered.forEach((d, claimIdx) => {
        const parts = d.content && d.content.length > 0 ? d.content : ['']
        const phantom = !d.id
        parts.forEach((text, partIndex) => {
          const isSelf = d.person === me
          out.push({
            key: phantom ? `phantom:${line.id}:${partIndex}` : `${d.id}:${partIndex}`,
            lineId: line.id,
            lineIndex,
            action: ACTION_EDIT,
            disputeId: d.id,
            personId: d.person,
            content: text,
            partIndex,
            partCount: parts.length,
            isSelf,
            showLineNo: claimIdx === lineNoOwner && partIndex === 0,
            lineNo,
            followerCount: (d.followers || []).length,
            suspended: !phantom && suspended.has(d.id),
            editable: isSelf,
            zebra,
            phantom,
          })
        })
      })
    }

    if (inserts.length > 0) {
      const ordered = arrange(line.id, ACTION_INSERT, inserts, me)
      if (ordered.length > 0) {
        pushClaimRows(out, line.id, lineIndex, lineNo, ACTION_INSERT, ordered, me, suspended, zebra)
      }
    }
  })

  return out
}
