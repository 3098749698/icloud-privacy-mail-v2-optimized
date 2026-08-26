import { webcrypto } from "node:crypto";
import vm from "node:vm";
import readline from "node:readline";

let sdk;
let profile;
let currentRequirements;

function reply(value) {
  process.stdout.write(`${JSON.stringify(value)}\n`);
}

function btoa(value) {
  return Buffer.from(String(value), "binary").toString("base64");
}

function atob(value) {
  return Buffer.from(String(value), "base64").toString("binary");
}

function runtime(source, options) {
  const listeners = new Map();
  const rect = (width = 0, height = 0) => ({ x: 0, y: 0, top: 0, left: 0, right: width, bottom: height, width, height });
  const makeElement = (tagName) => {
    const attrs = new Map();
    const children = [];
    const element = {
      nodeType: 1,
      nodeName: String(tagName).toUpperCase(),
      tagName: String(tagName).toUpperCase(),
      style: {},
      dataset: {},
      children,
      childNodes: children,
      parentNode: null,
      parentElement: null,
      textContent: "",
      innerHTML: "",
      value: "",
      width: 0,
      height: 0,
      appendChild(node) { children.push(node); node.parentNode = element; node.parentElement = element; return node; },
      removeChild(node) { const index = children.indexOf(node); if (index >= 0) children.splice(index, 1); return node; },
      setAttribute(name, value) { attrs.set(String(name), String(value)); },
      getAttribute(name) { return attrs.get(String(name)) ?? null; },
      hasAttribute(name) { return attrs.has(String(name)); },
      removeAttribute(name) { attrs.delete(String(name)); },
      querySelector() { return null; },
      querySelectorAll() { return []; },
      getBoundingClientRect() { return rect(Number(element.width || 0), Number(element.height || 0)); },
      addEventListener(name, handler) { listeners.set(`element:${tagName}:${name}`, handler); },
      removeEventListener(name) { listeners.delete(`element:${tagName}:${name}`); },
      dispatchEvent() { return true; },
      click() {},
      focus() {},
      blur() {},
    };
    if (String(tagName).toLowerCase() === "canvas") {
      element.width = 300;
      element.height = 150;
      element.toDataURL = () => "data:image/png;base64,iVBORw0KGgo=";
      element.getContext = () => ({
        canvas: element,
        fillStyle: "#000000",
        strokeStyle: "#000000",
        font: "10px sans-serif",
        textBaseline: "alphabetic",
        fillRect() {}, clearRect() {}, strokeRect() {}, fillText() {}, strokeText() {},
        beginPath() {}, closePath() {}, moveTo() {}, lineTo() {}, arc() {}, stroke() {}, fill() {},
        save() {}, restore() {}, translate() {}, rotate() {}, scale() {},
        measureText(text) { return { width: String(text).length * 7 }; },
        getImageData() { return { data: new Uint8ClampedArray(4), width: 1, height: 1 }; },
      });
    }
    return element;
  };
  const currentScript = makeElement("script");
  currentScript.src = options.sdkUrl;
  const iframe = {
    ...makeElement("iframe"),
    contentWindow: { postMessage() {} },
    addEventListener(name, handler) { listeners.set(`iframe:${name}`, handler); },
  };
  const documentElement = makeElement("html");
  documentElement.clientWidth = options.width;
  documentElement.clientHeight = options.height;
  const body = makeElement("body");
  body.clientWidth = options.width;
  body.clientHeight = options.height;
  const head = makeElement("head");
  const document = {
    currentScript,
    scripts: [currentScript],
    cookie: `oai-did=${encodeURIComponent(options.deviceId)}`,
    URL: options.pageUrl,
    documentURI: options.pageUrl,
    referrer: "https://auth.openai.com/",
    title: "",
    readyState: "complete",
    visibilityState: "visible",
    hidden: false,
    characterSet: "UTF-8",
    documentElement,
    body,
    head,
    fonts: { status: "loaded", ready: Promise.resolve(), check() { return true; }, addEventListener() {}, removeEventListener() {} },
    createElement(name) { return name === "iframe" ? iframe : makeElement(name); },
    createTextNode(text) { return { nodeType: 3, nodeName: "#text", textContent: String(text), parentNode: null }; },
    querySelector(selector) { if (selector === "body") return body; if (selector === "head") return head; if (selector === "html") return documentElement; return null; },
    querySelectorAll() { return []; },
    getElementById() { return null; },
    getElementsByTagName(name) { if (name === "script") return [currentScript]; return []; },
    addEventListener(name, handler) { listeners.set(`document:${name}`, handler); },
    removeEventListener(name) { listeners.delete(`document:${name}`); },
  };
  const performance = {
    now: () => Number(process.hrtime.bigint() / 1_000_000n),
    timeOrigin: Date.now() - Number(process.hrtime.bigint() / 1_000_000n),
    memory: { jsHeapSizeLimit: 4_294_967_296 },
    getEntries: () => [],
    getEntriesByType: () => [],
    getEntriesByName: () => [],
    mark() {}, measure() {}, clearMarks() {}, clearMeasures() {},
  };
  const navigator = {
    userAgent: options.userAgent,
    language: options.language,
    languages: options.languages,
    hardwareConcurrency: options.cores,
    deviceMemory: 8,
    platform: options.platform,
    vendor: "Google Inc.",
    product: "Gecko",
    cookieEnabled: true,
    onLine: true,
    webdriver: false,
    maxTouchPoints: 0,
    plugins: [],
    mimeTypes: [],
    connection: { effectiveType: "4g", rtt: 50, downlink: 10, saveData: false, addEventListener() {}, removeEventListener() {} },
    permissions: { async query() { return { state: "prompt", onchange: null }; } },
    sendBeacon() { return true; },
    getGamepads() { return []; },
    javaEnabled() { return false; },
  };
  class Event {
    constructor(type, init = {}) { this.type = String(type); Object.assign(this, init); }
  }
  class CustomEvent extends Event {
    constructor(type, init = {}) { super(type, init); this.detail = init.detail ?? null; }
  }
  class HTMLElement {}
  class HTMLCanvasElement extends HTMLElement {}
  class ImageData {
    constructor(data = new Uint8ClampedArray(4), width = 1, height = 1) {
      this.data = data;
      this.width = width;
      this.height = height;
    }
  }
  const location = new URL(options.pageUrl);
  const sandbox = {
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    Uint8Array,
    ArrayBuffer,
    Promise,
    Map,
    WeakMap,
    Date,
    Math,
    JSON,
    Object,
    String,
    Number,
    Boolean,
    RegExp,
    Error,
    TypeError,
    Intl,
    Reflect,
    Proxy,
    BigInt,
    BigInt64Array,
    BigUint64Array,
    Uint8ClampedArray,
    Float32Array,
    Float64Array,
    DataView,
    encodeURIComponent,
    decodeURIComponent,
    escape,
    unescape,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    queueMicrotask,
    console,
    crypto: webcrypto,
    btoa,
    atob,
    screen: {
      width: options.width,
      height: options.height,
      availWidth: options.width,
      availHeight: Math.max(0, options.height - 40),
      availLeft: 0,
      availTop: 0,
      colorDepth: 24,
      pixelDepth: 24,
      orientation: { type: "landscape-primary", angle: 0, addEventListener() {}, removeEventListener() {} },
    },
    performance,
    navigator,
    document,
    location,
    history: {
      length: 2,
      state: null,
      scrollRestoration: "auto",
      back() {},
      forward() {},
      go() {},
      pushState() {},
      replaceState() {},
    },
    Event,
    CustomEvent,
    HTMLElement,
    HTMLCanvasElement,
    ImageData,
    Blob,
    FormData,
    Headers,
    Request,
    Response,
    AbortController,
    AbortSignal,
    DOMException,
    structuredClone,
    cfIpLatitude: String(options.cfIpLatitude || "35.6762"),
    cfIpLongitude: String(options.cfIpLongitude || "139.6503"),
    cfIpCountry: String(options.cfIpCountry || "JP"),
    cfIpCity: String(options.cfIpCity || "Tokyo"),
    cfIpRegion: String(options.cfIpRegion || "Tokyo"),
    cfTimezone: String(options.timezone || "Asia/Tokyo"),
    fetch: async () => { throw new Error("sentinel runtime network is handled by the protocol worker"); },
    addEventListener(name, handler) { listeners.set(`window:${name}`, handler); },
    removeEventListener(name) { listeners.delete(`window:${name}`); },
    postMessage() {},
    requestIdleCallback(handler) { return setTimeout(() => handler({ timeRemaining: () => 10, didTimeout: false }), 0); },
    cancelIdleCallback(id) { clearTimeout(id); },
    innerWidth: options.width,
    innerHeight: options.height,
    outerWidth: options.width,
    outerHeight: options.height,
    devicePixelRatio: 1,
    visualViewport: { width: options.width, height: options.height, scale: 1, offsetLeft: 0, offsetTop: 0, addEventListener() {}, removeEventListener() {} },
    getComputedStyle() { return { display: "block", visibility: "visible", opacity: "1", getPropertyValue() { return ""; } }; },
    matchMedia(query) { return { matches: false, media: String(query), onchange: null, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} }; },
    localStorage: { getItem() { return null; }, setItem() {}, removeItem() {}, clear() {}, key() { return null; }, length: 0 },
    sessionStorage: { getItem() { return null; }, setItem() {}, removeItem() {}, clear() {}, key() { return null; }, length: 0 },
    MutationObserver: class { observe() {} disconnect() {} takeRecords() { return []; } },
  };
  sandbox.window = sandbox;
  sandbox.self = sandbox;
  sandbox.top = sandbox;
  sandbox.globalThis = sandbox;
  const context = vm.createContext(sandbox);
  const variants = [
    {
      marker: "t.init=we,t.sessionObserverToken=",
      replacement: "t.__proof=P,t.__bind=D,t.__solveTurnstile=_n,t.__solveSO=Nt,t.__startSO=Et,t.init=we,t.sessionObserverToken=",
    },
    {
      marker: "t.init=Ee,t.requirementsToken=",
      replacement: "t.__proof=E,t.__bind=D,t.__solveTurnstile=Rn,t.__solveSO=qt,t.__startSO=Mt,t.init=Ee,t.requirementsToken=",
    },
  ];
  const variant = variants.find((item) => source.includes(item.marker));
  if (!variant) throw new Error("unsupported Sentinel SDK shape: export marker not found");
  const instrumented = source.replace(variant.marker, variant.replacement);
  vm.runInContext(instrumented, context, { filename: "sentinel-sdk.js", timeout: 10_000 });
  const exported = context.SentinelSDK;
  if (!exported?.__proof || !exported?.__solveTurnstile || !exported?.__solveSO) {
    throw new Error("Sentinel SDK internal exports were not initialized");
  }
  return exported;
}

