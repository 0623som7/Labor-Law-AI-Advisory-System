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
</template>

<style scoped>
.login-page {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  overflow: auto;
}

.login-card {
  width: 100%;
  max-width: 380px;
  padding: 28px 28px 20px;
  background: var(--ll-surface);
  border: 1px solid var(--ll-border);
  border-radius: var(--ll-radius);
  box-shadow: 0 8px 24px rgb(31 35 41 / 6%);
}

.login-head h1 {
  margin: 0 0 6px;
  font-size: 20px;
  font-weight: 600;
}

.login-sub {
  margin: 0;
  font-size: 12px;
  color: var(--ll-text-weak);
}

.login-tabs {
  display: flex;
  gap: 4px;
  margin: 20px 0 16px;
  padding: 3px;
  background: var(--ll-bg);
  border-radius: 8px;
}

.login-tab {
  flex: 1;
  padding: 6px 0;
  font-size: 13px;
  color: var(--ll-text-weak);
  cursor: pointer;
  background: transparent;
  border: 0;
  border-radius: 6px;
}

.login-tab.active {
  color: var(--ll-text);
  background: var(--ll-surface);
  box-shadow: 0 1px 2px rgb(31 35 41 / 8%);
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
  font-size: 13px;
  color: var(--ll-text-weak);
}

.field-error {
  font-size: 12px;
  color: #d93026;
}

.form-error {
  margin: 0;
  font-size: 13px;
  color: #d93026;
}

.submit {
  width: 100%;
  margin-top: 4px;
}

.login-foot {
  margin-top: 16px;
  font-size: 12px;
  color: var(--ll-text-weak);
  text-align: center;
}

.login-foot a {
  color: var(--ll-primary);
  text-decoration: none;
}
</style>
