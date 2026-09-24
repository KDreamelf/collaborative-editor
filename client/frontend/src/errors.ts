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
  '请先完成当前输入，再切换文档',
  '复制失败，请选中原文手动复制',
  '服务器地址不能为空',
  '请输入有效的 http 或 https 地址',
  '还有未发送或未同步的修改，请处理后再切换服务器',
])

const ACTIONABLE: Record<string, string> = {
  '没有这场争议': '这场争议已经结束，请查看最新内容',
  '还有未决主张，不能合并': '这里还有未解决的主张，请先处理争议再合并',
  '非空行不能直接删除': '请先选中要删除的文字',
  '没有这条正式行': '这一行已发生变化，请查看最新内容',
  '跨度内有未决插入、子争议或跨越锚点，不能整段替换': '这里还有未解决的主张，请先处理争议再调整行的结构',
}

const LOG_ID_RE = /E\d{8}-\d{6}-[0-9a-z]+/
const LOG_ID_IN_MSG_RE = /日志编号\s+(E\d{8}-\d{6}-[0-9a-z]+)/
/** Go onBootstrap 已 ReportError 后 emit 的唯一固定文案；原样显示，不二次落盘。 */
const HOLD_RECONNECT_RE = /^本地内容已保留，正在重连，日志编号 E\d{8}-\d{6}-[0-9a-z]+$/

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
  if (ACTIONABLE[msg]) return ACTIONABLE[msg]
  if (HOLD_RECONNECT_RE.test(msg)) return msg
  const prior = existingLogId(msg)
  if (prior) {
    persistUnknown(prior, e)
    return `出错了，日志编号 ${prior}`
  }
  const id = newLogId()
  persistUnknown(id, e)
  return `出错了，日志编号 ${id}`
}
