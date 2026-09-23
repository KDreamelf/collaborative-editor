/** 可行动的用户文案；内部协议/数据完整性错误不进此表。 */
const KNOWN = new Set([
  '无法连接服务器',
  '粘贴内容为空',
  '尚未加入文档',
  '文章不存在',
  '有一处修改未能同步到服务器，原文已保存在本地',
  '本地保存失败，请检查磁盘后重试',
  '无法跨争议区或他人候选做整段修改',
  '这次修改暂时无法应用，内容已保存在未同步修改中',
])

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

/** 控制台 + 本机 client.log；无 Wails 时仅 console。记日志失败不抛。 */
function persistUnknown(id: string, e: unknown): void {
  console.error(`[${id}]`, e)
  const go = (window as unknown as { go?: { main?: { App?: { ReportError?: (a: string, b: string) => Promise<void> } } } })
    ?.go?.main?.App?.ReportError
  if (typeof go !== 'function') return
  try {
    void Promise.resolve(go(id, rawMessage(e))).catch(() => {})
  } catch {
    /* 浏览器预览无 runtime */
  }
}

/** 用户可见文案；未知错误只给干净编号句，原文进 console/本机日志。已有编号不二次生成。 */
export function formatUserError(e: unknown): string {
  const msg = rawMessage(e)
    .replace(/^Error:\s*/i, '')
    .trim()
  if (KNOWN.has(msg)) return msg
  const prior = existingLogId(msg)
  if (prior) {
    persistUnknown(prior, e)
    return `出错了，日志编号 ${prior}`
  }
  const id = newLogId()
  persistUnknown(id, e)
  return `出错了，日志编号 ${id}`
}
