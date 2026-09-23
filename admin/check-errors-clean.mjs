/** 直接打真实 errors.ts：含编号+堆栈只回干净句。需 node --experimental-strip-types */
import { formatUserError, httpFailureMessage } from './src/errors.ts'

const id = 'E20260324-143052-a3f'
const dirty =
  `请求失败，日志编号 ${id}\n    at Object.foo (http://x/app.js:1:1)\nHTTP 500 raw`

const u = formatUserError(dirty)
const h = httpFailureMessage(500, dirty)
if (u !== `出错了，日志编号 ${id}`) {
  console.error('formatUserError', u)
  process.exit(1)
}
if (h !== `请求失败，日志编号 ${id}`) {
  console.error('httpFailureMessage', h)
  process.exit(1)
}
if (/at Object|http:\/\/|HTTP 500|raw/.test(u + h)) {
  console.error('leaked detail', u, h)
  process.exit(1)
}
console.log('ok')
