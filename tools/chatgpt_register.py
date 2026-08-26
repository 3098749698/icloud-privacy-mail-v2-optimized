#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""独立 ChatGPT 协议注册程序（脱离轻量注册AI项目，自包含）。

依赖：
  - pip install curl_cffi
  - Node.js（用于运行同目录的 sentinel-runtime.mjs 解 Sentinel 挑战）

输入：邮箱池文件，每行 "邮箱----收码URL"
  收码URL 形如 https://ipm2.tiedan888.top/api/v1/mailboxes/{email}/code?key=...&wait_ms=30000&preview=1

产出：注册出的账号（邮箱 + 随机强密码 + TOTP secret + session/accessToken）。
"""

import base64
import hashlib
import hmac
import json
import os
import re
import secrets
import string
import struct
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone
from html.parser import HTMLParser
from pathlib import Path
from typing import Any
from urllib.parse import parse_qsl, urlencode, urljoin, urlparse, urlsplit, urlunsplit

from curl_cffi import requests


# ---------------- 常量 ----------------
CHATGPT = "https://chatgpt.com"
AUTH = "https://auth.openai.com"
SENTINEL = "https://sentinel.openai.com"
SENTINEL_LOADER = f"{SENTINEL}/backend-api/sentinel/sdk.js"
SENTINEL_REQUEST = f"{SENTINEL}/backend-api/sentinel/req"

CHATGPT_CURL_IMPERSONATE = os.environ.get("CHATGPT_CURL_IMPERSONATE", "chrome142")
ACTIVE_CURL_IMPERSONATE = CHATGPT_CURL_IMPERSONATE
ACTIVE_LOCALE = "ja-JP"
USER_AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"
SEC_CH_UA = '"Chromium";v="142", "Google Chrome";v="142", "Not_A Brand";v="99"'
SEC_CH_PLATFORM = '"macOS"'
ACTIVE_USER_AGENT = USER_AGENT
ACTIVE_PLATFORM = "MacIntel"
ACTIVE_SEC_CH_PLATFORM = SEC_CH_PLATFORM
ACTIVE_LANGUAGES = [ACTIVE_LOCALE, "ja"]
PASSKEY_CLIENT_CAPABILITIES = "11111"
CLIENT_CAPABILITIES = "login_methods"
SIGNUP_SCREEN_HINT = "signup"
CONNECT_TIMEOUT_SECONDS = 12
REQUEST_TIMEOUT = (CONNECT_TIMEOUT_SECONDS, 45)
ENTRY_TIMEOUT = (CONNECT_TIMEOUT_SECONDS, 30)
REGISTRATION_OTP_WAIT_MS = 120_000
OTP_SUBMIT_ATTEMPTS = 4
TRANSIENT_HTTP_STATUSES = {408, 425, 429, 500, 502, 503, 504}
TRANSPORT_RETRY_ATTEMPTS = 3
TIMEZONE_OFFSET_MIN = int(os.environ.get("TIMEZONE_OFFSET_MIN", "-480"))  # 东八区，JS getTimezoneOffset 语义取负

HERE = Path(__file__).resolve().parent
SENTINEL_RUNTIME = HERE / "sentinel-runtime.mjs"

TRANSIENT_TRANSPORT_PATTERN = re.compile(
    r"curl:\s*\((?:5|6|7|18|28|35|52|55|56|92)\)|"
    r"timed?\s*out|ssl|tls|unexpected\s+eof|recv\s+failure|send\s+failure|"
    r"connection[^:]{0,40}(?:closed|reset|aborted|refused)|"
    r"proxy[^:]{0,40}(?:connect|tunnel)",
    re.I,
)


def log(msg: str) -> None:
    print(msg, flush=True)


def short_error(error: Exception) -> str:
    return re.sub(r"\s+", " ", f"{type(error).__name__}: {error}")[:400]


def safe_location(value: str) -> str:
    return value.split("?", 1)[0].split("#", 1)[0]


# ---------------- 浏览器指纹 headers ----------------
def browser_headers() -> dict[str, str]:
    major = re.search(r"(?:Chrome|Chromium)/(\d+)", ACTIVE_USER_AGENT)
    if not major:
        major = re.search(r"(\d+)$", ACTIVE_CURL_IMPERSONATE)
    chrome_major = major.group(1) if major else "142"
    client_hints = SEC_CH_UA.replace('v="142"', f'v="{chrome_major}"')
    return {
        "accept": "*/*",
        "accept-language": f"{ACTIVE_LOCALE},{ACTIVE_LOCALE.split('-', 1)[0]};q=0.9",
        "sec-ch-ua": client_hints,
        "sec-ch-ua-mobile": "?0",
        "sec-ch-ua-platform": ACTIVE_SEC_CH_PLATFORM,
        "user-agent": ACTIVE_USER_AGENT,
    }


def navigation_headers(referer: str = "") -> dict[str, str]:
    headers = {
        **browser_headers(),
        "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
        "sec-fetch-dest": "document",
        "sec-fetch-mode": "navigate",
        "upgrade-insecure-requests": "1",
    }
    if referer:
        headers["referer"] = referer
    return headers


def api_headers(referer: str) -> dict[str, str]:
    return {
        **browser_headers(),
        "accept": "application/json",
        "referer": referer,
        "sec-fetch-dest": "empty",
        "sec-fetch-mode": "cors",
    }


def backend_api_headers(path: str, *, referer: str, access_token: str = "", device_id: str = "", content_type: bool = False) -> dict[str, str]:
    target = str(path or "").split("?", 1)[0]
    headers = {
        **browser_headers(),
        "accept": "*/*",
        "referer": referer,
        "sec-fetch-dest": "empty",
        "sec-fetch-mode": "cors",
        "sec-fetch-site": "same-origin",
        "oai-language": ACTIVE_LOCALE,
        "x-openai-target-path": target,
        "x-openai-target-route": target,
    }
    if access_token:
        headers["authorization"] = f"Bearer {access_token}"
    if device_id:
        headers["oai-device-id"] = device_id
    if content_type:
        headers["content-type"] = "application/json"
    return headers


def sentinel_headers(referer: str) -> dict[str, str]:
    return {**browser_headers(), "referer": referer, "sec-fetch-dest": "empty", "sec-fetch-mode": "cors"}


def protected_headers(referer: str, token: str, so_token: str | None) -> dict[str, str]:
    headers = {
        **api_headers(referer),
        "content-type": "application/json",
        "origin": AUTH,
        "openai-sentinel-token": token,
        "x-access-flow-invocation-id": str(uuid.uuid4()),
    }
    if so_token:
        headers["openai-sentinel-so-token"] = so_token
    return headers


# ---------------- 通用工具 ----------------
def generate_password(length: int = 18) -> str:
    alphabet = string.ascii_letters + string.digits + "!@#$%"
    while True:
        value = "".join(secrets.choice(alphabet) for _ in range(length))
        if all((re.search(pattern, value) for pattern in (r"[a-z]", r"[A-Z]", r"\d", r"[!@#$%]"))):
            return value


def generate_totp(secret: str, interval: int = 30) -> str:
    normalized = re.sub(r"\s+", "", secret).upper()
    normalized += "=" * ((8 - len(normalized) % 8) % 8)
    key = base64.b32decode(normalized, casefold=True)
    counter = int(time.time() // interval)
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0F
    value = struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF
    return str(value % 1_000_000).zfill(6)


def response_payload(response: Any) -> Any:
    try:
        return response.json()
    except Exception:
        return {}


def find_url(payload: Any, key: str) -> str:
    if isinstance(payload, dict):
        direct = payload.get(key)
        if isinstance(direct, str) and direct.startswith("http"):
            return direct
        for value in payload.values():
            found = find_url(value, key)
            if found:
                return found
    elif isinstance(payload, list):
        for value in payload:
            found = find_url(value, key)
            if found:
                return found
    return ""


def response_location(response: Any) -> str:
    return safe_location(str(response.url or ""))


def response_error_code(response: Any) -> str:
    headers = response.headers if getattr(response, "headers", None) is not None else {}
    ide_error = str(headers.get("x-openai-ide-error-code", "")).strip()
    if ide_error:
        return ide_error
    try:
        payload = response.json()
        error = payload.get("error") if isinstance(payload, dict) else None
        if isinstance(error, dict):
            code = str(error.get("code") or error.get("type") or "").strip()
            if code:
                return code
    except Exception:
        pass
    authorization_error = str(headers.get("x-openai-authorization-error", "")).strip()
    if authorization_error:
        return authorization_error
    return f"HTTP {response.status_code}"


def require_ok(response: Any, label: str) -> None:
    if 200 <= int(response.status_code) < 400:
        return
    text = re.sub(r"\s+", " ", str(response.text or ""))[:500]
    raise RuntimeError(f"{label} HTTP {response.status_code}: {text}")


def is_transient_transport_error(error: Exception) -> bool:
    return bool(TRANSIENT_TRANSPORT_PATTERN.search(short_error(error)))


def is_transient_protocol_failure(error: Exception) -> bool:
    return is_transient_transport_error(error) or bool(
        re.search(r"\bHTTP\s+(?:408|425|429|500|502|503|504)\b", short_error(error), re.I)
    )


def request_with_transport_retry(operation: Any, label: str, attempts: int = TRANSPORT_RETRY_ATTEMPTS) -> Any:
    total = max(1, int(attempts))
    last_response = None
    for attempt in range(1, total + 1):
        reason = ""
        try:
            response = operation()
            last_response = response
            status = int(response.status_code)
            if status not in TRANSIENT_HTTP_STATUSES or attempt >= total:
                return response
            reason = f"HTTP {status}"
        except Exception as error:
            if attempt >= total or not is_transient_transport_error(error):
                raise
            reason = short_error(error)
        log(f"  [重试] {label}; attempt={attempt + 1}/{total}; {reason}")
        time.sleep(min(0.75 * attempt, 2.0))
    return last_response


def get_navigation_with_retry(session, url, headers, label, timeout=(10, 30), attempts: int = 3) -> Any:
    last_response = None
    last_error = ""
    retryable_statuses = {403, 408, 425, 429, 500, 502, 503, 504}
    for attempt in range(1, max(1, attempts) + 1):
        try:
            response = session.get(url, headers=headers, allow_redirects=True, timeout=timeout)
            last_response = response
            if 200 <= int(response.status_code) < 400:
                return response
            last_error = f"HTTP {response.status_code}"
            if int(response.status_code) not in retryable_statuses:
                require_ok(response, label)
        except Exception as error:
            last_error = short_error(error)
            if attempt >= attempts:
                raise
        if attempt < attempts:
            log(f"  [导航重试] {label}; attempt={attempt + 1}/{attempts}; {last_error}")
            time.sleep(min(1.0 * attempt, 2.0))
    if last_response is not None:
        require_ok(last_response, label)
    raise RuntimeError(f"{label} failed after {attempts} attempts: {last_error}")


def account_id_from_session(session_payload: dict[str, Any], access_token: str) -> str | None:
    account = session_payload.get("account") if isinstance(session_payload.get("account"), dict) else {}
    direct = account.get("id") or session_payload.get("accountId")
    if direct:
        return str(direct)
    try:
        payload = access_token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        claims = json.loads(base64.urlsafe_b64decode(payload).decode("utf-8"))
        auth = claims.get("https://api.openai.com/auth") or {}
        return str(auth.get("chatgpt_account_id") or auth.get("user_id") or claims.get("sub") or "") or None
    except Exception:
        user = session_payload.get("user") if isinstance(session_payload.get("user"), dict) else {}
        return str(user.get("id") or "") or None


def detect_geo(session) -> dict[str, str]:
    geo: dict[str, str] = {}
    try:
        r = session.get("https://ipapi.co/json/", timeout=(3, 5), headers={"accept": "application/json"})
        if r.ok:
            payload = r.json()
            for target, source in (("country", "country_code"), ("city", "city"), ("region", "region"),
                                   ("latitude", "latitude"), ("longitude", "longitude"), ("timezone", "timezone")):
                value = payload.get(source)
                if value not in (None, ""):
                    geo[target] = str(value)
    except Exception:
        pass
    if not geo:
        geo = {"country": "JP", "city": "Tokyo", "region": "Tokyo",
               "latitude": "35.6762", "longitude": "139.6503", "timezone": "Asia/Tokyo"}
    return geo


# ---------------- Sentinel 本地运行时桥接 ----------------
class RuntimeBridge:
    def __init__(self, session, device_id: str, geo: dict[str, str]):
        self.session = session
        self.device_id = device_id
        self.geo = geo
        self.process: subprocess.Popen[str] | None = None
        self.initial_requirements = ""

    def _load_sdk(self) -> tuple[str, str]:
        loader = request_with_transport_retry(
            lambda: self.session.get(SENTINEL_LOADER, headers=sentinel_headers(f"{AUTH}/"), timeout=ENTRY_TIMEOUT),
            "Sentinel loader",
        )
        require_ok(loader, "Sentinel loader")
        match = re.search(r"script\.src\s*=\s*['\"]([^'\"]+/sdk\.js)['\"]", loader.text)
        if not match:
            raise RuntimeError("Sentinel loader did not expose a versioned SDK")
        sdk_url = match.group(1)
        sdk_response = request_with_transport_retry(
            lambda: self.session.get(sdk_url, headers=sentinel_headers(f"{AUTH}/"), timeout=ENTRY_TIMEOUT),
            "Sentinel SDK",
        )
        require_ok(sdk_response, "Sentinel SDK")
        return sdk_url, str(sdk_response.text or "")

    def start(self, page_url: str = "") -> None:
        if not SENTINEL_RUNTIME.exists():
            raise RuntimeError(f"缺少 Sentinel 运行时: {SENTINEL_RUNTIME}")
        sdk_url, sdk_source = self._load_sdk()
        self.process = subprocess.Popen(
            ["node", str(SENTINEL_RUNTIME)],
            cwd=str(HERE),
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            bufsize=1,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
        )
        init = self.call({
            "type": "init",
            "sdkUrl": sdk_url,
            "sdkSource": sdk_source,
            "deviceId": self.device_id,
            "pageUrl": page_url or f"{AUTH}/email-verification",
            "userAgent": ACTIVE_USER_AGENT,
            "platform": ACTIVE_PLATFORM,
            "language": ACTIVE_LOCALE,
            "languages": ACTIVE_LANGUAGES,
            "cores": 8,
            "width": 1920,
            "height": 1080,
            "timezone": self.geo.get("timezone") or "Asia/Tokyo",
            "cfIpCountry": self.geo.get("country") or "JP",
            "cfIpCity": self.geo.get("city") or "Tokyo",
            "cfIpRegion": self.geo.get("region") or "Tokyo",
            "cfIpLatitude": self.geo.get("latitude") or "35.6762",
            "cfIpLongitude": self.geo.get("longitude") or "139.6503",
        })
        self.initial_requirements = str(init.get("p") or "")
        if not self.initial_requirements:
            raise RuntimeError("Sentinel requirements token is empty")
        log(f"  [Sentinel] ready sdk={sdk_url.rsplit('/', 2)[-2]} geo={self.geo.get('country', '?')}")

    def solve(self, flow: str) -> tuple[str, str | None]:
        requirements = self.call({"type": "requirements"})
        p = str(requirements.get("p") or self.initial_requirements)
        response = request_with_transport_retry(
            lambda: self.session.post(
                SENTINEL_REQUEST,
                data=json.dumps({"p": p, "id": self.device_id, "flow": flow}, separators=(",", ":")),
                headers={
                    **sentinel_headers(f"{SENTINEL}/backend-api/sentinel/frame.html"),
                    "content-type": "text/plain;charset=UTF-8",
                    "origin": SENTINEL,
                },
                timeout=ENTRY_TIMEOUT,
            ),
            f"Sentinel challenge {flow}",
        )
        require_ok(response, f"Sentinel challenge {flow}")
        challenge = response.json()
        solved = self.call({"type": "solve", "challenge": challenge, "flow": flow})
        fields = {name: int(value) for name, value in dict(solved.get("fields") or {}).items()}
        if fields.get("t", 0) < 500 or fields.get("c", 0) < 500:
            raise RuntimeError(f"Sentinel generated an invalid token shape for {flow}: {fields}")
        token = str(solved.get("token") or "")
        so_token = str(solved.get("soToken") or "") or None
        log(f"  [Sentinel] solved flow={flow} p={fields.get('p', 0)} t={fields.get('t', 0)} so={fields.get('so', 0)}")
        return token, so_token

    def call(self, payload: dict[str, Any]) -> dict[str, Any]:
        if not self.process or not self.process.stdin or not self.process.stdout:
            raise RuntimeError("Sentinel runtime is not running")
        try:
            self.process.stdin.write(json.dumps(payload, separators=(",", ":")) + "\n")
            self.process.stdin.flush()
        except OSError as error:
            stderr = self.process.stderr.read()[-800:] if self.process.stderr else ""
            raise RuntimeError(f"Sentinel runtime input failed: {error}; {stderr}") from error
        line = self.process.stdout.readline()
        if not line:
            stderr = self.process.stderr.read()[-800:] if self.process.stderr else ""
            raise RuntimeError(f"Sentinel runtime exited without a response: {stderr}")
        result = json.loads(line)
        if result.get("ok") is not True:
            raise RuntimeError(str(result.get("error") or "Sentinel runtime error"))
        return result

    def close(self) -> None:
        if self.process and self.process.poll() is None:
            self.process.kill()
        self.process = None


# ---------------- 接码（icloud 收码 URL） ----------------
class _SignupPasswordLinkParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.href = ""

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if self.href or tag.lower() != "a":
            return
        values = {str(k).lower(): str(v or "") for k, v in attrs}
        href = values.get("href", "")
        if "/create-account/password" in href:
            self.href = href


def signup_password_url(html: str, page_url: str) -> str:
    parser = _SignupPasswordLinkParser()
    try:
        parser.feed(str(html or ""))
    except Exception:
        return ""
    return urljoin(page_url, parser.href) if parser.href else ""


def mailbox_url_after(mailbox_url: str, requested_at: float) -> str:
    parts = urlsplit(mailbox_url)
    query = dict(parse_qsl(parts.query, keep_blank_values=True))
    after = datetime.fromtimestamp(requested_at - 30.0, timezone.utc).isoformat().replace("+00:00", "Z")
    query["after"] = after
    query.setdefault("wait_ms", "30000")
    query.setdefault("preview", "1")
    return urlunsplit((parts.scheme, parts.netloc, parts.path, urlencode(query), parts.fragment))


def poll_code(mailbox_url: str, email: str, max_wait_s: float, requested_at: float) -> tuple[str, dict[str, Any]]:
    deadline = time.monotonic() + max_wait_s
    last_raw = ""
    transport_failures = 0
    request_url = mailbox_url_after(mailbox_url, requested_at)
    while time.monotonic() < deadline:
        request_started = time.time()
        request_monotonic = time.monotonic()
        retry_delay = 0.0
        try:
            r = requests.get(request_url, headers={"accept": "application/json"}, timeout=(10, 40))
            request_elapsed = time.monotonic() - request_monotonic
            if r.status_code in (200, 201):
                data = r.json()
                # 成功响应把 code 嵌在 data.code；no_code 响应用顶层 code
                raw = data.get("code") if isinstance(data, dict) else None
                inner = data.get("data") if isinstance(data, dict) else None
                if isinstance(inner, dict):
                    raw = inner.get("code") or raw
                code = re.sub(r"\D", "", str(raw or ""))
                last_raw = str(raw or "")
                if len(code) == 6:
                    meta = data.get("meta") if isinstance(data.get("meta"), dict) else {}
                    delivery = inner if isinstance(inner, dict) else {}
                    return code, {
                        "request_started_at": datetime.fromtimestamp(request_started, timezone.utc).isoformat(),
                        "client_received_at": datetime.now(timezone.utc).isoformat(),
                        "client_request_ms": round(request_elapsed * 1000),
                        "folder": delivery.get("folder") or meta.get("folder"),
                        "message_date_at": delivery.get("message_date_at") or delivery.get("received_at"),
                        "provider_received_at": delivery.get("provider_received_at"),
                        "detected_at": delivery.get("detected_at"),
                        "api_returned_at": delivery.get("api_returned_at"),
                        "provider_to_detect_ms": meta.get("provider_to_detect_ms"),
                        "detect_to_response_ms": meta.get("detect_to_response_ms"),
                        "queue_wait_ms": meta.get("queue_wait_ms"),
                        "fetch_mode": meta.get("fetch_mode"),
                        "idle_ready": meta.get("idle_ready"),
                    }
                retry_after_ms = data.get("retry_after_ms") if isinstance(data, dict) else None
                # A completed long poll should reconnect immediately. Fast
                # no-code responses honor the server backoff to avoid hammering.
                if request_elapsed < 5.0:
                    try:
                        retry_delay = min(max(float(retry_after_ms or 250) / 1000.0, 0.1), 2.0)
                    except (TypeError, ValueError):
                        retry_delay = 0.25
                transport_failures = 0
        except Exception as e:
            log(f"    [收码] {type(e).__name__}")
            transport_failures += 1
            retry_delay = min(0.5 * (2 ** (transport_failures - 1)), 3.0)
        remaining = deadline - time.monotonic()
        if retry_delay > 0 and remaining > 0:
            time.sleep(min(retry_delay, remaining))
    log(f"    [收码] 超时，最后一次原始响应: {last_raw[:200]}")
    return "", {}


# ---------------- 注册主流程 ----------------
def signup_auth_url(session, email: str, device_id: str, logging_id: str) -> str:
    csrf_response = request_with_transport_retry(
        lambda: session.get(
            f"{CHATGPT}/api/auth/csrf",
            headers=api_headers(f"{CHATGPT}/auth/login"),
            timeout=ENTRY_TIMEOUT,
        ),
        "ChatGPT CSRF",
    )
    require_ok(csrf_response, "ChatGPT CSRF")
    csrf = str(response_payload(csrf_response).get("csrfToken") or "")
    if not csrf:
        # 直接 CSRF 失败则预热一次
        get_navigation_with_retry(session, f"{CHATGPT}/", navigation_headers(), "ChatGPT homepage", timeout=ENTRY_TIMEOUT)
        get_navigation_with_retry(session, f"{CHATGPT}/auth/login", navigation_headers(f"{CHATGPT}/"), "ChatGPT login", timeout=ENTRY_TIMEOUT)
        csrf_response = request_with_transport_retry(
            lambda: session.get(f"{CHATGPT}/api/auth/csrf", headers=api_headers(f"{CHATGPT}/auth/login"), timeout=ENTRY_TIMEOUT),
            "ChatGPT CSRF after warmup",
        )
        require_ok(csrf_response, "ChatGPT CSRF after warmup")
        csrf = str(response_payload(csrf_response).get("csrfToken") or "")
    if not csrf:
        raise RuntimeError("ChatGPT CSRF response did not contain csrfToken")

    params = {
        "prompt": "login",
        "ext-oai-did": device_id,
        "auth_session_logging_id": logging_id,
        "ext-passkey-client-capabilities": PASSKEY_CLIENT_CAPABILITIES,
        "screen_hint": SIGNUP_SCREEN_HINT,
        "login_hint": email,
        "ccaps": CLIENT_CAPABILITIES,
    }
    signin = request_with_transport_retry(
        lambda: session.post(
            f"{CHATGPT}/api/auth/signin/openai",
            params=params,
            data={"callbackUrl": f"{CHATGPT}/", "csrfToken": csrf, "json": "true"},
            headers={**api_headers(f"{CHATGPT}/auth/login"), "content-type": "application/x-www-form-urlencoded"},
            timeout=ENTRY_TIMEOUT,
        ),
        "ChatGPT sign-in bootstrap",
    )
    require_ok(signin, "ChatGPT sign-in bootstrap")
    auth_url = str(response_payload(signin).get("url") or "")
    if not auth_url.startswith(AUTH):
        raise RuntimeError("ChatGPT sign-in bootstrap did not return an OpenAI authorize URL")
    return auth_url


def refresh_session(session) -> dict[str, Any]:
    for attempt in range(1, 4):
        response = request_with_transport_retry(
            lambda: session.get(
                f"{CHATGPT}/api/auth/session?update={int(time.time() * 1000)}",
                headers=api_headers(f"{CHATGPT}/"),
                timeout=(10, 30),
            ),
            "ChatGPT session refresh",
        )
        if not (200 <= int(response.status_code) < 400):
            code = response_error_code(response)
            text = re.sub(r"\s+", " ", str(response.text or ""))[:500]
            raise RuntimeError(f"ChatGPT session refresh {code}: {text}")
        payload = response.json()
        if payload.get("accessToken"):
            return payload
        if attempt < 3:
            log(f"  [session] accessToken 尚未可见 attempt={attempt + 1}/3")
            time.sleep(min(float(attempt), 2.0))
    raise RuntimeError("refreshed ChatGPT session did not contain accessToken")


def setup_mfa(session, device_id: str, access_token: str) -> dict[str, Any]:
    path = "/backend-api/accounts/mfa/enroll"
    enroll = session.post(
        f"{CHATGPT}{path}",
        json={"factor_type": "totp"},
        headers=backend_api_headers(path, referer=f"{CHATGPT}/", access_token=access_token, device_id=device_id, content_type=True),
        timeout=(10, 30),
    )
    require_ok(enroll, "MFA enroll")
    payload = enroll.json()
    secret = str(payload.get("secret") or "")
    session_id = str(payload.get("session_id") or "")
    if not secret or not session_id:
        raise RuntimeError("MFA enroll response did not contain secret/session_id")
    log(f"  [MFA] enrolled secret={secret[:8]}...")

    activate_path = "/backend-api/accounts/mfa/user/activate_enrollment"
    activate_headers = backend_api_headers(
        activate_path, referer=f"{CHATGPT}/", access_token=access_token, device_id=device_id, content_type=True,
    )
    activate = None
    for attempt in range(1, 3):
        activate = session.post(
            f"{CHATGPT}{activate_path}",
            json={"code": generate_totp(secret), "factor_type": "totp", "session_id": session_id},
            headers=activate_headers,
            timeout=(10, 30),
        )
        if 200 <= int(activate.status_code) < 400:
            break
        if attempt < 2:
            remaining = 30 - (int(time.time()) % 30)
            time.sleep(min(remaining + 0.5, 30.5))
    if activate is None:
        raise RuntimeError("MFA activation did not run")
    require_ok(activate, "MFA activation")
    activated = activate.json()
    if activated.get("success") is not True:
        raise RuntimeError(f"MFA activation returned success=false: {str(activated)[:400]}")
    log("  [MFA] activated")
    return {"enabled": True, "factorType": "totp", "totpSecret": secret, "sessionId": session_id}


def check_plan(session, device_id: str, access_token: str) -> dict[str, Any]:
    """查询套餐/试用资格（0元升级Plus或半价升级）。"""
    path = f"/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min={TIMEZONE_OFFSET_MIN}"
    r = session.get(
        f"{CHATGPT}{path}",
        headers=backend_api_headers(path, referer=f"{CHATGPT}/", access_token=access_token, device_id=device_id),
        timeout=(10, 30),
    )
    require_ok(r, "Plan check")
    payload = r.json()
    accounts = payload.get("accounts") if isinstance(payload.get("accounts"), dict) else {}

    summary: dict[str, Any] = {"raw": payload}
    for account_id, node in accounts.items():
        if not isinstance(node, dict):
            continue
        account = node.get("account") if isinstance(node.get("account"), dict) else {}
        entitlement = node.get("entitlement") if isinstance(node.get("entitlement"), dict) else {}
        plan_type = str(account.get("plan_type") or "")
        subscription_plan = str(entitlement.get("subscription_plan") or "")
        has_active = bool(entitlement.get("has_active_subscription"))
        is_gratis = bool(entitlement.get("is_active_subscription_gratis"))

        eligible_promo = node.get("eligible_promo_campaigns") if isinstance(node.get("eligible_promo_campaigns"), dict) else {}
        plus_campaign = eligible_promo.get("plus") if isinstance(eligible_promo.get("plus"), dict) else {}
        offers = node.get("eligible_offers") if isinstance(node.get("eligible_offers"), dict) else {}
        offer_ids = [str(o.get("id")) for o in offers.get("offers", []) if isinstance(o, dict) and o.get("id")]

        is_free = plan_type == "free" or subscription_plan.lower() in ("chatgptfreeplan",)
        plus_trial_eligible = is_free and bool(plus_campaign)

        discount: dict[str, Any] = {}
        if isinstance(plus_campaign.get("metadata"), dict):
            meta = plus_campaign["metadata"]
            discount = {
                "title": str(meta.get("title") or ""),
                "summary": str(meta.get("summary") or ""),
                "percentage": (meta.get("discount") or {}).get("percentage"),
                "periods": (meta.get("duration") or {}).get("num_periods"),
                "period": (meta.get("duration") or {}).get("period"),
                "label": str(meta.get("promotion_type_label") or ""),
            }

        summary = {
            "account_id": account_id,
            "plan_type": plan_type,
            "subscription_plan": subscription_plan,
            "has_active_subscription": has_active,
            "is_active_subscription_gratis": is_gratis,
            "is_free": is_free,
            "plus_trial_eligible": plus_trial_eligible,
            "discount": discount,
            "eligible_offer_ids": offer_ids,
            "raw": payload,
        }
        break  # 取第一个账号即可

    if plus_trial := summary.get("plus_trial_eligible"):
        log(f"  [套餐] plan={summary.get('plan_type')} 可0元试用Plus")
    elif summary.get("discount", {}).get("percentage"):
        log(f"  [套餐] plan={summary.get('plan_type')} 折扣 {summary['discount']['percentage']}%")
    else:
        log(f"  [套餐] plan={summary.get('plan_type')} sub={summary.get('subscription_plan')} 无试用/折扣")
    return summary


def register_one(session, runtime: RuntimeBridge, email: str, mailbox_url: str, device_id: str,
                 profile_name: str, birth_date: str, enable_mfa: bool) -> dict[str, Any]:
    timing: dict[str, float] = {}
    _t_start = time.monotonic()
    _t_last = _t_start

    def mark(name: str) -> None:
        nonlocal _t_last
        now = time.monotonic()
        timing[name] = round(now - _t_last, 2)
        _t_last = now

    logging_id = str(uuid.uuid4())
    session.cookies.set("oai-did", device_id, domain=".chatgpt.com")
    session.cookies.set("oai-did", device_id, domain=".openai.com")

    password = generate_password()
    password_set = False

    log(f"[{email}] 发起 OAuth...")
    auth_url = signup_auth_url(session, email, device_id, logging_id)

    otp_requested_at = time.time()
    authorize = get_navigation_with_retry(session, auth_url, navigation_headers(f"{CHATGPT}/"), "OpenAI authorize", timeout=REQUEST_TIMEOUT)
    authorize_location = response_location(authorize)
    mark("oauth")
    runtime.start(authorize_location)
    mark("sentinel")

    log(f"[{email}] authorize -> {authorize_location}")

    # 1) email-verification 页面会暴露 "Use password to continue" 分支，
    #    点击后落地 /create-account/password，用 user/register 先设密码再走 OTP。
    password_link = signup_password_url(str(authorize.text or ""), str(authorize.url or authorize_location))
    if password_link and "create-account/password" not in authorize_location:
        log(f"[{email}] 发现注册密码分支，切换...")
        authorize = get_navigation_with_retry(
            session,
            password_link,
            navigation_headers(str(authorize.url or f"{AUTH}/email-verification")),
            "Signup password page",
            timeout=(10, 30),
        )
        authorize_location = response_location(authorize)

    if "create-account/password" in authorize_location:
        password_page_url = str(authorize.url or password_link or f"{AUTH}/create-account/password")
        registered = None
        for submit_attempt in range(1, OTP_SUBMIT_ATTEMPTS + 1):
            token, so_token = runtime.solve("username_password_create")
            log(f"[{email}] 提交注册密码 attempt={submit_attempt}/{OTP_SUBMIT_ATTEMPTS}")
            try:
                otp_requested_at = time.time()
                registered = session.post(
                    f"{AUTH}/api/accounts/user/register",
                    json={"username": email, "password": password},
                    headers=protected_headers(password_page_url, token, so_token),
                    allow_redirects=False,
                    timeout=(10, 30),
                )
                require_ok(registered, "Create signup password")
                break
            except Exception as error:
                if not is_transient_protocol_failure(error) or submit_attempt >= OTP_SUBMIT_ATTEMPTS:
                    raise
                log(f"[{email}] 注册密码提交失败，重试: {short_error(error)}")
        if registered is None:
            raise RuntimeError("Create signup password did not return a result")
        otp_url = find_url(response_payload(registered), "continue_url") or str(registered.headers.get("location") or "")
        if not otp_url:
            raise RuntimeError("Create signup password response did not contain continue_url")
        authorize = get_navigation_with_retry(
            session, urljoin(AUTH, otp_url), navigation_headers(password_page_url), "Signup OTP continuation", timeout=(10, 30),
        )
        authorize_location = response_location(authorize)
        password_set = True
        mark("password")
        log(f"[{email}] 注册密码已设置")

    # 2) 邮箱验证码 OTP
    if "email-verification" not in authorize_location:
        if password_set and "log-in/password" in authorize_location:
            # 已有密码 -> 落到密码登录页，用刚设的密码验证恢复
            token, so_token = runtime.solve("password_verify")
            verified = session.post(
                f"{AUTH}/api/accounts/password/verify",
                json={"password": password},
                headers=protected_headers(str(authorize.url or f"{AUTH}/log-in/password"), token, so_token),
                allow_redirects=False,
                timeout=(10, 30),
            )
            if int(verified.status_code) == 401:
                raise RuntimeError("account_already_exists: 邮箱已注册但密码不匹配(401)")
            require_ok(verified, "Recover password verify")
            continue_url = find_url(response_payload(verified), "continue_url") or verified.headers.get("location", "")
            if not continue_url:
                raise RuntimeError("Recover password verify did not return continue_url")
            authorize = get_navigation_with_retry(session, continue_url, navigation_headers(str(authorize.url or f"{AUTH}/log-in/password")), "Recover password continuation", timeout=(10, 40))
            authorize_location = response_location(authorize)
        if "email-verification" not in authorize_location:
            raise RuntimeError(f"OpenAI authorize 落在意外页面: {authorize_location}")

    log(f"[{email}] 等待邮箱验证码...")
    code, code_timeline = poll_code(mailbox_url, email, REGISTRATION_OTP_WAIT_MS / 1000.0, otp_requested_at)
    if not code:
        raise RuntimeError("邮箱验证码超时未收到")
    mark("wait_code")
    log(f"[{email}] 收到验证码 {code}")

    token, so_token = runtime.solve("email_otp_validate")
    validate = session.post(
        f"{AUTH}/api/accounts/email-otp/validate",
        json={"code": code},
        headers=protected_headers(f"{AUTH}/email-verification", token, so_token),
        allow_redirects=False,
        timeout=(10, 30),
    )
    require_ok(validate, "OTP validate")
    continue_url = find_url(response_payload(validate), "continue_url") or validate.headers.get("location", "")
    if not continue_url:
        raise RuntimeError("OTP validate response did not contain continue_url")
    mark("validate")
    log(f"[{email}] 邮箱验证通过")

    about = get_navigation_with_retry(session, continue_url, navigation_headers(f"{AUTH}/email-verification"), "About-you page", timeout=(10, 30))
    about_location = response_location(about)

    if "about-you" in about_location:
        create_token, create_so_token = runtime.solve("oauth_create_account")
        created = session.post(
            f"{AUTH}/api/accounts/create_account",
            json={"name": profile_name, "birthdate": birth_date},
            headers=protected_headers(f"{AUTH}/about-you", create_token, create_so_token),
            allow_redirects=False,
            timeout=(10, 30),
        )
        require_ok(created, "Create account")
        finish_url = find_url(response_payload(created), "continue_url") or created.headers.get("location", "")
        if not finish_url:
            raise RuntimeError("Create-account response did not contain continue_url")
        get_navigation_with_retry(session, finish_url, navigation_headers(f"{AUTH}/about-you"), "OAuth callback", timeout=(10, 40))
        log(f"[{email}] 账号已创建 ({profile_name})")

    mark("account")
    session_payload = refresh_session(session)
    access_token = str(session_payload.get("accessToken") or "")
    account_id = account_id_from_session(session_payload, access_token)
    mark("session")

    mfa_result: dict[str, Any] = {"enabled": False}
    if enable_mfa:
        try:
            mfa_result = setup_mfa(session, device_id, access_token)
        except Exception as e:
            log(f"[{email}] MFA 启用失败（不影响账号）: {short_error(e)}")
            mfa_result = {"enabled": False, "error": short_error(e)}

    mark("mfa")
    try:
        plan_summary = check_plan(session, device_id, access_token)
    except Exception as e:
        log(f"[{email}] 套餐查询失败（不影响账号）: {short_error(e)}")
        plan_summary = {"error": short_error(e)}

    mark("plan")
    timing["total"] = round(time.monotonic() - _t_start, 2)

    return {
        "email": email,
        "password": password if password_set else "",
        "password_set": password_set,
        "state": "completed",
        "accountId": account_id,
        "accessToken": access_token,
        "session": session_payload,
        "mfa": mfa_result,
        "plan": plan_summary,
        "timing": timing,
        "code_timeline": code_timeline,
    }


def main() -> int:
    input_file = Path(sys.argv[1] if len(sys.argv) > 1 else "icloud-mailbox-apis-20-20260826-193203.txt")
    if not input_file.is_absolute():
        input_file = Path.cwd() / input_file
    if not input_file.exists():
        raise SystemExit(f"找不到输入文件: {input_file}")

    enable_mfa = "--no-mfa" not in sys.argv
    profile_name = os.environ.get("PROFILE_NAME", "Alex Morgan")
    birth_date = os.environ.get("BIRTH_DATE", "1995-01-01")

    accounts: list[tuple[str, str]] = []
    for line in input_file.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or "----" not in line:
            continue
        email, url = line.split("----", 1)
        accounts.append((email.strip().lower(), url.strip()))

    if not accounts:
        raise SystemExit("输入文件中没有有效的 邮箱----收码URL 记录")

    ts = time.strftime("%Y%m%d-%H%M%S", time.localtime())
    out_txt = input_file.parent / f"注册结果-{ts}.txt"
    out_json = input_file.parent / f"注册结果_session-{ts}.json"
    session_rows: list[dict[str, Any]] = []
    per_account: list[dict[str, Any]] = []

    for index, (email, mailbox_url) in enumerate(accounts, 1):
        log(f"\n===== [{index}/{len(accounts)}] {email} =====")
        t_start = time.monotonic()
        geo = detect_geo(requests.Session())
        session = requests.Session(impersonate=select_curl_impersonate(), headers=browser_headers(), timeout=REQUEST_TIMEOUT)
        runtime = RuntimeBridge(session, str(uuid.uuid4()), geo)
        try:
            result = register_one(session, runtime, email, mailbox_url, str(runtime.device_id), profile_name, birth_date, enable_mfa)
        except Exception as e:
            log(f"[{email}] 注册失败: {short_error(e)}")
            result = {"email": email, "password": "", "state": "failed", "error": short_error(e),
                      "session": {}, "mfa": {"enabled": False}}
        finally:
            runtime.close()
            session.close()

        elapsed = round(time.monotonic() - t_start, 2)
        result["elapsed"] = elapsed
        per_account.append({"email": email, "elapsed": elapsed, "state": result.get("state"), "timing": result.get("timing") or {},
                            "code_timeline": result.get("code_timeline") or {}})
        log(f"[{email}] 耗时 {elapsed}s  阶段={result.get('timing') or {}}")

        mfa = result.get("mfa") or {}
        totp = mfa.get("totpSecret") or ""
        plan = result.get("plan") or {}
        if plan.get("plus_trial_eligible"):
            plan_label = "0元试用Plus"
        elif plan.get("discount", {}).get("percentage"):
            plan_label = f"折扣{plan['discount']['percentage']}%"
        else:
            plan_label = f"plan={plan.get('plan_type') or plan.get('subscription_plan') or '-'}"
        password = result.get("password", "")
        if password:
            line_out = f"{result['email']}----{password}----{totp}----{plan_label}"
        else:
            line_out = f"{result['email']}----FAILED----{totp}----{result.get('error', '')}----{plan_label}"
        with open(out_txt, "a", encoding="utf-8") as f:
            f.write(line_out + "\n")
        session_rows.append(result)
        log(f"[{email}] -> {line_out}")

        if index < len(accounts):
            pause = 6 + (index % 13)
            log(f"  停顿 {pause}s...")
            time.sleep(pause)

    Path(out_json).write_text(json.dumps(session_rows, ensure_ascii=False, indent=2), encoding="utf-8")

    stage_keys = ["oauth", "sentinel", "password", "wait_code", "validate", "account", "session", "mfa", "plan", "total"]
    elapsed_list = [a["elapsed"] for a in per_account]
    ok_count = sum(1 for a in per_account if a.get("state") == "completed")
    fail_count = len(per_account) - ok_count

    log("\n===== 时间分布统计 =====")
    if elapsed_list:
        n = len(elapsed_list)
        avg = sum(elapsed_list) / n
        sorted_e = sorted(elapsed_list)
        median = (sorted_e[n // 2] if n % 2 else (sorted_e[n // 2 - 1] + sorted_e[n // 2]) / 2)
        log(f"账号数 {n}（成功 {ok_count} / 失败 {fail_count}）")
        log(f"总耗时 平均 {avg:.1f}s  中位 {median:.1f}s  最小 {min(elapsed_list):.1f}s  最大 {max(elapsed_list):.1f}s")
        for a in per_account:
            log(f"  {a['email']:<42} {a['elapsed']:>7.1f}s  {a['state']}")

    stage_agg: dict[str, list[float]] = {k: [] for k in stage_keys}
    for a in per_account:
        t = a.get("timing") or {}
        for k in stage_keys:
            v = t.get(k)
            if isinstance(v, (int, float)):
                stage_agg[k].append(float(v))
    log("各阶段平均耗时:")
    for k in stage_keys:
        vals = stage_agg[k]
        if vals:
            log(f"  {k:<10} 平均 {sum(vals) / len(vals):6.1f}s  (n={len(vals)}, min {min(vals):.1f}, max {max(vals):.1f})")

    log(f"\n完成。结果: {out_txt}\nSession: {out_json}")
    return 0


def select_curl_impersonate(preferred: str = CHATGPT_CURL_IMPERSONATE) -> str:
    try:
        from curl_cffi.requests import BrowserType
        supported = {str(item.value) for item in BrowserType}
    except Exception:
        return preferred
    if preferred in supported:
        return preferred
    for candidate in ("chrome150", "chrome146", "chrome145", "chrome142", "chrome136", "chrome133a", "chrome131", "chrome124"):
        if candidate in supported:
            return candidate
    return preferred


if __name__ == "__main__":
    raise SystemExit(main())
