<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import { CalendarClock, CheckCircle2, Database, Download, FileJson, KeyRound, LoaderCircle, Mail, PackageCheck, ShieldAlert } from '@lucide/vue'
import { api } from '../api/client'
import CardSelect from '../components/CardSelect.vue'
import { useToast } from '../composables/useToast'
import { autoDeleteReadinessProblem, exportFormProblem } from '../utils/exportEligibility'

const runtime = ref({})
const settings = ref({})
const accounts = ref([])
const accountID = ref('')
const exportCount = ref(20)
const validDays = ref(0)
const format = ref('txt')
const availableCount = ref(0)
const usedCount = ref(0)
const busy = ref('')
const { success, error: showError } = useToast()

const accountOptions = computed(() => [
	{ value: '', label: '全部 Apple 账号', dot: 'bg-slate-400' },
	...accounts.value.map((account) => ({
		value: account.id,
		label: account.label || account.apple_id || account.id,
		description: account.auto_delete_ready ? '可限时导出' : '限时导出需重新协议登录',
		dot: account.auto_delete_ready ? 'bg-emerald-500' : 'bg-amber-500',
	})),
])
const formatOptions = [
  { value: 'txt', label: 'TXT', dot: 'bg-slate-400' },
  { value: 'csv', label: 'CSV', dot: 'bg-emerald-500' },
  { value: 'tsv', label: 'TSV', dot: 'bg-sky-500' },
  { value: 'jsonl', label: 'JSONL', dot: 'bg-violet-500' },
]
const countPresets = [10, 20, 50, 100]
const validityPresets = [0, 1, 3, 7, 30, 90]
const selectedAccount = computed(() => accounts.value.find((account) => String(account.id) === String(accountID.value)))
const formProblem = computed(() => exportFormProblem({
  availableCount: availableCount.value,
  exportCount: exportCount.value,
  validDays: validDays.value,
  accountID: accountID.value,
}))
const readinessProblem = computed(() => autoDeleteReadinessProblem(selectedAccount.value, validDays.value))
const canExport = computed(() => !formProblem.value)
const publicAPIKeyReady = computed(() => Boolean(String(settings.value.public_api_key || '').trim() || runtime.value.config_api_key_configured))

async function refreshCounts() {
  try {
    const query = accountID.value ? `&account_id=${encodeURIComponent(accountID.value)}` : ''
    const [available, used] = await Promise.all([
      api(`/api/mailboxes?status=available&page=1&page_size=1${query}`),
      api(`/api/mailboxes?status=used&page=1&page_size=1${query}`),
    ])
    availableCount.value = Number(available.total || 0)
    usedCount.value = Number(used.total || 0)
    if (availableCount.value > 0 && Number(exportCount.value) > availableCount.value) exportCount.value = availableCount.value
  } catch (err) {
    showError(err.message)
  }
}

async function load() {
  try {
    const [settingData, accountData] = await Promise.all([api('/api/settings'), api('/api/apple-accounts')])
    runtime.value = settingData.runtime || {}
    settings.value = settingData.settings || {}
    accounts.value = accountData.items || []
    await refreshCounts()
  } catch (err) {
    showError(err.message)
  }
}

function responseFilename(response, fallback) {
  const disposition = response.headers.get('content-disposition') || ''
  const match = disposition.match(/filename="?([^";]+)"?/i)
  return match?.[1] || fallback
}

async function downloadMailboxExport(kind) {
  if (!canExport.value) {
    if (formProblem.value) showError(formProblem.value)
    return
  }
  busy.value = kind
  try {
    const response = await fetch(`/api/runtime/export-mailbox-${kind === 'apis' ? 'apis' : 'emails'}`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        count: Number(exportCount.value),
        account_id: accountID.value,
        format: format.value,
        valid_days: Number(validDays.value),
      }),
    })
    if (!response.ok) {
      let message = `导出失败（HTTP ${response.status}）`
      try { message = (await response.json()).message || message } catch { /* 使用 HTTP 错误 */ }
      throw new Error(message)
    }
    const actualCount = Number(response.headers.get('x-export-count') || 0)
    const blob = await response.blob()
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = responseFilename(response, `icloud-mailboxes-${Date.now()}.${format.value}`)
    document.body.appendChild(link)
    link.click()
    link.remove()
    URL.revokeObjectURL(url)
    success(Number(validDays.value) > 0
      ? `已导出 ${actualCount} 个邮箱，有效期 ${Number(validDays.value)} 天`
      : `已导出 ${actualCount} 个邮箱，永久有效`)
    await refreshCounts()
  } catch (err) {
    showError(err.message)
  } finally {
    busy.value = ''
  }
}

