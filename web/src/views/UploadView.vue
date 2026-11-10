<script setup lang="ts">
// 上传页。
//
// 只做三件事：选文件、前端先校验一遍、传完跳预览页。
// 校验放在上传前不是为了性能，是为了 20MB 的包在慢网上飞完四十秒才被后端拒。
// file.type 在部分系统上是空串（尤其是从网盘拖下来的 pdf），所以后缀再判一次。

import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'

import { ApiError } from '@/api/client'
import { uploadFile } from '@/api/files'
import ReviewHistoryList from '@/components/ReviewHistoryList.vue'

const MAX_BYTES = 20 * 1024 * 1024

const router = useRouter()

const picked = ref<File | null>(null)
const uploading = ref(false)
/** null 表示拿不到总大小：不显示百分比，只显示"上传中…" */
const percent = ref<number | null>(null)
const errorText = ref('')

const sizeText = computed(() => {
  const f = picked.value
  if (!f) return ''
  const mb = f.size / 1024 / 1024
  return mb >= 1 ? `${mb.toFixed(1)} MB` : `${Math.max(1, Math.round(f.size / 1024))} KB`
})

function onFileChange(uploadFile_: { raw?: File }): void {
  errorText.value = ''
  percent.value = null
  picked.value = uploadFile_.raw ?? null
}

function onExceed(): void {
  ElMessage.warning('一次只能处理一份合同，请先移除已选文件')
}

/** 左栏点一行：直接进全屏标注页，与上传成功后走同一条路。 */
function openReview(fileId: string): void {
  void router.push({ name: 'review', params: { fileId } })
}

/** 早失败：这两条与后端 41301/41501 的判断保持一致，只是省一次往返。 */
function validate(file: File): string | null {
  if (file.size > MAX_BYTES) return '文件超过 20MB'
  const isPdf = file.type === 'application/pdf' || file.name.toLowerCase().endsWith('.pdf')
  if (!isPdf) return '只支持 PDF 文件'
  return null
}

async function start(): Promise<void> {
  const file = picked.value
  if (!file || uploading.value) return
  const invalid = validate(file)
  if (invalid) {
    errorText.value = invalid
    ElMessage.error(invalid)
    return
  }
  uploading.value = true
  errorText.value = ''
  percent.value = null
  try {
    const record = await uploadFile(file, (p) => {
      percent.value = p
    })
    // 同内容文件会命中后端缓存、拿回同一个 id，这不是错误，照常跳转。
    await router.push({ name: 'review', params: { fileId: record.id } })
  } catch (err) {
    const message = err instanceof ApiError ? err.message : '上传失败，请重试'
    errorText.value = message
    ElMessage.error(message)
  } finally {
    uploading.value = false
  }
}
</script>

<template>
  <div class="upload-page">
    <header class="topbar">
      <div class="brand">劳动法 AI 顾问</div>
      <nav class="links">
        <router-link :to="{ name: 'chat' }">对话咨询</router-link>
        <span class="current">合同审查</span>
      </nav>
    </header>

    <div class="workspace">
      <aside class="pane-history">
        <ReviewHistoryList @open="openReview" />
      </aside>

      <main class="pane-work">
        <h1 class="title">上传劳动合同</h1>
        <p class="subtitle">
          上传后会先解析文字层，再逐条核对试用期、社保、押金、最低工资等 8 类风险，命中的每一条都能定位到原文位置。
        </p>

        <el-upload
          class="drop"
          drag
          accept="application/pdf,.pdf"
          :auto-upload="false"
          :show-file-list="false"
          :limit="1"
          :on-change="onFileChange"
          :on-exceed="onExceed"
        >
          <div class="drop-inner">
            <div class="drop-icon">PDF</div>
            <p class="drop-main">把合同拖到这里，或<em>点击选择文件</em></p>
            <p class="drop-hint">只支持 PDF，单份不超过 20MB</p>
          </div>
        </el-upload>

        <div v-if="picked" class="picked">
          <div class="picked-file">
            <span class="picked-name">{{ picked.name }}</span>
            <span class="picked-size">{{ sizeText }}</span>
          </div>
          <div class="picked-actions">
            <el-progress
              v-if="uploading"
              class="progress"
              :percentage="percent ?? 0"
              :indeterminate="percent === null"
              :show-text="percent !== null"
              :stroke-width="8"
            />
            <span v-else-if="percent === null && uploading" class="hint">上传中…</span>
            <el-button type="primary" :loading="uploading" :disabled="uploading" @click="start">
              开始审查
            </el-button>
          </div>
        </div>

        <p v-if="errorText" class="error">{{ errorText }}</p>
      </main>
    </div>
  </div>
