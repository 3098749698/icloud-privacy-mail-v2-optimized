import test from 'node:test'
import assert from 'node:assert/strict'
import { autoDeleteReadinessProblem, exportFormProblem } from './exportEligibility.js'

test('permanent exports do not require a selected account', () => {
  assert.equal(exportFormProblem({ availableCount: 10, exportCount: 5, validDays: 0, accountID: '' }), '')
})

test('expiring exports require a selected account', () => {
  assert.match(
    exportFormProblem({ availableCount: 10, exportCount: 5, validDays: 7, accountID: '' }),
    /选择一个 Apple 账号/,
  )
  assert.equal(exportFormProblem({ availableCount: 10, exportCount: 5, validDays: 7, accountID: 'acc_1' }), '')
})

test('accounts without a remote delete state get an actionable expiry warning', () => {
  const message = autoDeleteReadinessProblem({
    auto_delete_ready: false,
    login_states: [{ kind: 'apple_account', saved: true }],
  }, 7)
  assert.match(message, /重新完成协议登录/)
  assert.equal(autoDeleteReadinessProblem({ auto_delete_ready: true }, 7), '')
  assert.equal(autoDeleteReadinessProblem({ auto_delete_ready: false }, 0), '')
})
