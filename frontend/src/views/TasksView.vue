<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { Activity, CircleAlert, Gauge, LoaderCircle, Pause, Play, Plus, Settings, Trash2, X } from '@lucide/vue'
import { api } from '../api/client'
import CardSelect from '../components/CardSelect.vue'
import { useConfirm } from '../composables/useConfirm'
import { useToast } from '../composables/useToast'

const loading = ref(true)
const busy = ref('')
const taskBusy = reactive({})
const scheduler = ref({ tasks: [], stats: {} })
const accounts = ref([])
const showCreate = ref(false)
const showDefaults = ref(false)
let refreshTimer
const taskForm = reactive({ account_id: '', label: '', note: '', create_channel: 'auto', interval_minutes: 60, round_interval_seconds: 5, round_limit: 20, target: 750 })
const defaultForm = reactive({ label: '', note: '', account_ids: [], create_channel: 'auto', scheduler_create_channel: 'auto', apple_account_two_factor_method: 'trusted_device', icloud_web_two_factor_method: 'trusted_device', scheduler_interval_minutes: 60, scheduler_round_interval_seconds: 5, scheduler_round_limit: 20, scheduler_target: 750, mailbox_page_size: 7 })
const { success, error: showError } = useToast()
const { confirm: confirmAction } = useConfirm()

const tasks = computed(() => Array.isArray(scheduler.value.tasks) ? scheduler.value.tasks : [])
const taskAccountIDs = computed(() => new Set(tasks.value.map((task) => task.account_id)))
const accountMap = computed(() => new Map(accounts.value.map((account) => [account.id, account])))
const availableAccountOptions = computed(() => accounts.value.filter((account) => !taskAccountIDs.value.has(account.id)).map((account) => ({ value: account.id, label: account.label || account.apple_id || account.id, dot: 'bg-emerald-500' })))
const overview = computed(() => {
  const result = { total: tasks.value.length, active: 0, waiting: 0, capacity: 0, issues: 0, mailboxes: 0 }
  for (const task of tasks.value) {
    if (task.status === 'running' || task.status === 'creating') result.active++
    if (task.status === 'waiting' && Number(task.progress || 0) >= Number(task.target || 750)) result.capacity++
    else if (task.status === 'waiting') result.waiting++
    if (task.last_error) result.issues++
    result.mailboxes += Number(task.progress || 0)
  }
  return result
})

const createChannelOptions = [
  { value: 'auto', label: '自动接口', description: '新接口优先，失败后使用旧接口', dot: 'bg-slate-400' },
  { value: 'apple_account', label: 'Apple Account 新接口', dot: 'bg-violet-500' },
  { value: 'icloud_web', label: 'iCloud Web 旧接口', dot: 'bg-sky-500' },
]

function statusText(task) {
	if (task.status === 'waiting' && Number(task.progress || 0) >= Number(task.target || 750)) return '满额等待'
	if (task.status === 'completed') return '已完成'
  return ({ running: '创建中', creating: '创建中', waiting: '等待下一轮', paused: '已暂停' })[task.status] || '未启动'
}

function statusClass(task) {
  if (task.status === 'running' || task.status === 'creating') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-200'
  if (task.status === 'waiting' && Number(task.progress || 0) >= Number(task.target || 750)) return 'bg-violet-100 text-violet-700 dark:bg-violet-950/60 dark:text-violet-200'
  if (task.status === 'waiting') return 'bg-sky-100 text-sky-700 dark:bg-sky-950/60 dark:text-sky-200'
  if (task.status === 'completed') return 'bg-violet-100 text-violet-700 dark:bg-violet-950/60 dark:text-violet-200'
  return 'bg-slate-100 text-slate-500 dark:bg-slate-700 dark:text-slate-300'
}

function accountName(task) {
  const account = accountMap.value.get(task.account_id)
  return task.apple_id || account?.apple_id || account?.label || task.account_id
}

function progressPercent(task) {
  const target = Math.max(1, Number(task.target || 750))
  return Math.min(100, Math.max(0, Math.round(Number(task.progress || 0) / target * 100)))
}

function formatTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getFullYear() < 2000) return '-'
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(date)
}

function intervalText(seconds) {
  const value = Number(seconds || 0)
  if (value % 3600 === 0) return `${value / 3600} 小时`
  if (value % 60 === 0) return `${value / 60} 分钟`
  return `${value} 秒`
}

function assignDefaults(settings = {}) {
  Object.assign(defaultForm, {
    label: '', note: '', account_ids: [], create_channel: 'auto', scheduler_create_channel: 'auto', apple_account_two_factor_method: 'trusted_device', icloud_web_two_factor_method: 'trusted_device', scheduler_interval_minutes: 60, scheduler_round_interval_seconds: 5, scheduler_round_limit: 20, scheduler_target: 750, mailbox_page_size: 7,
  }, settings)
}

function openCreateDialog() {
  taskForm.account_id = availableAccountOptions.value[0]?.value || ''
  taskForm.label = String(defaultForm.label || '').toLowerCase() === 'x' ? '' : (defaultForm.label || '')
  taskForm.note = defaultForm.note || ''
  taskForm.create_channel = defaultForm.scheduler_create_channel || 'auto'
  taskForm.interval_minutes = defaultForm.scheduler_interval_minutes || 60
  taskForm.round_interval_seconds = defaultForm.scheduler_round_interval_seconds || 5
  taskForm.round_limit = defaultForm.scheduler_round_limit || 20
  taskForm.target = defaultForm.scheduler_target || 750
  showCreate.value = true
}

async function load() {
  loading.value = true
  try {
    const [schedulerData, accountData, settingData] = await Promise.all([api('/api/scheduler/status'), api('/api/apple-accounts'), api('/api/create-settings')])
    scheduler.value = schedulerData.scheduler || scheduler.value
    accounts.value = accountData.items || []
    assignDefaults(settingData.settings)
  } catch (err) {
    showError(err.message)
  } finally {
    loading.value = false
  }
}

async function refresh() {
  try {
    const data = await api('/api/scheduler/status')
    scheduler.value = data.scheduler || scheduler.value
  } catch { /* 主操作显示错误 */ }
}

async function createTask() {
  if (!taskForm.account_id) return
  busy.value = `start:${taskForm.account_id}`
  try {
    const data = await api(`/api/scheduler/tasks/${taskForm.account_id}/start`, { method: 'POST', body: JSON.stringify(taskForm) })
    scheduler.value = data.scheduler || scheduler.value
    showCreate.value = false
    success('账号任务已创建并开始同步邮箱')
  } catch (err) {
    showError(err.message)
  } finally {
    busy.value = ''
  }
}

async function pauseTask(task) {
	taskBusy[task.account_id] = 'pause'
  try {
    const data = await api(`/api/scheduler/tasks/${task.account_id}/pause`, { method: 'POST', body: '{}' })
    scheduler.value = data.scheduler || scheduler.value
    success('账号任务已暂停')
	} catch (err) { showError(err.message) } finally { delete taskBusy[task.account_id] }
}

async function resumeTask(task) {
	taskBusy[task.account_id] = 'resume'
  try {
    const data = await api(`/api/scheduler/tasks/${task.account_id}/resume`, { method: 'POST', body: '{}' })
    scheduler.value = data.scheduler || scheduler.value
    success('账号任务已继续')
	} catch (err) { showError(err.message) } finally { delete taskBusy[task.account_id] }
}

async function removeTask(task) {
  const confirmed = await confirmAction({ title: '移除账号任务', message: `移除 ${accountName(task)} 的创建任务吗？已创建的邮箱会保留。`, confirmText: '移除任务', tone: 'danger' })
  if (!confirmed) return
	taskBusy[task.account_id] = 'remove'
  try {
    const data = await api(`/api/scheduler/tasks/${task.account_id}`, { method: 'DELETE' })
    scheduler.value = data.scheduler || scheduler.value
    success('账号任务已移除')
	} catch (err) { showError(err.message) } finally { delete taskBusy[task.account_id] }
}

