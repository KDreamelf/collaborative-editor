/** 可行动的用户文案；HTTP/JSON 原文与内部细节不进此表。 */
const KNOWN = new Set(['无法连接服务器', '文章不存在'])

const LOG_ID_RE = /E\d{8}-\d{6}-[0-9a-z]+/
const LOG_ID_IN_MSG_RE = /日志编号\s+(E\d{8}-\d{6}-[0-9a-z]+)/

function pad(n: number, w = 2): string {
  return String(n).padStart(w, '0')
}

/** 时间戳类日志编号，例如 E20260324-143052-a3f */
export function newLogId(): string {
  const d = new Date()
  const stamp =
    `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-` +
    `${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`
  const rand = Math.floor(Math.random() * 36 ** 3)
    .toString(36)
    .padStart(3, '0')
  return `E${stamp}-${rand}`
}

function rawMessage(e: unknown): string {
  if (typeof e === 'string') return e
  if (e instanceof Error) return e.message
  return String(e)
}

function existingLogId(msg: string): string | null {
  const labeled = msg.match(LOG_ID_IN_MSG_RE)
  if (labeled) return labeled[1]
  const bare = msg.match(LOG_ID_RE)
  return bare && bare[0] === msg.trim() ? bare[0] : null
}

/**
 * HTTP 失败文案：服务端已带日志编号则只回干净编号句；404→文章不存在；
 * 其余本地生成编号（仅进 console，服务端不可达时无处可查服务端日志）。
 */
export function httpFailureMessage(status: number, body: string): string {
  const trimmed = body.replace(/\r?\n/g, ' ').trim()
  if (status === 404) return '文章不存在'
  const prior = existingLogId(trimmed)
  if (prior) {
    return `请求失败，日志编号 ${prior}`
  }
  const id = newLogId()
  console.error(`[${id}] HTTP ${status}`, body)
  return `请求失败，日志编号 ${id}`
}

/** 用户可见文案；未知错误只给干净编号句，原文进 console。已有编号不二次生成。 */
export function formatUserError(e: unknown): string {
  const msg = rawMessage(e)
    .replace(/^Error:\s*/i, '')
    .trim()
  if (KNOWN.has(msg)) return msg
  const prior = existingLogId(msg)
  if (prior) {
    console.error(`[${prior}]`, e)
    return `出错了，日志编号 ${prior}`
  }
  const id = newLogId()
  console.error(`[${id}]`, e)
  return `出错了，日志编号 ${id}`
}
