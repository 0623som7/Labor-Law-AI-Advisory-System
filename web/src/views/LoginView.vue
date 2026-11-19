<script setup lang="ts">
// 登录 / 注册两页合一。
//
// 校验只做前端能做的那部分（格式与长度），文案不自己编：
// 手机号已被注册、密码错误这些结论只有后端知道，直接显示 ApiError.message。
// 后端返回带 fields 时把字段级提示挂到对应输入框下面，与接口的 fields 字段对齐。

import { computed, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { ApiError } from '@/api/client'
import { resetSessionExpired } from '@/api/session-expiry'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

// 只接受站内路径，防 //evil.com 这种协议相对跳转。
const redirect = computed(() => {
  const raw = route.query.redirect
  if (typeof raw !== 'string') return ''
  if (!raw.startsWith('/') || raw.startsWith('//')) return ''
  return raw
})

const mode = ref<'login' | 'register'>('login')
const form = reactive({ phone: '', password: '', nickname: '' })
const fieldErrors = reactive<Record<string, string>>({})
const formError = ref('')

const PHONE_RE = /^1[3-9]\d{9}$/

const submitting = computed(() => auth.loading)

function resetFeedback(): void {
  formError.value = ''
  for (const key of Object.keys(fieldErrors)) delete fieldErrors[key]
}

function validate(): boolean {
  resetFeedback()
  if (!PHONE_RE.test(form.phone.trim())) {
    fieldErrors.phone = '请输入 11 位手机号'
    return false
  }
  if (form.password.length < 8) {
    fieldErrors.password = '密码至少 8 位'
    return false
  }
  return true
}

async function submit(): Promise<void> {
  if (submitting.value) return
  if (!validate()) return

  try {
    if (mode.value === 'login') {
      await auth.login({ phone: form.phone.trim(), password: form.password })
    } else {
      await auth.register({
        phone: form.phone.trim(),
        password: form.password,
        nickname: form.nickname.trim(),
      })
    }
    ElMessage.success(mode.value === 'login' ? '登录成功' : '注册成功')
    // 登录成功要把失效标志位放开，否则下一次会话过期弹不出提示。
    resetSessionExpired()
    await router.replace(redirect.value || { name: 'chat' })
  } catch (error) {
    if (error instanceof ApiError) {
      formError.value = error.message
      if (error.fields) {
        for (const [key, value] of Object.entries(error.fields)) fieldErrors[key] = value
      }
    } else {
      formError.value = error instanceof Error ? error.message : '请求失败，请重试'
    }
  }
}

function switchMode(next: 'login' | 'register'): void {
  mode.value = next
  resetFeedback()
}
</script>

<template>
  <div class="login-page">
    <!-- 左栏品牌：只放静态文案，不参与登录逻辑 -->
    <section class="brand">
      <span class="brand-mark" aria-hidden="true">
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">
          <path d="M12 4v16" />
          <path d="M5 8h14" />
          <path d="M5 8 2 15h6L5 8Z" />
          <path d="M19 8l-3 7h6l-3-7Z" />
          <path d="M8 20h8" />
        </svg>
      </span>
      <h1>劳动法 AI 顾问</h1>
      <p class="brand-tagline">条文可溯源 · 风险定位到原文 · 修改建议可直接复制</p>
      <ul class="brand-points">
        <li class="brand-point"><span class="brand-dot" aria-hidden="true"></span>回答逐条标注法条出处，不凭空生成</li>
        <li class="brand-point"><span class="brand-dot" aria-hidden="true"></span>上传合同自动比对 20 条规则，风险框标到条款上</li>
        <li class="brand-point"><span class="brand-dot" aria-hidden="true"></span>不确定的地方直接给出边界条件，不含糊其辞</li>
      </ul>
    </section>

    <div class="login-shell">
    <section class="login-card">
      <header class="login-head">
        <h1>劳动法 AI 顾问</h1>
        <p class="login-sub">基于法条原文的劳动法咨询，回答逐条标注出处。</p>
      </header>

      <div class="login-tabs">
        <button
          type="button"
          class="login-tab"
          :class="{ active: mode === 'login' }"
          @click="switchMode('login')"
        >
          登录
        </button>
        <button
          type="button"
          class="login-tab"
          :class="{ active: mode === 'register' }"
          @click="switchMode('register')"
        >
          注册
        </button>
      </div>

      <form class="login-form" @submit.prevent="submit">
        <label class="field">
          <span class="field-label">手机号</span>
          <el-input
            v-model="form.phone"
            placeholder="11 位手机号"
            maxlength="11"
            autocomplete="username"
            :disabled="submitting"
          />
          <span v-if="fieldErrors.phone" class="field-error">{{ fieldErrors.phone }}</span>
        </label>

        <label class="field">
          <span class="field-label">密码</span>
          <el-input
            v-model="form.password"
            type="password"
            placeholder="至少 8 位"
            show-password
            :autocomplete="mode === 'login' ? 'current-password' : 'new-password'"
            :disabled="submitting"
            @keyup.enter="submit"
          />
          <span v-if="fieldErrors.password" class="field-error">{{ fieldErrors.password }}</span>
        </label>

        <label v-if="mode === 'register'" class="field">
          <span class="field-label">昵称（可不填）</span>
          <el-input
            v-model="form.nickname"
            placeholder="怎么称呼你"
            maxlength="32"
            :disabled="submitting"
            @keyup.enter="submit"
          />
          <span v-if="fieldErrors.nickname" class="field-error">{{ fieldErrors.nickname }}</span>
        </label>

        <p v-if="formError" class="form-error">{{ formError }}</p>

        <el-button type="primary" class="submit" :loading="submitting" native-type="submit">
          {{ mode === 'login' ? '登录' : '注册并进入' }}
        </el-button>
      </form>

      <footer class="login-foot">
        <span v-if="mode === 'login'">还没有账号？<a href="#" @click.prevent="switchMode('register')">注册一个</a></span>
        <span v-else>已有账号？<a href="#" @click.prevent="switchMode('login')">去登录</a></span>
      </footer>
    </section>
    </div>
  </div>
</template>

<style scoped>
.login-page {
  flex: 1;
  display: grid;
  /* 左品牌栏固定占比，右栏吃掉剩余；420px 下限保证品牌语不断成单字 */
  grid-template-columns: minmax(420px, 44%) 1fr;
  padding: 0;
  overflow: auto;
}

.brand {
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 18px;
  padding: 0 56px;
  color: var(--ll-on-primary);
  /* 品牌栏底色单独一个令牌：深色主题下 --ll-primary 是亮蓝，白字只有 2.6:1 */
  background: var(--ll-brand-bg);
}

.brand-mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  border: 1px solid color-mix(in srgb, var(--ll-on-primary) 45%, transparent);
  border-radius: var(--ll-radius-sm);
}

