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
  followers: string[]
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
  personId: string
  content: string
  /** 多行主张时的行内下标；普通行与单行主张为 0 */
  partIndex: number
  partCount: number
  isSelf: boolean
  showLineNo: boolean
  lineNo: number
  followerCount: number
  suspended: boolean
  editable: boolean
  zebra: number
  phantom: boolean
}

export const ACTION_EDIT = '改这行'
export const ACTION_INSERT = '插在后面'

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
