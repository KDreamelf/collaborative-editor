export interface Article {
  id: string
  title: string
}

export interface Line {
  id: string
  prev?: string
  next?: string
  content: string
}

export interface Dispute {
  id: string
  realLine: string
  action: string
  person: string
  content: string[]
  /** 跨多条正式行的整段「改这行」：原跨度各正式行 ID，首项即 realLine */
  baseIDs?: string[]
  followers: string[]
  pendingConfirm?: { from: string; to: string; clientTs: number }[]
}

export interface Person {
  id: string
  name: string
}

export interface Cursor {
  personId: string
  name: string
  lineId: string
  disputeId?: string
  partIndex?: number
  selEndLineId?: string
  selEndDisputeId?: string
  selEndPartIndex?: number
  offset: number
  selEnd: number
}

export interface Snapshot {
  type?: string
  yourLine?: string
  article: Article
  lines: Line[]
  disputes: Dispute[]
  suspended: string[]
  people: Person[]
  cursors: Cursor[]
}

export interface FollowAsk {
  type?: string
  fromId: string
  fromName: string
  disputeId: string
  clientTs: number
}

export interface FollowResult {
  type?: string
  status: string
  disputeId?: string
}

export interface VisualRow {
  key: string
  lineId: string
  lineIndex: number
  action: string
  disputeId: string
  /** 点击追随用；他人插入块上下文行与主张行都指向真实争议 id */
  followId: string
  personId: string
  content: string
  /** 多行主张时的行内下标；普通行与单行主张为 0 */
  partIndex: number
  partCount: number
  isSelf: boolean
  /** 插入块内复用正式行正文的首行 */
  isContext: boolean
  /** 上下文可编辑正文，但注意力属于对应方向的插入主张。 */
  contextAction?: string
  showLineNo: boolean
  lineNo: number
  followerCount: number
  /** gutter 色点：主张者 + 各 follower id，仅他人块首行 */
  gutterDots: string[]
  suspended: boolean
  editable: boolean
  zebra: number
  phantom: boolean
  /** 候选块首行（自我左边线等） */
  blockStart: boolean
  /** 同争议组第 2+ 个候选首行前画分隔线；组顶/底与块内续行不标 */
  separatorBefore: boolean
  /** 整段跨度主张的基准行 ID；有则提交走 SubmitSpanEdit */
  spanBaseIDs?: string[]
}

export const ACTION_EDIT = '改这行'
export const ACTION_INSERT = '插在后面'
export const ACTION_INSERT_BEFORE = '插在前面'
export function isInsertAction(action: string): boolean {
  return action === ACTION_INSERT || action === ACTION_INSERT_BEFORE
}
export const ACTION_DELETE = '删这行'
export const DELETE_LABEL = '删除此行'

export const PALETTE = [
  '#e57373',
  '#64b5f6',
  '#81c784',
  '#ffb74d',
  '#ba68c8',
  '#4db6ac',
  '#f06292',
  '#a1887f',
  '#90a4ae',
  '#ff8a65',
]

export function colorFor(personId: string): string {
  let h = 0
  for (let i = 0; i < personId.length; i++) {
    h = (h * 31 + personId.charCodeAt(i)) >>> 0
  }
  return PALETTE[h % PALETTE.length]
}