.brand h1 {
  margin: 0;
  /* 品牌栏主标题 */
  font-size: var(--ll-fs-display);
  font-weight: 600;
}

.brand-tagline {
  margin: 0;
  /* 品牌定位语 */
  font-size: var(--ll-fs-body);
  opacity: 0.82;
}

.brand-points {
  display: flex;
  flex-direction: column;
  gap: 10px;
  margin: 8px 0 0;
  padding: 0;
  list-style: none;
}

.brand-point {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  /* 品牌三点：说明文字 */
  font-size: var(--ll-fs-meta);
  line-height: var(--ll-lh-body);
  opacity: 0.85;
}

.brand-dot {
  flex: none;
  width: 6px;
  height: 6px;
  margin-top: 7px;
  background: var(--ll-on-primary);
  border-radius: 999px;
  opacity: 0.7;
}

.login-shell {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 48px 32px;
  background: var(--ll-bg);
}

/* 窄屏：品牌栏改顶部横条，不许藏起来 */
@media (max-width: 960px) {
  .login-page {
    grid-template-columns: 1fr;
  }

  .brand {
    flex-direction: row;
    align-items: center;
    justify-content: flex-start;
    gap: 12px;
    padding: 20px 24px;
  }

  .brand-points {
    display: none;
  }

  .login-shell {
    padding: 24px 16px 40px;
  }
}

.login-card {
  width: 100%;
  max-width: 380px;
  padding: 32px 32px 24px;
  background: var(--ll-surface);
  border: 1px solid var(--ll-border);
  border-radius: var(--ll-radius);
  box-shadow: var(--ll-shadow-float);
}

.login-head h1 {
  margin: 0 0 6px;
  /* 登录页主标题 */
  font-size: var(--ll-fs-display);
  font-weight: 600;
}

.login-sub {
  margin: 0;
  /* 登录页副标题：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-weak);
}

.login-tabs {
  display: flex;
  gap: 4px;
  margin: 20px 0 16px;
  padding: 4px;
  background: var(--ll-bg);
  border-radius: var(--ll-radius-md);
}

.login-tab {
  flex: 1;
  padding: 8px 0;
  /* 登录/注册切换：tab */
  font-size: var(--ll-fs-tag);
  color: var(--ll-text-weak);
  cursor: pointer;
  background: transparent;
  border: 0;
  border-radius: var(--ll-radius-sm);
}

.login-tab.active {
  color: var(--ll-text);
  background: var(--ll-surface);
  box-shadow: var(--ll-shadow-card);
}

.login-form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.field-label {
  /* 表单标签 */
  font-size: var(--ll-fs-body);
  line-height: var(--ll-lh-body);
  color: var(--ll-text-weak);
}

.field-error {
  /* 字段错误：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-danger);
}

.form-error {
  margin: 0;
  /* 表单错误：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-danger);
}

.submit {
  width: 100%;
  margin-top: 4px;
}

.login-foot {
  margin-top: 16px;
  /* 页脚说明 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-weak);
  text-align: center;
}

.login-foot a {
  color: var(--ll-primary);
  text-decoration: none;
}
</style>