watch(accountID, refreshCounts)
onMounted(load)
</script>

<template>
  <div class="mx-auto max-w-6xl space-y-5">
    <section class="panel overflow-hidden">
      <header class="flex flex-col gap-3 border-b border-slate-100 px-5 py-5 dark:border-slate-700 sm:flex-row sm:items-center sm:justify-between sm:px-6">
        <div><h2 class="text-lg font-black">邮箱交付导出</h2><p class="mt-1 text-sm text-slate-400">按账号和数量生成本次交付文件</p></div>
        <span class="inline-flex self-start items-center gap-1.5 rounded-full bg-emerald-100 px-3 py-1.5 text-xs font-bold text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-300"><PackageCheck :size="14" />导出后标记已使用</span>
      </header>

      <div class="grid border-b border-slate-100 dark:border-slate-700 sm:grid-cols-3">
        <div class="px-5 py-4 sm:px-6"><span class="text-xs text-slate-400">当前可用</span><strong class="mt-1 block text-2xl text-emerald-600">{{ availableCount }}</strong></div>
        <div class="border-t border-slate-100 px-5 py-4 dark:border-slate-700 sm:border-l sm:border-t-0 sm:px-6"><span class="text-xs text-slate-400">已使用</span><strong class="mt-1 block text-2xl text-slate-700 dark:text-slate-200">{{ usedCount }}</strong></div>
        <div class="border-t border-slate-100 px-5 py-4 dark:border-slate-700 sm:border-l sm:border-t-0 sm:px-6"><span class="text-xs text-slate-400">取码 API</span><strong :class="publicAPIKeyReady ? 'text-emerald-600' : 'text-amber-600'" class="mt-2 flex items-center gap-1.5 text-sm"><CheckCircle2 :size="15" />{{ publicAPIKeyReady ? '可导出' : '待配置全局 Key' }}</strong></div>
      </div>

      <div class="grid gap-4 p-5 sm:grid-cols-2 sm:p-6">
        <div class="form-group"><span class="form-label">来源账号</span><CardSelect v-model="accountID" :options="accountOptions" aria-label="导出来源账号" /></div>
        <div class="form-group"><span class="form-label">文件格式</span><CardSelect v-model="format" :options="formatOptions" aria-label="导出格式" /></div>
        <div class="form-group"><span class="form-label">导出数量</span><div class="flex gap-2"><input v-model.number="exportCount" class="field min-w-0 flex-1" type="number" min="1" max="10000" :placeholder="String(availableCount || 1)" /><button class="secondary-button shrink-0 px-3" type="button" :disabled="!availableCount" @click="exportCount = availableCount">全部可用</button></div></div>
        <div class="form-group"><span class="form-label">快速选择</span><div class="grid grid-cols-4 gap-2"><button v-for="count in countPresets" :key="count" type="button" class="h-11 rounded-lg border text-sm font-bold transition" :class="Number(exportCount) === count ? 'border-emerald-500 bg-emerald-50 text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300' : 'border-slate-200 bg-white text-slate-500 hover:border-emerald-300 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300'" :disabled="!availableCount" @click="exportCount = Math.min(count, availableCount)">{{ count }}</button></div></div>
        <div class="form-group sm:col-span-2">
          <span class="form-label">邮箱有效期</span>
          <div class="grid gap-2 sm:grid-cols-[minmax(0,1fr)_2fr]">
            <span class="field-wrap"><CalendarClock :size="17" class="field-icon" /><input v-model.number="validDays" class="field field-leading" type="number" min="0" max="3650" step="1" placeholder="0" /></span>
			<div class="grid grid-cols-3 gap-2 sm:grid-cols-6"><button v-for="days in validityPresets" :key="days" type="button" class="h-11 rounded-lg border text-xs font-bold transition" :class="Number(validDays) === days ? 'border-emerald-500 bg-emerald-50 text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300' : 'border-slate-200 bg-white text-slate-500 hover:border-emerald-300 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300'" @click="validDays = days">{{ days === 0 ? '永久' : `${days} 天` }}</button></div>
		  </div>
		  <span v-if="Number(validDays) === 0" class="form-help">0 表示永久，不会自动删除 Apple 端邮箱。</span>
		  <span v-else-if="!accountID" class="mt-2 flex items-start gap-2 rounded-lg bg-amber-50 px-3 py-2 text-xs leading-5 text-amber-700 dark:bg-amber-950/35 dark:text-amber-300"><ShieldAlert :size="15" class="mt-0.5 shrink-0" />请先选择一个 Apple 账号，限时导出不支持“全部账号”。</span>
		  <span v-else-if="readinessProblem" class="mt-2 flex items-start gap-2 rounded-lg bg-amber-50 px-3 py-2 text-xs leading-5 text-amber-700 dark:bg-amber-950/35 dark:text-amber-300"><ShieldAlert :size="15" class="mt-0.5 shrink-0" />{{ readinessProblem }} 导出时会由服务端再次即时校验。</span>
		  <span v-else class="form-help">到期后删除 Apple 端真实隐私邮箱；删除确认成功后，该账号的已占用数量自然减少。</span>
        </div>
      </div>

      <div class="grid border-t border-slate-100 dark:border-slate-700 md:grid-cols-2">
        <div class="flex items-center gap-4 px-5 py-5 sm:px-6"><span class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-emerald-100 text-emerald-600 dark:bg-emerald-950/60 dark:text-emerald-300"><Mail :size="19" /></span><div class="min-w-0 flex-1"><strong class="block text-sm">邮箱地址</strong><span class="mt-1 block text-xs text-slate-400">每行一个邮箱</span></div><button class="primary-button shrink-0" :disabled="busy || !canExport" @click="downloadMailboxExport('emails')"><LoaderCircle v-if="busy === 'emails'" :size="16" class="animate-spin" /><Download v-else :size="16" />导出</button></div>
        <div class="flex items-center gap-4 border-t border-slate-100 px-5 py-5 dark:border-slate-700 md:border-l md:border-t-0 sm:px-6"><span class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-amber-100 text-amber-600 dark:bg-amber-950/60 dark:text-amber-300"><KeyRound :size="19" /></span><div class="min-w-0 flex-1"><strong class="block text-sm">邮箱与取码 API</strong><span class="mt-1 block text-xs text-slate-400">地址和独立取码链接</span></div><button class="primary-button shrink-0" :disabled="busy || !canExport || !publicAPIKeyReady" @click="downloadMailboxExport('apis')"><LoaderCircle v-if="busy === 'apis'" :size="16" class="animate-spin" /><Download v-else :size="16" />导出</button></div>
      </div>
    </section>

    <section class="panel overflow-hidden">
      <header class="flex items-center justify-between border-b border-slate-100 px-5 py-4 dark:border-slate-700 sm:px-6"><div><h3 class="font-black">系统备份</h3><p class="mt-1 text-xs text-slate-400">完整运行状态和本地邮件</p></div><ShieldAlert :size="18" class="text-amber-500" /></header>
      <div class="divide-y divide-slate-100 dark:divide-slate-700">
        <a href="/api/runtime/export" download class="flex items-center gap-4 px-5 py-4 transition hover:bg-slate-50 dark:hover:bg-slate-800/50 sm:px-6"><span class="flex h-9 w-9 items-center justify-center rounded-lg bg-violet-100 text-violet-600 dark:bg-violet-950/60 dark:text-violet-300"><FileJson :size="18" /></span><span class="min-w-0 flex-1"><strong class="block text-sm">运行数据</strong><small class="mt-1 block text-xs text-slate-400">账号、邮箱、登录态和系统设置</small></span><Download :size="17" class="text-slate-400" /></a>
        <a href="/api/runtime/export?include_messages=1" download class="flex items-center gap-4 px-5 py-4 transition hover:bg-slate-50 dark:hover:bg-slate-800/50 sm:px-6"><span class="flex h-9 w-9 items-center justify-center rounded-lg bg-sky-100 text-sky-600 dark:bg-sky-950/60 dark:text-sky-300"><Database :size="18" /></span><span class="min-w-0 flex-1"><strong class="block text-sm">运行数据与邮件</strong><small class="mt-1 block text-xs text-slate-400">完整状态并包含本地邮件正文</small></span><Download :size="17" class="text-slate-400" /></a>
      </div>
    </section>
  </div>
</template>
