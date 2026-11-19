<script setup lang="ts">
// 上传页。
//
// 只做三件事：选文件、前端先校验一遍、传完跳预览页。
// 校验放在上传前不是为了性能，是为了 5MB 的包在慢网上飞完几十秒才被后端拒。
// file.type 在部分系统上是空串（尤其是从网盘拖下来的 pdf），所以后缀再判一次。

import { computed, onBeforeUnmount, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'

import { ApiError } from '@/api/client'
import { uploadFile } from '@/api/files'
import { MAX_UPLOAD_LABEL, validateUpload } from '@/utils/upload-validate'
import { breadcrumbFor } from '@/utils/breadcrumb'
import {
  clampSideWidth,
  KEY_STEP,
  MAX_SIDE,
  MIN_SIDE,
  nextWidthByKey,
  readSideWidth,
  writeSideWidth,
} from '@/utils/splitter'
import ReviewHistoryList from '@/components/ReviewHistoryList.vue'
import PageHeader from '@/components/PageHeader.vue'

const router = useRouter()

const crumbs = breadcrumbFor('upload')

// 左栏（审查历史）宽度：默认 340 与改动前 grid 的第一列一致。
// 历史条目标题长短差别很大，窄了要截断、宽了压上传区，所以给人一条拖拽线。
const SIDE_KEY = 'laborlaw.upload.sideWidth'
const SIDE_DEFAULT = 340
const sideWidth = ref(readSideWidth(SIDE_KEY, SIDE_DEFAULT, MIN_SIDE, MAX_SIDE))
const workEl = ref<HTMLElement | null>(null)
const dragging = ref(false)
const maxSideWidth = computed(() =>
  workEl.value ? Math.min(MAX_SIDE, Math.round(workEl.value.clientWidth * 0.6)) : MAX_SIDE,
)

let stopDrag: (() => void) | null = null

function onSplitterDown(e: PointerEvent): void {
  if (!workEl.value) return
  e.preventDefault()
  const target = e.currentTarget as HTMLElement
  target.setPointerCapture?.(e.pointerId)
  const rect = workEl.value.getBoundingClientRect()
  dragging.value = true

  const onMove = (ev: PointerEvent): void => {
    sideWidth.value = clampSideWidth(ev.clientX - rect.left, rect.width, {
      min: MIN_SIDE,
      max: MAX_SIDE,
      minOpposite: 320,
    })
  }
  const onUp = (): void => {
    dragging.value = false
    writeSideWidth(SIDE_KEY, sideWidth.value)
    stopDrag?.()
    stopDrag = null
  }
  target.addEventListener('pointermove', onMove)
  target.addEventListener('pointerup', onUp)
  target.addEventListener('pointercancel', onUp)
  stopDrag = () => {
    target.removeEventListener('pointermove', onMove)
    target.removeEventListener('pointerup', onUp)
    target.removeEventListener('pointercancel', onUp)
  }
}

function onSplitterKey(e: KeyboardEvent): void {
  const next = nextWidthByKey(sideWidth.value, e.key, {
    step: KEY_STEP,
    min: MIN_SIDE,
    max: maxSideWidth.value,
  })
  if (next === null) return
  sideWidth.value = next
  writeSideWidth(SIDE_KEY, next)
  e.preventDefault()
}

function resetSideWidth(): void {
  sideWidth.value = SIDE_DEFAULT
  writeSideWidth(SIDE_KEY, SIDE_DEFAULT)
}

onBeforeUnmount(() => {
  dragging.value = false
  stopDrag?.()
  stopDrag = null
})

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
  const file = uploadFile_.raw ?? null
  // 选完就判一次：不合格的文件不进"待上传"状态，用户不必点一次"开始上传"
  // 才知道自己选错了，也不会在慢网上白传几十秒才被拒。
  const invalid = file ? validateUpload(file) : null
  if (invalid) {
    ElMessage.error(invalid)
    errorText.value = invalid
    picked.value = null
    return
  }
  picked.value = file
}

function onExceed(): void {
  ElMessage.warning('一次只能处理一份合同，请先移除已选文件')
}

/** 左栏点一行：直接进全屏标注页，与上传成功后走同一条路。 */
function openReview(fileId: string): void {
  void router.push({ name: 'review', params: { fileId } })
}