async function saveDefaults() {
  busy.value = 'save-defaults'
  try {
    const data = await api('/api/create-settings', { method: 'PUT', body: JSON.stringify(defaultForm) })
    assignDefaults(data.settings)
    showDefaults.value = false
    success('任务默认设置已保存')
  } catch (err) { showError(err.message) } finally { busy.value = '' }
}

onMounted(() => {
  load()
  refreshTimer = window.setInterval(refresh, 2000)
})
onBeforeUnmount(() => window.clearInterval(refreshTimer))
</script>

<template>
  <div class="mx-auto max-w-7xl space-y-5">
    <section class="panel overflow-hidden">
      <header class="flex flex-col gap-3 border-b border-slate-100 px-5 py-5 dark:border-slate-700 sm:flex-row sm:items-center sm:justify-between sm:px-6">
        <div><h2 class="text-lg font-black">账号创建任务</h2><p class="mt-1 text-sm text-slate-400">任务开启后持续运行，满额时等待，出现空位后自动补建</p></div>
        <div class="flex items-center gap-2"><button class="icon-button" title="任务默认设置" aria-label="打开任务默认设置" @click="showDefaults = true"><Settings :size="17" /></button><button class="primary-button" :disabled="!availableAccountOptions.length" @click="openCreateDialog"><Plus :size="17" />添加账号任务</button></div>
      </header>
      <div class="grid grid-cols-2 border-b border-slate-100 dark:border-slate-700 sm:grid-cols-3 xl:grid-cols-6">
        <div v-for="item in [{ label: '全部任务', value: overview.total }, { label: '创建中', value: overview.active }, { label: '等待中', value: overview.waiting }, { label: '满额等待', value: overview.capacity }, { label: '有异常', value: overview.issues }, { label: '邮箱总数', value: overview.mailboxes }]" :key="item.label" class="border-b border-r border-slate-100 px-5 py-4 last:border-r-0 dark:border-slate-700 xl:border-b-0"><span class="text-xs text-slate-400">{{ item.label }}</span><strong class="mt-1 block text-2xl text-slate-800 dark:text-slate-100">{{ item.value }}</strong></div>
      </div>
    </section>

    <div v-if="loading" class="flex min-h-64 items-center justify-center text-slate-400"><LoaderCircle :size="24" class="animate-spin" /></div>
    <section v-else class="panel overflow-hidden">
      <div class="flex items-center justify-between border-b border-slate-100 px-5 py-4 dark:border-slate-700 sm:px-6"><div><h3 class="font-black">任务列表</h3><p class="mt-1 text-xs text-slate-400">{{ tasks.length }} 个账号任务</p></div><Activity :size="18" class="text-emerald-500" /></div>
      <div v-if="!tasks.length" class="flex min-h-64 flex-col items-center justify-center gap-3 px-6 text-center text-slate-400"><Gauge :size="34" /><strong class="text-sm text-slate-600 dark:text-slate-300">暂无账号任务</strong></div>
      <div v-else class="divide-y divide-slate-100 dark:divide-slate-700">
        <article v-for="task in tasks" :key="task.account_id" class="px-5 py-5 sm:px-6">
          <div class="flex flex-col gap-4 lg:flex-row lg:items-start">
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2"><strong class="truncate text-sm text-slate-800 dark:text-slate-100">{{ accountName(task) }}</strong><span :class="statusClass(task)" class="rounded-full px-2.5 py-1 text-[10px] font-bold">{{ statusText(task) }}</span><span v-if="task.last_error" class="inline-flex items-center gap-1 text-[10px] font-bold text-amber-600"><CircleAlert :size="12" />最近有异常</span></div>
              <div class="mt-3 flex items-center gap-3"><div class="h-2 min-w-0 flex-1 overflow-hidden rounded-full bg-slate-100 dark:bg-slate-700"><div class="h-full rounded-full bg-emerald-500 transition-all" :style="{ width: `${progressPercent(task)}%` }" /></div><strong class="shrink-0 font-mono text-xs text-slate-600 dark:text-slate-300">{{ task.progress || 0 }}/{{ task.target || 750 }}</strong></div>
              <div class="mt-3 grid grid-cols-2 gap-x-5 gap-y-2 text-xs sm:grid-cols-4"><div><span class="text-slate-400">每轮</span><strong class="ml-2">{{ task.round_limit || 20 }}</strong></div><div><span class="text-slate-400">成功</span><strong class="ml-2 text-emerald-600">{{ task.success || 0 }}</strong></div><div><span class="text-slate-400">失败</span><strong class="ml-2 text-rose-500">{{ task.failed || 0 }}</strong></div><div><span class="text-slate-400">轮次间隔</span><strong class="ml-2">{{ intervalText(task.interval_seconds || 3600) }}</strong></div></div>
              <div class="mt-3 flex flex-wrap gap-x-5 gap-y-2 text-xs"><span><span class="text-slate-400">最近执行</span><strong class="ml-2">{{ formatTime(task.last_run_at) }}</strong></span><span><span class="text-slate-400">下次执行</span><strong class="ml-2">{{ task.status === 'completed' || task.status === 'paused' ? '-' : formatTime(task.next_run_at) }}</strong></span></div>
              <p v-if="task.last_error" class="mt-3 break-words rounded-lg bg-amber-50 px-3 py-2 text-xs leading-5 text-amber-700 dark:bg-amber-950/30 dark:text-amber-300">{{ task.last_error }}</p>
            </div>
            <div class="flex shrink-0 items-center gap-2 lg:pt-0.5">
				<button v-if="task.status === 'running' || task.status === 'creating' || task.status === 'waiting'" type="button" class="icon-button !h-11 !w-11" :disabled="Boolean(taskBusy[task.account_id])" title="暂停任务" aria-label="暂停任务" @click.stop="pauseTask(task)"><LoaderCircle v-if="taskBusy[task.account_id] === 'pause'" :size="17" class="animate-spin" /><Pause v-else :size="17" /></button>
				<button v-else-if="task.status === 'paused'" type="button" class="icon-button !h-11 !w-11" :disabled="Boolean(taskBusy[task.account_id])" title="继续任务" aria-label="继续任务" @click.stop="resumeTask(task)"><LoaderCircle v-if="taskBusy[task.account_id] === 'resume'" :size="17" class="animate-spin" /><Play v-else :size="17" /></button>
				<button type="button" class="icon-button !h-11 !w-11 text-rose-500" :disabled="Boolean(taskBusy[task.account_id])" title="移除任务" aria-label="移除任务" @click.stop="removeTask(task)"><LoaderCircle v-if="taskBusy[task.account_id] === 'remove'" :size="17" class="animate-spin" /><Trash2 v-else :size="17" /></button>
            </div>
          </div>
        </article>
      </div>
    </section>

    <Teleport to="body">
      <div v-if="showCreate" class="fixed inset-0 z-[70] !m-0 flex items-center justify-center bg-slate-950/55 p-4 backdrop-blur-[3px]" role="presentation" @click.stop>
        <form class="panel max-h-[calc(100vh-2rem)] w-full max-w-2xl overflow-y-auto p-5 shadow-2xl sm:p-6" role="dialog" aria-modal="true" aria-labelledby="task-create-title" @submit.prevent="createTask">
          <div class="flex items-start justify-between gap-4 border-b border-slate-100 pb-4 dark:border-slate-700"><div><h2 id="task-create-title" class="text-lg font-black">添加账号任务</h2><p class="mt-1 text-xs text-slate-400">首次执行前同步已有邮箱数量</p></div><button type="button" class="icon-button" title="关闭" @click="showCreate = false"><X :size="17" /></button></div>
          <div class="mt-5 grid gap-4 sm:grid-cols-2">
            <div class="form-group sm:col-span-2"><span class="form-label">Apple 账号</span><CardSelect v-model="taskForm.account_id" :options="availableAccountOptions" aria-label="任务 Apple 账号" /></div>
            <div class="form-group"><span class="form-label">创建通道</span><CardSelect v-model="taskForm.create_channel" :options="createChannelOptions" aria-label="任务创建通道" /></div>
            <label class="form-group"><span class="form-label">标签前缀</span><input v-model.trim="taskForm.label" class="field" placeholder="可选" /></label>
            <label class="form-group"><span class="form-label">下一轮间隔（分钟）</span><input v-model.number="taskForm.interval_minutes" class="field" type="number" min="1" max="10080" /></label>
            <label class="form-group"><span class="form-label">单次创建间隔（秒）</span><input v-model.number="taskForm.round_interval_seconds" class="field" type="number" min="1" max="3600" /></label>
            <label class="form-group"><span class="form-label">每轮创建个数</span><input v-model.number="taskForm.round_limit" class="field" type="number" min="1" :max="taskForm.target || 750" /><span class="form-help">每轮最多创建多少个，不能超过目标总数。</span></label>
            <label class="form-group"><span class="form-label">账号邮箱目标总数</span><input v-model.number="taskForm.target" class="field" type="number" min="1" max="750" /><span class="form-help">包含同步到的已有邮箱；达到目标后持续巡检，删除隐私邮箱空出名额时自动补足。</span></label>
            <label class="form-group sm:col-span-2"><span class="form-label">备注</span><input v-model.trim="taskForm.note" class="field" placeholder="可选" /></label>
          </div>
          <div class="mt-6 flex justify-end gap-2"><button type="button" class="secondary-button" @click="showCreate = false">取消</button><button class="primary-button" :disabled="busy || !taskForm.account_id"><LoaderCircle v-if="busy.startsWith('start:')" :size="16" class="animate-spin" /><Play v-else :size="16" />启动任务</button></div>
        </form>
      </div>

      <div v-if="showDefaults" class="fixed inset-0 z-[70] !m-0 flex items-center justify-center bg-slate-950/55 p-4 backdrop-blur-[3px]" role="presentation" @click.stop>
        <form class="panel max-h-[calc(100vh-2rem)] w-full max-w-xl overflow-y-auto p-5 shadow-2xl sm:p-6" role="dialog" aria-modal="true" aria-labelledby="task-defaults-title" @submit.prevent="saveDefaults">
          <div class="flex items-start justify-between gap-4 border-b border-slate-100 pb-4 dark:border-slate-700"><div><h2 id="task-defaults-title" class="text-lg font-black">任务默认设置</h2></div><button type="button" class="icon-button" title="关闭" @click="showDefaults = false"><X :size="17" /></button></div>
          <div class="mt-5 grid gap-4 sm:grid-cols-2">
            <div class="form-group sm:col-span-2"><span class="form-label">默认创建通道</span><CardSelect v-model="defaultForm.scheduler_create_channel" :options="createChannelOptions" aria-label="默认创建通道" /></div>
            <label class="form-group"><span class="form-label">下一轮间隔（分钟）</span><input v-model.number="defaultForm.scheduler_interval_minutes" class="field" type="number" min="1" max="10080" /></label>
            <label class="form-group"><span class="form-label">单次创建间隔（秒）</span><input v-model.number="defaultForm.scheduler_round_interval_seconds" class="field" type="number" min="1" max="3600" /></label>
            <label class="form-group"><span class="form-label">默认每轮创建个数</span><input v-model.number="defaultForm.scheduler_round_limit" class="field" type="number" min="1" :max="defaultForm.scheduler_target || 750" /></label>
            <label class="form-group"><span class="form-label">默认账号目标总数</span><input v-model.number="defaultForm.scheduler_target" class="field" type="number" min="1" max="750" /></label>
            <label class="form-group"><span class="form-label">默认标签前缀</span><input v-model.trim="defaultForm.label" class="field" placeholder="可选" /></label>
            <label class="form-group"><span class="form-label">默认备注</span><input v-model.trim="defaultForm.note" class="field" placeholder="可选" /></label>
          </div>
          <div class="mt-6 flex justify-end gap-2"><button type="button" class="secondary-button" @click="showDefaults = false">取消</button><button class="primary-button" :disabled="busy === 'save-defaults'"><LoaderCircle v-if="busy === 'save-defaults'" :size="16" class="animate-spin" />保存</button></div>
        </form>
      </div>
    </Teleport>
  </div>
</template>
