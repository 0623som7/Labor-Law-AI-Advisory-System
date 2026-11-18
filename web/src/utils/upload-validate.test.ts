import test from 'node:test'
import assert from 'node:assert/strict'

import { MAX_UPLOAD_BYTES, validateUpload } from './upload-validate.ts'

/** 造一个只带 size/type/name 的 File，不上真文件。 */
function fakeFile(size: number, name = '劳动合同.pdf', type = 'application/pdf'): File {
  return { size, name, type } as File
}

test('超过 5MB 在选择阶段就被拦下，红字要带实际体积', () => {
  const big = fakeFile(11 * 1024 * 1024)
  assert.equal(validateUpload(big), '这份 11.0 MB，超过 5 MB 上限，请压缩后再传')
})

test('正好 5MB 放行（与后端的 > 口径一致）', () => {
  assert.equal(validateUpload(fakeFile(MAX_UPLOAD_BYTES)), null)
  assert.equal(
    validateUpload(fakeFile(MAX_UPLOAD_BYTES + 1)),
    '这份 5.0 MB，超过 5 MB 上限，请压缩后再传',
  )
})

test('非 PDF 拦下，type 为空时看后缀', () => {
  assert.equal(validateUpload(fakeFile(1024, '合同.docx', 'application/msword')), '只支持 PDF 文件')
  assert.equal(validateUpload(fakeFile(1024, '合同.PDF', '')), null)
})