async function handle(command) {
  if (command.type === "init") {
    process.env.TZ = String(command.timezone || "Asia/Tokyo");
    profile = {
      sdkUrl: String(command.sdkUrl),
      deviceId: String(command.deviceId),
      pageUrl: String(command.pageUrl || "https://auth.openai.com/email-verification"),
      userAgent: String(command.userAgent || "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"),
      platform: String(command.platform || "MacIntel"),
      language: String(command.language || "ja-JP"),
      languages: Array.isArray(command.languages) ? command.languages.map(String) : ["ja-JP", "ja"],
      cores: Math.max(2, Number(command.cores || 8)),
      width: Math.max(800, Number(command.width || 1920)),
      height: Math.max(600, Number(command.height || 1080)),
      timezone: String(command.timezone || "Asia/Tokyo"),
      cfIpLatitude: String(command.cfIpLatitude || "35.6762"),
      cfIpLongitude: String(command.cfIpLongitude || "139.6503"),
      cfIpCountry: String(command.cfIpCountry || "JP"),
      cfIpCity: String(command.cfIpCity || "Tokyo"),
      cfIpRegion: String(command.cfIpRegion || "Tokyo"),
    };
    sdk = runtime(String(command.sdkSource), profile);
    currentRequirements = await sdk.__proof.getRequirementsToken();
    return { ok: true, p: currentRequirements };
  }
  if (command.type === "requirements") {
    if (!sdk) throw new Error("runtime is not initialized");
    currentRequirements = await sdk.__proof.getRequirementsToken();
    return { ok: true, p: currentRequirements };
  }
  if (command.type === "solve") {
    if (!sdk || !profile) throw new Error("runtime is not initialized");
    const challenge = command.challenge && typeof command.challenge === "object" ? command.challenge : {};
    const flow = String(command.flow || "");
    sdk.__bind(challenge, currentRequirements);
    const p = await sdk.__proof.getEnforcementToken(challenge);
    const t = challenge.turnstile?.required && challenge.turnstile?.dx
      ? await sdk.__solveTurnstile(challenge, challenge.turnstile.dx)
      : null;
    let so = null;
    if (challenge.so?.required && challenge.so?.snapshot_dx) {
      sdk.__startSO(challenge);
      await new Promise((resolve) => setTimeout(resolve, 80));
      so = await sdk.__solveSO(challenge.so.snapshot_dx);
    }
    const common = { c: String(challenge.token || ""), id: profile.deviceId, flow };
    return {
      ok: true,
      token: JSON.stringify({ p, t, ...common }),
      soToken: so ? JSON.stringify({ so, ...common }) : null,
      fields: { p: String(p || "").length, t: String(t || "").length, so: String(so || "").length, c: common.c.length },
    };
  }
  throw new Error(`unknown command type: ${command.type}`);
}

const input = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
for await (const line of input) {
  if (!line.trim()) continue;
  try {
    reply(await handle(JSON.parse(line)));
  } catch (error) {
    reply({ ok: false, error: `${error?.name || "Error"}: ${error?.message || error}` });
  }
}