async function start(): Promise<void> {
  const file = picked.value
  if (!file || uploading.value) return
  const invalid = validateUpload(file)
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
    // autostart=1 让审查页自己开跑：不带上它，用户传完不点按钮就走人，
    // files 表有行、reviews 没有行，文件在左栏（读 GET /reviews）里就消失了。
    await router.push({
      name: 'review',
      params: { fileId: record.id },
      query: { autostart: '1' },
    })
  } catch (err) {
    // 无文本层：文件已经落库了（后端先存记录再解析），只是审查无从下手。
    // 直接把它送进审查页看"为什么不能审"，比停在上传页只说一句错误有用。
    if (err instanceof ApiError && err.code === 42201 && err.fields?.file_id) {
      await router.push({ name: 'review', params: { fileId: err.fields.file_id } })
      return
    }
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
    <PageHeader title="劳动法 AI 顾问">
      <template #right>
        <nav class="links" aria-label="面包屑">
          <ol class="crumbs">
            <li v-for="c in crumbs" :key="c.label" class="crumb">
              <router-link v-if="c.to" :to="{ name: c.to }">{{ c.label }}</router-link>
              <span v-else class="current" aria-current="page">{{ c.label }}</span>
            </li>
          </ol>
        </nav>
      </template>
    </PageHeader>

    <div ref="workEl" class="workspace" :class="{ 'is-resizing': dragging }">
      <aside class="pane-history">
        <ReviewHistoryList @open="openReview" />
      </aside>

      <div
        class="splitter"
        role="separator"
        aria-orientation="vertical"
        :aria-valuenow="sideWidth"
        :aria-valuemin="MIN_SIDE"
        :aria-valuemax="maxSideWidth"
        aria-label="调整审查历史与上传区的宽度"
        title="拖动调宽，双击复位"
        tabindex="0"
        @pointerdown="onSplitterDown"
        @keydown="onSplitterKey"
        @dblclick="resetSideWidth"
      />

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
            <p class="drop-hint">只支持 PDF，单份不超过 {{ MAX_UPLOAD_LABEL }}</p>
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
  background: var(--ll-page-alt);
}

.links {
  /* 面包屑：正文导航 */
  font-size: var(--ll-fs-body);
  line-height: var(--ll-lh-body);
}

.crumbs {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.crumb {
  display: flex;
  align-items: center;
  gap: 8px;
}

.crumb + .crumb::before {
  content: '/';
  color: var(--ll-border);
}

.crumbs a {
  color: var(--ll-accent);
  text-decoration: none;
}

.crumbs a:hover {
  text-decoration: underline;
}

.current {
  color: var(--ll-text-weak);
}

.workspace {
  flex: 1;
  min-height: 0;
  display: grid;
  /* 中间 6px 是分隔条（第三列 1fr 是上传区）；默认第一列 340 与改动前一致。 */
  grid-template-columns: v-bind('sideWidth + "px"') 6px 1fr;
}

.workspace.is-resizing {
  cursor: col-resize;
  user-select: none;
}

.splitter {
  cursor: col-resize;
  background: transparent;
  border-left: 1px solid var(--ll-border-muted);
}

.splitter:hover,
.splitter:focus-visible {
  background: var(--ll-primary-wash);
  outline: none;
}

.pane-history {
  min-height: 0;
  background: var(--ll-surface);
  border-right: 1px solid var(--ll-border-muted);
}

.pane-work {
  min-height: 0;
  overflow-y: auto;
  padding: 32px 32px 32px;
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

  /* 单栏时没有第二条栏，分隔条留着会多一条 6px 的空白列。 */
  .splitter {
    display: none;
  }

  .pane-history {
    border-right: none;
    border-bottom: 1px solid var(--ll-border-muted);
  }

  .pane-work {
    /* 页面级左右内边距统一 32px */
    padding: 32px;
  }
}

.title {
  margin: 0 0 8px;
  /* 上传页主标题 */
  font-size: var(--ll-fs-display);
  color: var(--ll-text-body);
}

.subtitle {
  margin: 0 0 24px;
  /* 上传页副标题：说明文字 */
  font-size: var(--ll-fs-meta);
  line-height: 1.7;
  color: var(--ll-text-subtle);
}

.drop :deep(.el-upload-dragger) {
  padding: 32px 16px;
  border-radius: var(--ll-radius-md);
  background: var(--ll-surface);
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
  border-radius: var(--ll-radius-md);
  background: var(--ll-accent-wash-3);
  color: var(--ll-indigo);
  /* 拖拽区图标：标签 */
  font-size: var(--ll-fs-tag);
  font-weight: 700;
}

.drop-main {
  margin: 6px 0 0;
  /* 拖拽区主文案 */
  font-size: var(--ll-fs-body);
  line-height: var(--ll-lh-body);
  color: var(--ll-text-mid);
}

.drop-main em {
  font-style: normal;
  color: var(--ll-accent);
}

.drop-hint {
  margin: 0;
  /* 拖拽区提示：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-faint);
}

.picked {
  margin-top: 20px;
  padding: 16px;
  background: var(--ll-surface);
  border: 1px solid var(--ll-border-muted);
  border-radius: var(--ll-radius-md);
}

.picked-file {
  display: flex;
  align-items: baseline;
  gap: 10px;
}

.picked-name {
  /* 已选文件名：正文 */
  font-size: var(--ll-fs-body);
  line-height: var(--ll-lh-body);
  color: var(--ll-text-strong);
  word-break: break-all;
}

.picked-size {
  /* 已选文件大小：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-faint);
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
  /* 页脚提示：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-subtle);
}

.error {
  margin-top: 16px;
  /* 错误提示：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-danger);
}

/* 900 以下再钉一次单栏：三段式 grid 若被任何后续规则改回，这里兜住，
   不会出现「1fr 6px 1fr」在窄屏里挤出横向滚动条。 */
@media (max-width: 900px) {
  .workspace {
    grid-template-columns: 1fr;
  }
}
</style>
