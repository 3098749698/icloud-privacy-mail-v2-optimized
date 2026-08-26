export class ApiError extends Error {
  constructor(message, status = 0, code = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

export async function api(path, options = {}) {
  const {
    timeoutMs = 0,
    signal: callerSignal,
    ...fetchOptions
  } = options
  const headers = new Headers(fetchOptions.headers || {})
  if (options.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  const requestedTimeout = Number(timeoutMs)
  const hasTimeout = Number.isFinite(requestedTimeout) && requestedTimeout > 0
  const controller = hasTimeout ? new AbortController() : null
  let timedOut = false
  let timeoutID
  const abortFromCaller = () => controller?.abort()
  if (controller) {
    if (callerSignal?.aborted) controller.abort()
    else callerSignal?.addEventListener('abort', abortFromCaller, { once: true })
    timeoutID = globalThis.setTimeout(() => {
      timedOut = true
      controller.abort()
    }, requestedTimeout)
  }
  const requestOptions = {
    credentials: 'same-origin',
    cache: path.startsWith('/api/') ? 'no-store' : 'default',
    ...fetchOptions,
    headers,
  }
  if (controller) requestOptions.signal = controller.signal
  else if (callerSignal) requestOptions.signal = callerSignal
  try {
    const response = await fetch(path, requestOptions)
    let payload = null
    try {
      payload = await response.json()
    } catch (err) {
      if (err?.name === 'AbortError') {
        throw err
      }
      throw new ApiError(`服务器返回了无效响应（HTTP ${response.status}）`, response.status)
    }
    if (!response.ok || payload?.success === false) {
	  if (response.status === 401 && path !== '/api/auth/login' && path !== '/api/auth/setup' && !path.startsWith('/api/v1/')) {
	    const redirect = `${window.location.pathname}${window.location.search}`
	    window.location.replace(`/login?redirect=${encodeURIComponent(redirect)}`)
	  }
      throw new ApiError(payload?.message || `请求失败（HTTP ${response.status}）`, response.status, payload?.code || '')
    }
    const data = payload?.data ?? payload
    // Keep optional response diagnostics available without changing the existing
    // data-first API shape. Callers that do not use meta continue to behave as
    // before, while code lookups can show cache/sync provenance and timings.
    if (payload?.meta && data && typeof data === 'object' && !Array.isArray(data)) {
      data.meta = payload.meta
    }
    return data
  } catch (err) {
    if (err?.name === 'AbortError') {
      throw new ApiError(timedOut ? '请求超时，请重试' : '请求已取消', 0, timedOut ? 'timeout' : 'request_cancelled')
    }
    throw err
  } finally {
    if (timeoutID) globalThis.clearTimeout(timeoutID)
    callerSignal?.removeEventListener('abort', abortFromCaller)
  }
}
