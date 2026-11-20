import assert from 'node:assert/strict'
import { test } from 'node:test'
import { dedupeTitles, parseAskTitle, sessionTitle, stripExtension } from './session-title.ts'

test('有文件名：文件名去扩展名 + 条款', () => {
  assert.equal(sessionTitle({ fileName: '劳动合同.pdf', section: '第七条' }), '劳动合同 · 第七条')
})

test('没有文件名：条款 + 规则号', () => {
  assert.equal(
    sessionTitle({ section: '第七条', ruleId: 'R-PROBATION-ONCE' }),
    '第七条 · R-PROBATION-ONCE',
  )
})

test('只有条款：就只显示条款', () => {
  assert.equal(sessionTitle({ section: '第三条' }), '第三条')
})

test('超长文件名按 18 字截断加省略号', () => {
  const long = '广州百联家政服务有限公司劳动合同（2026版）'
  // 18 字：广州百联家政服务有限公司劳动合同（2 —— 常见名如 contract-demo.pdf 能整个装下。
  assert.equal(sessionTitle({ fileName: long, section: '第七条' }), '广州百联家政服务有限公司劳动合同（2… · 第七条')
})

test('超长条款按 10 字截断', () => {
  const section = '第一条第（三）项关于试用期工资的约定'
  const out = sessionTitle({ section })
  assert.ok(out.startsWith('第一条第（三）项关于…'), out)
})

test('整条标题不超过 30 字', () => {
  const out = sessionTitle({ fileName: '一二三四五六七八九十十一十二.pdf', section: '一二三四五六七八九十十一' })
  assert.ok([...out].length <= 30, `${[...out].length} 字：${out}`)
})

test('空 section 且无文件名：用 fallback', () => {
  assert.equal(sessionTitle({ fileName: '', section: '  ', ruleId: '', fallback: '合同到期不续签有补偿吗' }), '合同到期不续签有补偿吗')
})

test('什么都没有时也不返回空串之外的脏值（空 fallback → 空串）', () => {
  assert.equal(sessionTitle({}), '')
})

test('中文标点原样保留', () => {
  assert.equal(sessionTitle({ fileName: '劳动合同（示范文本）.pdf', section: '第九条（一）' }), '劳动合同（示范文本） · 第九条（一）')
})

test('扩展名只吃最后一段，且不误伤带点的中文名', () => {
  assert.equal(stripExtension('劳动合同.pdf'), '劳动合同')
  assert.equal(stripExtension('2026.11 合同.pdf'), '2026.11 合同')
  assert.equal(stripExtension('v1.0'), 'v1')
  assert.equal(stripExtension('没有扩展名'), '没有扩展名')
})

test('从原话标题里认出条款号与规则号', () => {
  const parsed = parseAskTitle('合同「第七条」 · 合同中有 2 处互不相同的试用期约定 · 结论 R-PROBATION-ONCE')
  assert.equal(parsed.section, '第七条')
  assert.equal(parsed.ruleId, 'R-PROBATION-ONCE')
})

test('原话不带条款时解析为空串', () => {
  assert.deepEqual(parseAskTitle('合同到期不续签有补偿吗'), { section: '', ruleId: '' })
})

test('新形态（后端 structuredTitle）：· 规则号 也能解析出来', () => {
  // 后端存的是 `合同「第七条」 · R-PROBATION-ONCE`，没有「结论」两个字
  assert.deepEqual(parseAskTitle('合同「第七条」 · R-PROBATION-ONCE'), {
    section: '第七条',
    ruleId: 'R-PROBATION-ONCE',
  })
})

test('新形态接到 sessionTitle：侧栏显示 条款 · 规则号', () => {
  const parsed = parseAskTitle('合同「第七条」 · R-PROBATION-ONCE')
  assert.equal(sessionTitle({ section: parsed.section, ruleId: parsed.ruleId }), '第七条 · R-PROBATION-ONCE')
})

test('负例：括号里没有规则号时不硬认', () => {
  const parsed = parseAskTitle('合同「第七条」 · 合同中有 2 处（无规则号）')
  assert.equal(parsed.ruleId, '')
  assert.equal(sessionTitle({ section: parsed.section, ruleId: parsed.ruleId }), '第七条')
})

test('同名去重：第二次起追加 #2 #3，顺序不变', () => {
  assert.deepEqual(dedupeTitles(['第七条', '第十二条', '第七条', '第七条']), ['第七条', '第十二条', '第七条 #2', '第七条 #3'])
})

test('去重不合并不同标题，也不给唯一的加后缀', () => {
  assert.deepEqual(dedupeTitles(['a', 'b', 'c']), ['a', 'b', 'c'])
})

test('会话带了文件名：标题以文件名开头（侧栏据此分辨是哪份合同）', () => {
  const withFile = sessionTitle({ fileName: '劳动合同.pdf', section: '第七条', ruleId: 'R-PROBATION-ONCE' })
  assert.ok(withFile.startsWith('劳动合同'), withFile)
  assert.equal(withFile, '劳动合同 · 第七条')
  // 同一份合同同一条款的三次追问，靠 dedupe 才能分开
  assert.deepEqual(dedupeTitles([withFile, withFile, withFile]), [
    '劳动合同 · 第七条',
    '劳动合同 · 第七条 #2',
    '劳动合同 · 第七条 #3',
  ])
})
