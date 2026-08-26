export function exportFormProblem({ availableCount, exportCount, validDays, accountID }) {
  const count = Number(exportCount)
  const days = Number(validDays)

  if (Number(availableCount) <= 0) return '当前没有可导出的邮箱'
  if (!Number.isInteger(count) || count <= 0 || count > 10000) return '导出数量范围为 1 到 10000'
  if (!Number.isInteger(days) || days < 0 || days > 3650) return '有效天数范围为 0 到 3650'
  if (days > 0 && !String(accountID || '').trim()) return '限时导出需要选择一个 Apple 账号'
  return ''
}

export function autoDeleteReadinessProblem(account, validDays) {
  if (Number(validDays) <= 0 || !account || account.auto_delete_ready) return ''
  return '所选账号当前缺少可用的 Apple 远端删除登录态，请重新完成协议登录。'
}