</template>

<style scoped>
.upload-page {
  /* 同 .review-page：外层 .app-shell 已是 100vh，这里再要 100vh 会多出一条纵向滚动条。 */
  display: flex;
  flex-direction: column;
  min-height: 0;
  height: 100%;
  background: #f5f7fa;
}

.topbar {
  flex: none;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 24px;
  height: 56px;
  background: #fff;
  border-bottom: 1px solid #e5e7eb;
}

.brand {
  font-weight: 600;
  color: #1f2937;
}

.links {
  display: flex;
  gap: 16px;
  font-size: 14px;
}

.links a {
  color: #2563eb;
  text-decoration: none;
}

.current {
  color: #6b7280;
}

.workspace {
  flex: 1;
  min-height: 0;
  display: grid;
  grid-template-columns: 340px 1fr;
}

.pane-history {
  min-height: 0;
  background: #fff;
  border-right: 1px solid #e5e7eb;
}

.pane-work {
  min-height: 0;
  overflow-y: auto;
  padding: 40px 32px 64px;
}

/* 右栏内容保持原来的居中宽度，只是不再居中于整页 */
.pane-work > * {
  max-width: 640px;
  margin-left: auto;
  margin-right: auto;
}

/* 窄屏：左栏改成顶部一条，仍然可见——「看不见历史」正是这次要解决的问题，
   不能一窄就藏起来。 */
@media (max-width: 1100px) {
  .workspace {
    grid-template-columns: 1fr;
    grid-template-rows: minmax(180px, 34vh) 1fr;
  }

  .pane-history {
    border-right: none;
    border-bottom: 1px solid #e5e7eb;
  }

  .pane-work {
    padding: 24px 20px 48px;
  }
}

.title {
  margin: 0 0 8px;
  font-size: 22px;
  color: #1f2937;
}

.subtitle {
  margin: 0 0 24px;
  font-size: 14px;
  line-height: 1.7;
  color: #6b7280;
}

.drop :deep(.el-upload-dragger) {
  padding: 32px 16px;
  border-radius: 10px;
  background: #fff;
}

.drop-inner {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
}

.drop-icon {
  width: 44px;
  height: 44px;
  line-height: 44px;
  border-radius: 10px;
  background: #eef2ff;
  color: #4f46e5;
  font-size: 12px;
  font-weight: 700;
}

.drop-main {
  margin: 6px 0 0;
  font-size: 14px;
  color: #374151;
}

.drop-main em {
  font-style: normal;
  color: #2563eb;
}

.drop-hint {
  margin: 0;
  font-size: 12px;
  color: #9ca3af;
}

.picked {
  margin-top: 20px;
  padding: 16px;
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 10px;
}

.picked-file {
  display: flex;
  align-items: baseline;
  gap: 10px;
}

.picked-name {
  font-size: 14px;
  color: #111827;
  word-break: break-all;
}

.picked-size {
  font-size: 12px;
  color: #9ca3af;
}

.picked-actions {
  display: flex;
  align-items: center;
  gap: 16px;
  margin-top: 14px;
}

.progress {
  flex: 1;
}

.hint {
  font-size: 13px;
  color: #6b7280;
}

.error {
  margin-top: 16px;
  font-size: 13px;
  color: #d93026;
}
</style>
