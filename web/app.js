"use strict";

(() => {
  const ROUTES = {
    overview: ["概览", "实时掌握转发节点与内核状态"],
    rules: ["转发规则", "管理监听端口、目标与访问限制"],
    engine: ["协议内核", "配置并运行可选 sing-box 内核"],
    help: ["使用帮助", "规则、安全与运行说明"],
  };
  const POLL_INTERVAL = 8000;
  const THEME_KEY = "relaydeck-theme";
  const SAFE_NETWORKS = new Set(["tcp", "udp", "both"]);
  const SAFE_PROBE_STATUSES = new Set(["listening", "available", "reachable", "error", "unverified"]);
  const DEFAULT_MAX_CONNECTIONS = 8192;
  const MAX_CONNECTIONS = 65536;
  const ENGINE_INSTALL_COMMAND = "curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install-sing-box.sh | bash";

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));
  const byId = (id) => document.getElementById(id);

  const state = {
    authenticated: false,
    route: "overview",
    status: null,
    statusLoading: false,
    browserPanelRttMs: null,
    rules: [],
    rulesLoaded: false,
    rulesLoading: false,
    rulesSaving: false,
    editingRuleIndex: -1,
    ruleProbeResults: new Map(),
    ruleProbePending: new Map(),
    ruleProbeLatest: new Map(),
    ruleProbeSequence: 0,
    ruleDraftProbe: {
      token: 0,
      key: "",
      phase: "idle",
      result: null,
      error: "",
      controller: null,
    },
    engine: null,
    engineLoaded: false,
    engineLoading: false,
    engineSaving: false,
    engineDirty: false,
    engineInstallation: null,
    engineInstallRequesting: false,
    engineInstallRequestError: "",
    engineRequestEpoch: 0,
    engineRequestSequence: 0,
    templates: [],
    templatesLoaded: false,
    pollTimer: null,
    clockTimer: null,
    toastId: 0,
    confirmResolve: null,
    themePreference: "dark",
  };

  const elements = {
    loginView: byId("login-view"),
    appView: byId("app-view"),
    loginForm: byId("login-form"),
    loginUsername: byId("login-username"),
    loginPassword: byId("login-password"),
    loginSubmit: byId("login-submit"),
    loginError: byId("login-error"),
    sidebar: byId("sidebar"),
    navScrim: byId("nav-scrim"),
    navOpen: byId("nav-open"),
    navClose: byId("nav-close"),
    pageTitle: byId("page-title"),
    pageSubtitle: byId("page-subtitle"),
    servicePill: byId("service-pill"),
    sidebarStatusDot: byId("sidebar-status-dot"),
    sidebarVersion: byId("sidebar-version-text"),
    browserPanelRtt: byId("browser-panel-rtt"),
    browserPanelRttValue: byId("browser-panel-rtt-value"),
    ruleProbeHttpRtt: byId("rule-probe-http-rtt"),
    navRuleCount: byId("nav-rule-count"),
    rulesBody: byId("rules-body"),
    rulesLoading: byId("rules-loading"),
    rulesEmpty: byId("rules-empty"),
    rulesNoResults: byId("rules-no-results"),
    rulesCount: byId("rules-count"),
    rulesError: byId("rules-error"),
    ruleSearch: byId("rule-search"),
    importFile: byId("import-file"),
    ruleDialog: byId("rule-dialog"),
    ruleForm: byId("rule-form"),
    ruleDialogTitle: byId("rule-dialog-title"),
    ruleFormError: byId("rule-form-error"),
    ruleSubmit: byId("rule-submit"),
    ruleProbe: byId("rule-probe"),
    ruleProbeNotice: byId("rule-probe-notice"),
    ruleListenProbe: byId("rule-listen-probe"),
    ruleListenProbeBadge: byId("rule-listen-probe-badge"),
    ruleListenProbeMetric: byId("rule-listen-probe-metric"),
    ruleListenProbeMessage: byId("rule-listen-probe-message"),
    ruleListenProbeTime: byId("rule-listen-probe-time"),
    ruleTargetProbe: byId("rule-target-probe"),
    ruleTargetProbeBadge: byId("rule-target-probe-badge"),
    ruleTargetProbeMetric: byId("rule-target-probe-metric"),
    ruleTargetProbeMessage: byId("rule-target-probe-message"),
    ruleTargetProbeTime: byId("rule-target-probe-time"),
    ruleTargetProbeAddress: byId("rule-target-probe-address"),
    confirmDialog: byId("confirm-dialog"),
    confirmTitle: byId("confirm-title"),
    confirmMessage: byId("confirm-message"),
    confirmError: byId("confirm-error"),
    confirmAction: byId("confirm-action"),
    confirmCancel: byId("confirm-cancel"),
    passwordDialog: byId("password-dialog"),
    passwordForm: byId("password-form"),
    passwordSubmit: byId("password-submit"),
    passwordFormError: byId("password-form-error"),
    engineAlert: byId("engine-alert"),
    engineInstallHelp: byId("engine-install-help"),
    engineInstall: byId("engine-install"),
    engineInstallStatus: byId("engine-install-status"),
    engineInstallCommand: byId("engine-install-command"),
    engineCopyInstall: byId("engine-copy-install"),
    engineRecheck: byId("engine-recheck"),
    engineRefresh: byId("engine-refresh"),
    engineFormError: byId("engine-form-error"),
    engineConfig: byId("engine-config"),
    engineEnabled: byId("engine-enabled"),
    engineTemplate: byId("engine-template"),
    engineApplyTemplate: byId("engine-apply-template"),
    engineTemplateDescription: byId("engine-template-description"),
    engineSave: byId("engine-save"),
    engineStop: byId("engine-stop"),
    engineDirty: byId("engine-dirty"),
    engineLogs: byId("engine-logs"),
    toastRegion: byId("toast-region"),
    themeToggle: byId("theme-toggle"),
  };

  class ApiError extends Error {
    constructor(message, status = 0) {
      super(message);
      this.name = "ApiError";
      this.status = status;
    }
  }

  function createElement(tag, className = "", text = "") {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== "") node.textContent = String(text);
    return node;
  }

  function createIcon(name, title = "") {
    const safeName = /^[a-z-]+$/.test(name) ? name : "info";
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "icon");
    svg.setAttribute("aria-hidden", title ? "false" : "true");
    if (title) {
      svg.setAttribute("role", "img");
      const titleNode = document.createElementNS("http://www.w3.org/2000/svg", "title");
      titleNode.textContent = title;
      svg.append(titleNode);
    }
    const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", `#i-${safeName}`);
    svg.append(use);
    return svg;
  }

  function setText(id, value) {
    const node = byId(id);
    if (node) node.textContent = value === undefined || value === null ? "—" : String(value);
  }

  function setBadge(node, text, tone = "neutral") {
    if (!node) return;
    node.textContent = text;
    node.className = `tiny-badge ${tone}`;
  }

  function hintError(node, message) {
    if (!node) return;
    node.textContent = message || "";
    node.hidden = !message;
  }

  function errorMessage(error) {
    if (error instanceof ApiError) return error.message;
    if (error instanceof TypeError) return "无法连接 RelayDeck 服务，请检查网络或服务状态。";
    return error && error.message ? error.message : "操作未能完成，请稍后重试。";
  }

  function showToast(message, type = "success", duration = 4200) {
    const toast = createElement("div", `toast ${type}`);
    toast.setAttribute("role", type === "error" ? "alert" : "status");
    toast.append(createIcon(type === "error" ? "alert" : "check"));
    toast.append(createElement("span", "", message));
    const close = createElement("button", "", "×");
    close.type = "button";
    close.setAttribute("aria-label", "关闭通知");
    close.addEventListener("click", () => toast.remove());
    toast.append(close);
    elements.toastRegion.append(toast);
    state.toastId += 1;
    const id = state.toastId;
    window.setTimeout(() => {
      if (toast.isConnected && state.toastId >= id) toast.remove();
    }, duration);
  }

  async function api(path, options = {}) {
    const method = (options.method || "GET").toUpperCase();
    const headers = {
      Accept: "application/json",
      ...(options.headers || {}),
    };
    if (method !== "GET" && method !== "HEAD") {
      headers["X-RelayDeck-CSRF"] = "1";
    }
    if (options.body !== undefined) {
      headers["Content-Type"] = "application/json";
    }
    const request = {
      method,
      credentials: "same-origin",
      cache: "no-store",
      headers,
      signal: options.signal,
    };
    if (options.body !== undefined) {
      request.body = JSON.stringify(options.body);
    }

    let response;
    try {
      response = await fetch(path, request);
    } catch (error) {
      throw new ApiError("无法连接 RelayDeck 服务，请检查网络或服务状态。", 0);
    }

    const contentType = response.headers.get("content-type") || "";
    let payload = null;
    if (response.status !== 204) {
      if (contentType.includes("application/json")) {
        payload = await response.json().catch(() => null);
      } else {
        const text = await response.text().catch(() => "");
        payload = text ? { error: text } : null;
      }
    }

    if (!response.ok) {
      if (response.status === 401 && options.authRedirect !== false) {
        showLogin("登录状态已失效，请重新登录。");
      }
      const message = payload && typeof payload.error === "string"
        ? payload.error
        : `请求失败（HTTP ${response.status}）`;
      throw new ApiError(message, response.status);
    }
    return payload;
  }

  function formatNumber(value) {
    if (value === null || value === undefined || value === "") return "—";
    const number = Number(value);
    if (!Number.isFinite(number)) return "—";
    return new Intl.NumberFormat("zh-CN").format(number);
  }

  function formatBytes(value) {
    if (value === null || value === undefined || value === "") return "—";
    let number = Number(value);
    if (!Number.isFinite(number) || number < 0) return "—";
    const units = ["B", "KB", "MB", "GB", "TB", "PB"];
    let index = 0;
    while (number >= 1024 && index < units.length - 1) {
      number /= 1024;
      index += 1;
    }
    const digits = index === 0 ? 0 : number >= 100 ? 0 : number >= 10 ? 1 : 2;
    return `${number.toFixed(digits)} ${units[index]}`;
  }

  function formatDuration(seconds) {
    const total = Math.max(0, Math.floor(Number(seconds) || 0));
    const days = Math.floor(total / 86400);
    const hours = Math.floor((total % 86400) / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    if (days > 0) return `${days} 天 ${hours} 小时`;
    if (hours > 0) return `${hours} 小时 ${minutes} 分`;
    if (minutes > 0) return `${minutes} 分钟`;
    return `${total} 秒`;
  }

  function numberOrNull(value) {
    if (value === null || value === undefined || value === "") return null;
    const number = Number(value);
    return Number.isFinite(number) ? number : null;
  }

  function effectiveMaxConnections(rule) {
    const effective = numberOrNull(rule && rule.effective_max_connections);
    if (effective !== null) return Math.max(0, effective);
    const configured = numberOrNull(rule && rule.max_connections);
    return configured && configured > 0 ? configured : DEFAULT_MAX_CONNECTIONS;
  }

  function normalizedProbeEndpoint(value) {
    const endpoint = value && typeof value === "object" ? value : {};
    return {
      status: SAFE_PROBE_STATUSES.has(endpoint.status) ? endpoint.status : "unverified",
      latency_ms: numberOrNull(endpoint.latency_ms),
      message: typeof endpoint.message === "string" ? endpoint.message : "",
      udp_status: typeof endpoint.udp_status === "string" ? endpoint.udp_status : "",
    };
  }

  function normalizedProbeResult(value) {
    if (!value || typeof value !== "object") {
      throw new ApiError("连通性检测接口返回格式无效。", 0);
    }
    return {
      checked_at: typeof value.checked_at === "string" ? value.checked_at : "",
      target_address: typeof value.target_address === "string" ? value.target_address : "",
      listen: normalizedProbeEndpoint(value.listen),
      target: normalizedProbeEndpoint(value.target),
    };
  }

  function probePayload(rule) {
    return {
      id: typeof rule.id === "string" ? rule.id : "",
      name: typeof rule.name === "string" ? rule.name : "",
      enabled: Boolean(rule.enabled),
      network: SAFE_NETWORKS.has(rule.network) ? rule.network : "both",
      listen: typeof rule.listen === "string" ? rule.listen.trim() : "",
      target: typeof rule.target === "string" ? rule.target.trim() : "",
      allowed_cidrs: Array.isArray(rule.allowed_cidrs) ? rule.allowed_cidrs : [],
      max_connections: Number(rule.max_connections) || 0,
      udp_timeout: Number(rule.udp_timeout) || 0,
    };
  }

  function probeCacheKey(rule) {
    const payload = probePayload(rule);
    return JSON.stringify({
      id: payload.id,
      enabled: payload.enabled,
      network: payload.network,
      listen: payload.listen,
      target: payload.target,
      allowed_cidrs: payload.allowed_cidrs,
      max_connections: payload.max_connections,
      udp_timeout: payload.udp_timeout,
    });
  }

  function beginRuleProbe(key) {
    const sequence = ++state.ruleProbeSequence;
    state.ruleProbeLatest.set(key, sequence);
    return sequence;
  }

  function rememberRuleProbe(key, sequence, result) {
    if (state.ruleProbeLatest.get(key) !== sequence) return false;
    state.ruleProbeResults.set(key, result);
    return true;
  }

  function formatProbeLatency(value) {
    const latency = numberOrNull(value);
    if (latency === null) return "";
    const digits = latency < 10 ? 2 : 1;
    return `${new Intl.NumberFormat("zh-CN", { maximumFractionDigits: digits }).format(latency)} ms`;
  }

  function formatProbeTime(value) {
    const date = value ? new Date(value) : null;
    if (!date || !Number.isFinite(date.getTime())) return "检测时间未知";
    return `检测于 ${new Intl.DateTimeFormat("zh-CN", {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    }).format(date)}`;
  }

  function updateBrowserPanelRtt(elapsedMs) {
    const value = numberOrNull(elapsedMs);
    if (value === null || value < 0) return;
    state.browserPanelRttMs = value;
    const formatted = `${new Intl.NumberFormat("zh-CN", {
      maximumFractionDigits: value < 100 ? 1 : 0,
    }).format(value)} ms`;
    if (elements.browserPanelRttValue) elements.browserPanelRttValue.textContent = formatted;
    if (elements.browserPanelRtt) {
      elements.browserPanelRtt.hidden = false;
      elements.browserPanelRtt.setAttribute(
        "aria-label",
        `浏览器到面板的最近一次 HTTP 往返耗时 ${formatted}；不代表规则入口或协议 RTT`,
      );
    }
    if (elements.ruleProbeHttpRtt) elements.ruleProbeHttpRtt.textContent = formatted;
  }

  function probeStatusPresentation(status) {
    switch (status) {
      case "listening":
        return { text: "监听中", tone: "ok" };
      case "available":
        return { text: "可绑定", tone: "ok" };
      case "reachable":
        return { text: "TCP 可达", tone: "ok" };
      case "error":
        return { text: "检测失败", tone: "bad" };
      default:
        return { text: "未验证", tone: "warn" };
    }
  }

  function setProbeBadge(node, text, tone = "neutral") {
    if (!node) return;
    node.textContent = text;
    node.className = `probe-status ${tone}`;
  }

  function setProbeCard(card, tone) {
    if (card) card.className = `probe-card ${tone || "neutral"}`;
  }

  function sumRules(field) {
    return state.rules.reduce((total, rule) => total + (numberOrNull(rule[field]) || 0), 0);
  }

  function updateConnectionLabels(status) {
    const healthy = Boolean(status);
    elements.servicePill.className = `service-pill ${healthy ? "" : "muted"}`;
    const dot = $(".status-dot", elements.servicePill);
    if (dot) dot.className = `status-dot ${healthy ? "ok" : ""}`;
    const label = $("span:last-child", elements.servicePill);
    if (label) label.textContent = healthy ? "服务正常" : "连接中";
    elements.sidebarStatusDot.className = `status-dot ${healthy ? "ok" : ""}`;
    elements.sidebarVersion.textContent = healthy
      ? `RelayDeck ${status.version || "在线"}`
      : "正在连接服务";
  }

  function markServiceError() {
    elements.servicePill.className = "service-pill bad";
    const dot = $(".status-dot", elements.servicePill);
    if (dot) dot.className = "status-dot bad";
    const label = $("span:last-child", elements.servicePill);
    if (label) label.textContent = "服务异常";
    elements.sidebarStatusDot.className = "status-dot bad";
  }

  function updateOverview(status) {
    const forward = status && status.forward && typeof status.forward === "object"
      ? status.forward
      : {};
    const rulesLoaded = state.rulesLoaded;
    const totalRules = numberOrNull(forward.total_rules)
      ?? (rulesLoaded ? state.rules.length : null);
    const totalRunning = numberOrNull(forward.active_rules)
      ?? (rulesLoaded ? state.rules.filter((rule) => rule.running).length : null);
    const connections = numberOrNull(forward.active_connections)
      ?? (rulesLoaded ? sumRules("active_connections") : null);
    const effectiveConnections = numberOrNull(forward.effective_max_connections)
      ?? (rulesLoaded
        ? state.rules.reduce((total, rule) => total + effectiveMaxConnections(rule), 0)
        : null);
    const udpSessions = numberOrNull(forward.udp_sessions)
      ?? (rulesLoaded ? sumRules("udp_sessions") : null);
    const bytesIn = numberOrNull(forward.bytes_in)
      ?? (rulesLoaded ? sumRules("bytes_in") : null);
    const bytesOut = numberOrNull(forward.bytes_out)
      ?? (rulesLoaded ? sumRules("bytes_out") : null);

    setText("stat-rules", totalRules === null ? "—" : formatNumber(totalRules));
    setText("stat-running", totalRunning === null ? "—" : formatNumber(totalRunning));
    setText("stat-connections", connections === null ? "—" : formatNumber(connections));
    setText("stat-udp", udpSessions === null ? "—" : formatNumber(udpSessions));
    setText("stat-in", bytesIn === null ? "—" : formatBytes(bytesIn));
    setText("stat-out", bytesOut === null ? "—" : formatBytes(bytesOut));

    if (totalRules !== null) {
      setText("stat-rules-note", totalRules === 0 ? "尚未配置转发规则" : "当前配置总数");
      setText(
        "stat-running-note",
        totalRules === 0 ? "添加规则后开始转发" : `${totalRunning} 条端口监听处于运行状态`,
      );
    } else {
      setText("stat-rules-note", "等待规则数据");
      setText("stat-running-note", "等待运行状态");
    }

    const connectionsNote = byId("stat-connections-note");
    if (connectionsNote) {
      if (connections === null || effectiveConnections === null || effectiveConnections <= 0) {
        connectionsNote.textContent = "当前 TCP 连接";
        connectionsNote.className = "stat-note";
      } else {
        const ratio = connections / effectiveConnections;
        connectionsNote.textContent = `${formatNumber(connections)} / ${formatNumber(effectiveConnections)} 有效上限`;
        connectionsNote.className = `stat-note${ratio >= 1 ? " danger" : ratio >= 0.8 ? " warning" : ""}`;
      }
    }

    setText("detail-version", status && status.version);
    setText("detail-hostname", status && status.hostname);
    setText("detail-go", status && status.go_version);
    setText("detail-uptime", status ? formatDuration(status.uptime) : "—");
    setText(
      "detail-memory",
      status && numberOrNull(status.memory_bytes) !== null
        ? formatBytes(status.memory_bytes)
        : "—",
    );
    setText("detail-goroutines", status ? formatNumber(status.goroutines) : "—");

    const engine = status && status.engine && typeof status.engine === "object"
      ? status.engine
      : null;
    setText(
      "component-forward-text",
      totalRunning === null ? "等待状态" : `${formatNumber(totalRunning)} 条规则正在监听`,
    );
    setBadge(
      byId("component-forward-badge"),
      totalRunning === null ? "—" : totalRunning > 0 ? "ACTIVE" : "IDLE",
      totalRunning === null ? "neutral" : totalRunning > 0 ? "" : "neutral",
    );

    if (engine) {
      const engineRunning = Boolean(engine.running);
      const available = Boolean(engine.available);
      setText(
        "component-engine-text",
        available
          ? engineRunning ? `运行中${engine.version ? ` · ${engine.version}` : ""}` : "已安装，当前未运行"
          : "未安装或不可用",
      );
      setBadge(
        byId("component-engine-badge"),
        engineRunning ? "RUNNING" : available ? "READY" : "OFF",
        engineRunning ? "" : available ? "neutral" : "bad",
      );
    } else {
      setText("component-engine-text", "状态未返回");
      setBadge(byId("component-engine-badge"), "—", "neutral");
    }
  }

  async function loadStatus({ silent = false } = {}) {
    if (state.statusLoading || !state.authenticated) return null;
    state.statusLoading = true;
    try {
      const requestStarted = window.performance.now();
      const status = await api("/api/status");
      updateBrowserPanelRtt(window.performance.now() - requestStarted);
      if (!status || typeof status !== "object") {
        throw new ApiError("状态接口返回了无效数据。", 0);
      }
      state.status = status;
      updateConnectionLabels(status);
      if (state.route === "overview") updateOverview(status);
      return status;
    } catch (error) {
      if (!silent && !(error instanceof ApiError && error.status === 401)) {
        markServiceError();
        showToast(errorMessage(error), "error");
      }
      return null;
    } finally {
      state.statusLoading = false;
    }
  }

  async function loadOverview() {
    const tasks = [loadStatus()];
    if (!state.rulesLoaded) tasks.push(loadRules({ silent: true }));
    await Promise.all(tasks);
    if (state.status) updateOverview(state.status);
  }

  function updateRuleCounts() {
    if (!state.rulesLoaded) return;
    const total = state.rules.length;
    const running = state.rules.filter((rule) => rule.running).length;
    elements.navRuleCount.hidden = total === 0;
    elements.navRuleCount.textContent = String(total);
    elements.rulesCount.textContent = total === 0
      ? "没有转发规则"
      : `${total} 条规则 · ${running} 条运行中`;
  }

  function normalizedRule(rule) {
    return {
      id: typeof rule.id === "string" ? rule.id : "",
      name: typeof rule.name === "string" ? rule.name : "未命名规则",
      enabled: Boolean(rule.enabled),
      network: SAFE_NETWORKS.has(rule.network) ? rule.network : "both",
      listen: typeof rule.listen === "string" ? rule.listen : "",
      target: typeof rule.target === "string" ? rule.target : "",
      allowed_cidrs: Array.isArray(rule.allowed_cidrs)
        ? rule.allowed_cidrs.filter((item) => typeof item === "string")
        : [],
      max_connections: numberOrNull(rule.max_connections) || 0,
      udp_timeout: numberOrNull(rule.udp_timeout) || 0,
      active_connections: numberOrNull(rule.active_connections) || 0,
      effective_max_connections: numberOrNull(rule.effective_max_connections),
      bytes_in: numberOrNull(rule.bytes_in) || 0,
      bytes_out: numberOrNull(rule.bytes_out) || 0,
      error: typeof rule.error === "string" ? rule.error : "",
      running: Boolean(rule.running),
      udp_sessions: numberOrNull(rule.udp_sessions) || 0,
    };
  }

  function rulePayload(rule) {
    const payload = {
      name: rule.name,
      enabled: Boolean(rule.enabled),
      network: SAFE_NETWORKS.has(rule.network) ? rule.network : "both",
      listen: rule.listen,
      target: rule.target,
      allowed_cidrs: Array.isArray(rule.allowed_cidrs) ? rule.allowed_cidrs : [],
      max_connections: Number(rule.max_connections) || 0,
      udp_timeout: Number(rule.udp_timeout) || 0,
    };
    if (rule.id) payload.id = rule.id;
    return payload;
  }

  function makeRuleAction(iconName, label, className, handler) {
    const button = createElement("button", `icon-button ${className}`.trim());
    button.type = "button";
    button.title = label;
    button.setAttribute("aria-label", label);
    button.append(createIcon(iconName));
    button.addEventListener("click", handler);
    return button;
  }

  function makeProtocolBadges(network) {
    const wrapper = createElement("div", "protocol-badges");
    const protocols = network === "both" ? ["TCP", "UDP"] : [network.toUpperCase()];
    protocols.forEach((protocol) => wrapper.append(createElement("span", "protocol-badge", protocol)));
    return wrapper;
  }

  function makeEndpoint(rule) {
    const wrapper = createElement("div", "endpoint-stack");
    const route = createElement("div", "endpoint");
    route.append(createElement("code", "", rule.listen || "—"));
    route.append(createIcon("chevron"));
    route.append(createElement("code", "", rule.target || "—"));
    wrapper.append(route);
    wrapper.append(makeRuleProbeSummary(rule));
    return wrapper;
  }

  function makeLimits(rule) {
    const wrapper = createElement("div", "limit-list");
    const effectiveMax = effectiveMaxConnections(rule);
    const transports = [];
    if (rule.network !== "udp") transports.push(["TCP 连接", rule.active_connections]);
    if (rule.network !== "tcp") transports.push(["UDP 会话", rule.udp_sessions]);
    transports.forEach(([label, count]) => {
      const active = numberOrNull(count) || 0;
      const usage = effectiveMax > 0 ? active / effectiveMax : 0;
      const usageLine = createElement(
        "span",
        `limit-usage${usage >= 1 ? " full" : usage >= 0.8 ? " near" : ""}`,
      );
      usageLine.append(createElement("strong", "", label));
      usageLine.append(document.createTextNode(
        ` ${formatNumber(active)} / ${formatNumber(effectiveMax)}`,
      ));
      if (usage >= 1) {
        usageLine.append(createElement("em", "", "已达上限"));
      } else if (usage >= 0.8) {
        usageLine.append(createElement("em", "", "接近上限"));
      }
      wrapper.append(usageLine);
    });
    wrapper.append(createElement(
      "span",
      "",
      rule.max_connections > 0
        ? `配置上限 ${formatNumber(rule.max_connections)}`
        : `配置上限 0 · 默认 ${formatNumber(DEFAULT_MAX_CONNECTIONS)}`,
    ));
    wrapper.append(createElement(
      "span",
      "",
      rule.allowed_cidrs.length > 0
        ? `CIDR 白名单 ${rule.allowed_cidrs.length} 项`
        : "允许全部来源",
    ));
    wrapper.append(createElement(
      "span",
      "",
      rule.network === "tcp" ? "UDP 未启用" : `UDP 空闲 ${formatNumber(rule.udp_timeout)} 秒`,
    ));
    return wrapper;
  }

  function appendProbeSummaryLine(wrapper, label, statusText, tone, detail) {
    const line = createElement("div", "rule-probe-line");
    line.append(createElement("span", "rule-probe-label", label));
    line.append(createElement("span", `probe-status ${tone}`, statusText));
    line.append(createElement("span", "rule-probe-detail", detail || ""));
    wrapper.append(line);
  }

  function makeRuleProbeSummary(rule) {
    const key = probeCacheKey(rule);
    const pending = state.ruleProbePending.get(key);
    const stored = state.ruleProbeResults.get(key);
    const wrapper = createElement("div", "rule-probe");
    const heading = createElement("div", "rule-probe-heading");
    heading.append(createElement("span", "", "最近检测"));

    const probeButton = createElement("button", "probe-mini-action");
    probeButton.type = "button";
    probeButton.disabled = Boolean(pending);
    probeButton.setAttribute("aria-label", pending ? "正在检测连通性" : `检测“${rule.name}”的连通性`);
    probeButton.append(createIcon("refresh"));
    probeButton.append(createElement("span", "", pending ? "检测中…" : "检测"));
    probeButton.addEventListener("click", () => probeRule(rule));
    heading.append(probeButton);
    wrapper.append(heading);

    if (pending) {
      appendProbeSummaryLine(wrapper, "入口", "检测中…", "neutral", "本机监听检查");
      appendProbeSummaryLine(wrapper, "目标", "检测中…", "neutral", "TCP 握手 / UDP 验证范围");
      wrapper.append(createElement("small", "rule-probe-time", "检测请求进行中"));
      return wrapper;
    }

    if (!stored) {
      appendProbeSummaryLine(wrapper, "入口", "待检测", "neutral", "本机监听 / 端口可绑定");
      appendProbeSummaryLine(wrapper, "目标", "待检测", "neutral", "UDP 不发送随机测试包");
      return wrapper;
    }

    if (stored.error) {
      appendProbeSummaryLine(wrapper, "入口", "检测失败", "bad", "未取得本机监听结果");
      appendProbeSummaryLine(wrapper, "目标", "检测失败", "bad", "未取得目标握手结果");
      wrapper.append(createElement("small", "rule-probe-time", formatProbeTime(stored.receivedAt)));
      const details = createElement("details", "probe-details");
      details.append(createElement("summary", "", "查看检测原因"));
      details.append(createElement("p", "", stored.error));
      wrapper.append(details);
      return wrapper;
    }

    const result = stored.result;
    const listenStatus = probeStatusPresentation(result.listen.status);
    let listenDetail = "";
    if (result.listen.status === "listening" && result.listen.latency_ms !== null) {
      listenDetail = `本机 TCP ${formatProbeLatency(result.listen.latency_ms)}`;
    } else if (result.listen.status === "listening") {
      listenDetail = "本机监听正常 · 外部延迟需客户端测量";
    } else if (result.listen.status === "available") {
      listenDetail = rule.network === "udp" ? "UDP 端口可绑定" : "TCP 端口可绑定";
    } else if (result.listen.status === "error") {
      listenDetail = "入口监听检查失败";
    } else {
      listenDetail = "入口状态未验证";
    }
    if (result.listen.udp_status === "unverified" && rule.network !== "tcp") {
      listenDetail += " · UDP 未验证";
    }
    appendProbeSummaryLine(wrapper, "入口", listenStatus.text, listenStatus.tone, listenDetail);

    const targetStatus = probeStatusPresentation(result.target.status);
    let targetDetail = "";
    if (result.target.status === "reachable" && result.target.latency_ms !== null) {
      targetDetail = `TCP ${formatProbeLatency(result.target.latency_ms)}`;
    } else if (result.target.status === "error") {
      targetDetail = rule.network === "udp" ? "目标解析或检测失败" : "TCP 握手失败";
    } else if (rule.network === "udp") {
      targetDetail = "无通用握手";
    } else {
      targetDetail = "目标状态未验证";
    }
    if (result.target.udp_status === "unverified" && rule.network !== "tcp") {
      targetDetail += " · UDP 未验证";
    }
    appendProbeSummaryLine(wrapper, "目标", targetStatus.text, targetStatus.tone, targetDetail);
    wrapper.append(createElement("small", "rule-probe-time", formatProbeTime(result.checked_at || stored.receivedAt)));

    const messages = [];
    if (result.listen.message) messages.push(["入口", result.listen.message]);
    if (result.target.message) messages.push(["目标", result.target.message]);
    if (messages.length > 0) {
      const details = createElement("details", "probe-details");
      details.append(createElement("summary", "", "查看完整检测说明"));
      messages.forEach(([label, message]) => {
        const paragraph = createElement("p");
        paragraph.append(createElement("strong", "", `${label}：`));
        paragraph.append(document.createTextNode(message));
        details.append(paragraph);
      });
      if (result.target_address) {
        details.append(createElement("small", "", `解析目标：${result.target_address}`));
      }
      wrapper.append(details);
    }
    return wrapper;
  }

  async function probeRule(rule, { announce = true } = {}) {
    if (!rule || !SAFE_NETWORKS.has(rule.network)) return false;
    const key = probeCacheKey(rule);
    if (state.ruleProbePending.has(key)) return false;
    const sequence = beginRuleProbe(key);
    state.ruleProbePending.set(key, { sequence, startedAt: Date.now() });
    if (state.route === "rules") renderRules();
    try {
      const response = await api("/api/rules/probe", {
        method: "POST",
        body: { rule: probePayload(rule) },
      });
      const result = normalizedProbeResult(response);
      return rememberRuleProbe(key, sequence, {
        result,
        receivedAt: result.checked_at || new Date().toISOString(),
      });
    } catch (error) {
      const current = rememberRuleProbe(key, sequence, {
        error: errorMessage(error),
        receivedAt: new Date().toISOString(),
      });
      if (current && announce) showToast(`连通性检测失败：${errorMessage(error)}`, "error", 6000);
      return false;
    } finally {
      if (state.ruleProbePending.get(key)?.sequence === sequence) {
        state.ruleProbePending.delete(key);
      }
      if (state.route === "rules") renderRules();
    }
  }

  function makeRuntime(rule) {
    const wrapper = createElement("div", "runtime");
    const runtimeState = createElement(
      "span",
      `runtime-state ${rule.running ? "ok" : rule.error ? "bad" : ""}`,
    );
    runtimeState.append(createElement(
      "span",
      "status-dot",
    ));
    const dot = $(".status-dot", runtimeState);
    if (dot && rule.running) dot.classList.add("ok");
    if (dot && rule.error) dot.classList.add("bad");
    runtimeState.append(document.createTextNode(
      rule.running ? "运行中" : rule.error ? "启动异常" : rule.enabled ? "等待启动" : "已停用",
    ));
    wrapper.append(runtimeState);
    const traffic = rule.network === "udp"
      ? `会话 ${formatNumber(rule.udp_sessions)} · ↑ ${formatBytes(rule.bytes_in)} · ↓ ${formatBytes(rule.bytes_out)}`
      : `连接 ${formatNumber(rule.active_connections)} · ↑ ${formatBytes(rule.bytes_in)} · ↓ ${formatBytes(rule.bytes_out)}`;
    wrapper.append(createElement("small", "", traffic));
    if (rule.error) wrapper.append(createElement("span", "rule-error", rule.error));
    return wrapper;
  }

  function renderRules() {
    elements.rulesBody.replaceChildren();
    if (!state.rulesLoaded) return;

    const query = elements.ruleSearch.value.trim().toLocaleLowerCase("zh-CN");
    const visibleRules = state.rules
      .map((rule, index) => ({ rule, index }))
      .filter(({ rule }) => {
        if (!query) return true;
        return [
          rule.name,
          rule.listen,
          rule.target,
          rule.network,
          rule.id,
          ...rule.allowed_cidrs,
        ].join(" ").toLocaleLowerCase("zh-CN").includes(query);
      });

    elements.rulesLoading.hidden = true;
    elements.rulesEmpty.hidden = !(state.rulesLoaded && state.rules.length === 0);
    elements.rulesNoResults.hidden = !(state.rules.length > 0 && visibleRules.length === 0);

    visibleRules.forEach(({ rule, index }) => {
      const row = createElement("tr", rule.enabled ? "" : "disabled");

      const nameCell = createElement("td");
      nameCell.dataset.label = "规则";
      const nameWrap = createElement("div", "rule-name");
      nameWrap.append(createElement("strong", "", rule.name));
      nameWrap.append(createElement("small", "", rule.id ? `ID ${rule.id}` : "等待服务端分配 ID"));
      nameCell.append(nameWrap);

      const networkCell = createElement("td");
      networkCell.dataset.label = "协议";
      networkCell.append(makeProtocolBadges(rule.network));

      const endpointCell = createElement("td");
      endpointCell.dataset.label = "监听 → 目标";
      endpointCell.append(makeEndpoint(rule));

      const limitsCell = createElement("td");
      limitsCell.dataset.label = "连接限制";
      limitsCell.append(makeLimits(rule));

      const runtimeCell = createElement("td");
      runtimeCell.dataset.label = "实时状态";
      runtimeCell.append(makeRuntime(rule));

      const actionCell = createElement("td");
      actionCell.dataset.label = "操作";
      const actions = createElement("div", "row-actions");
      actions.append(makeRuleAction(
        "power",
        rule.enabled ? "停用规则" : "启用规则",
        "",
        () => toggleRule(index),
      ));
      actions.append(makeRuleAction("edit", "编辑规则", "", () => openRuleDialog(index)));
      actions.append(makeRuleAction("trash", "删除规则", "danger", () => deleteRule(index)));
      actionCell.append(actions);

      row.append(nameCell, networkCell, endpointCell, limitsCell, runtimeCell, actionCell);
      elements.rulesBody.append(row);
    });
    updateRuleCounts();
  }

  async function loadRules({ silent = false } = {}) {
    if (state.rulesLoading || !state.authenticated) return false;
    state.rulesLoading = true;
    if (!state.rulesLoaded) {
      elements.rulesLoading.hidden = false;
      elements.rulesEmpty.hidden = true;
      elements.rulesNoResults.hidden = true;
    }
    hintError(elements.rulesError, "");
    try {
      const rules = await api("/api/rules");
      if (!Array.isArray(rules)) throw new ApiError("规则接口返回格式无效。", 0);
      state.rules = rules.map(normalizedRule);
      state.rulesLoaded = true;
      renderRules();
      if (state.route === "overview" && state.status) updateOverview(state.status);
      return true;
    } catch (error) {
      if (!silent && !(error instanceof ApiError && error.status === 401)) {
        hintError(elements.rulesError, errorMessage(error));
        showToast(errorMessage(error), "error");
      }
      elements.rulesLoading.hidden = true;
      return false;
    } finally {
      state.rulesLoading = false;
    }
  }

  async function saveRules(candidateRules, successMessage) {
    if (state.rulesSaving) return false;
    state.rulesSaving = true;
    let rendered = false;
    try {
      const result = await api("/api/rules", {
        method: "PUT",
        body: { rules: candidateRules.map(rulePayload) },
      });
      let hasFreshRules = false;
      if (Array.isArray(result)) {
        state.rules = result.map(normalizedRule);
        hasFreshRules = true;
      } else if (result && Array.isArray(result.rules)) {
        state.rules = result.rules.map(normalizedRule);
        hasFreshRules = true;
      } else {
        state.rules = candidateRules.map(normalizedRule);
      }
      state.rulesLoaded = true;
      state.rulesSaving = false;
      renderRules();
      rendered = true;
      showToast(successMessage);
      if (!hasFreshRules) await loadRules({ silent: true });
      if (state.route === "overview") {
        await loadStatus({ silent: true });
        if (state.status) updateOverview(state.status);
      }
      return true;
    } catch (error) {
      showToast(`规则保存失败：${errorMessage(error)}`, "error", 6500);
      return false;
    } finally {
      state.rulesSaving = false;
      if (!rendered) renderRules();
    }
  }

  async function toggleRule(index) {
    const current = state.rules[index];
    if (!current || state.rulesSaving) return;
    const candidate = state.rules.map((rule) => ({ ...rule }));
    candidate[index].enabled = !current.enabled;
    await saveRules(
      candidate,
      current.enabled ? `已停用“${current.name}”` : `已启用“${current.name}”`,
    );
  }

  async function deleteRule(index) {
    const rule = state.rules[index];
    if (!rule || state.rulesSaving) return;
    const approved = await askConfirm({
      title: "删除转发规则？",
      message: `“${rule.name}”将被移除，保存后对应监听端口会停止服务。`,
      actionLabel: "确认删除",
      tone: "danger",
    });
    if (!approved) return;
    const candidate = state.rules.filter((_, itemIndex) => itemIndex !== index);
    await saveRules(candidate, `已删除“${rule.name}”`);
  }

  function parseCIDRs(value) {
    return value
      .split(/[\n,]+/)
      .map((item) => item.trim())
      .filter(Boolean);
  }

  function validateListen(value) {
    if (/\s/.test(value)) return "监听地址不能包含空格。";
    const match = value.match(/^(\[[^\]]+\]|[A-Za-z0-9.-]+)?:(\d{1,5})$/);
    if (!match) return "监听地址格式应类似 :443、0.0.0.0:443 或 [::]:443。";
    const port = Number(match[2]);
    if (port < 1 || port > 65535) return "监听端口必须在 1 到 65535 之间。";
    return "";
  }

  function validateTarget(value) {
    if (/\s/.test(value)) return "目标地址不能包含空格。";
    if (value.startsWith("[")) return "目标必须是 IPv4 字面量或解析到 IPv4 的主机名。";
    const separator = value.lastIndexOf(":");
    if (separator <= 0 || separator === value.length - 1) {
      return "目标地址格式应类似 203.0.113.1:443。";
    }
    const host = value.slice(0, separator);
    const port = Number(value.slice(separator + 1));
    if (!/^[A-Za-z0-9.-]+$/.test(host)) return "目标主机名或 IPv4 地址格式无效。";
    if (!Number.isInteger(port) || port < 1 || port > 65535) return "目标端口必须在 1 到 65535 之间。";
    return "";
  }

  function readRuleForm() {
    const network = $('input[name="rule-network"]:checked', elements.ruleForm);
    return {
      id: byId("rule-id").value,
      name: byId("rule-name").value.trim(),
      enabled: byId("rule-enabled").checked,
      network: network ? network.value : "both",
      listen: byId("rule-listen").value.trim(),
      target: byId("rule-target").value.trim(),
      allowed_cidrs: parseCIDRs(byId("rule-cidrs").value),
      max_connections: Number(byId("rule-max-connections").value),
      udp_timeout: Number(byId("rule-udp-timeout").value),
    };
  }

  function validateRule(rule) {
    if (!rule.name) return "请填写规则名称。";
    if (rule.name.length > 80) return "规则名称不能超过 80 个字符。";
    if (!SAFE_NETWORKS.has(rule.network)) return "请选择转发协议。";
    const listenError = validateListen(rule.listen);
    if (listenError) return listenError;
    const targetError = validateTarget(rule.target);
    if (targetError) return targetError;
    const invalidCIDR = rule.allowed_cidrs.find(
      (cidr) => !/^[0-9A-Fa-f:.]+\/\d{1,3}$/.test(cidr),
    );
    if (invalidCIDR) return `CIDR“${invalidCIDR}”格式无效。`;
    if (
      !Number.isInteger(rule.max_connections) ||
      rule.max_connections < 0 ||
      rule.max_connections > MAX_CONNECTIONS
    ) {
      return `最大连接数必须在 0 到 ${MAX_CONNECTIONS} 之间；0 表示采用默认值 ${DEFAULT_MAX_CONNECTIONS}。`;
    }
    if (
      !Number.isInteger(rule.udp_timeout) ||
      rule.udp_timeout < 1 ||
      rule.udp_timeout > 86400
    ) {
      return "UDP 空闲超时必须在 1 到 86400 秒之间。";
    }
    return "";
  }

  function currentDraftProbeKey(rule = readRuleForm()) {
    return probeCacheKey(rule || readRuleForm());
  }

  function stopDraftProbeRequest() {
    const draft = state.ruleDraftProbe;
    draft.token += 1;
    if (draft.controller) {
      draft.controller.abort();
      draft.controller = null;
    }
  }

  function setDraftProbeNotice(message, tone = "") {
    elements.ruleProbeNotice.textContent = message;
    elements.ruleProbeNotice.className = `probe-notice${tone ? ` ${tone}` : ""}`;
  }

  function renderDraftProbePlaceholder(phase, message) {
    const invalidated = phase === "invalidated";
    setProbeCard(elements.ruleListenProbe, invalidated ? "warn" : "neutral");
    setProbeBadge(elements.ruleListenProbeBadge, invalidated ? "需重新检测" : "未检测", invalidated ? "warn" : "neutral");
    elements.ruleListenProbeMetric.textContent = invalidated ? "旧入口结果已清除" : "尚未检查入口";
    elements.ruleListenProbeMessage.textContent = invalidated
      ? "配置已变化，请重新检测入口监听状态。"
      : "检测会检查端口可绑定或当前监听状态，不主动连接入口；外部客户端链路需另行测量。";
    elements.ruleListenProbeTime.textContent = "";

    setProbeCard(elements.ruleTargetProbe, invalidated ? "warn" : "neutral");
    setProbeBadge(elements.ruleTargetProbeBadge, invalidated ? "需重新检测" : "未检测", invalidated ? "warn" : "neutral");
    elements.ruleTargetProbeMetric.textContent = invalidated ? "旧目标结果已清除" : "尚未检查目标";
    elements.ruleTargetProbeMessage.textContent = invalidated
      ? "配置已变化，请重新检测目标握手范围。"
      : "TCP 结果为握手；UDP 不使用随机报文，需真实双向流量验证。";
    elements.ruleTargetProbeTime.textContent = "";
    elements.ruleTargetProbeAddress.hidden = true;
    elements.ruleTargetProbeAddress.textContent = "";
    setDraftProbeNotice(message || "修改监听地址、目标、协议或探测相关配置后，需要重新检测。");
  }

  function renderDraftProbeResult(result) {
    const network = readRuleForm().network;
    const listenPresentation = probeStatusPresentation(result.listen.status);
    let listenMetric = "本机监听状态未验证";
    if (result.listen.latency_ms !== null) {
      listenMetric = `本机 TCP ${formatProbeLatency(result.listen.latency_ms)}`;
    } else if (result.listen.status === "listening") {
      listenMetric = "本机监听正常 · 外部延迟需客户端测量";
    } else if (result.listen.status === "available") {
      listenMetric = network === "udp" ? "UDP 端口可绑定" : "TCP 端口可绑定";
    } else if (result.listen.status === "error") {
      listenMetric = "入口监听检查失败";
    } else if (result.listen.status === "unverified") {
      listenMetric = "入口状态未验证";
    }
    if (result.listen.udp_status === "unverified") {
      listenMetric += " · UDP 未验证";
    }
    setProbeCard(elements.ruleListenProbe, listenPresentation.tone);
    setProbeBadge(elements.ruleListenProbeBadge, listenPresentation.text, listenPresentation.tone);
    elements.ruleListenProbeMetric.textContent = listenMetric;
    elements.ruleListenProbeMessage.textContent = result.listen.message
      || "入口检测只在本机执行，不代表外部客户端到入口的链路已通过。";
    elements.ruleListenProbeTime.textContent = formatProbeTime(result.checked_at);

    const targetPresentation = probeStatusPresentation(result.target.status);
    if (result.target.status === "unverified" && network === "udp") {
      targetPresentation.text = "UDP 未验证";
    }
    let targetMetric = "TCP 未计时";
    if (result.target.latency_ms !== null) {
      targetMetric = `TCP ${formatProbeLatency(result.target.latency_ms)}`;
    } else if (result.target.status === "unverified") {
      targetMetric = "UDP 无通用握手";
    } else if (result.target.status === "error") {
      targetMetric = "解析或握手失败";
    }
    if (result.target.udp_status === "unverified") {
      targetMetric += " · UDP 未验证";
    }
    setProbeCard(elements.ruleTargetProbe, targetPresentation.tone);
    setProbeBadge(elements.ruleTargetProbeBadge, targetPresentation.text, targetPresentation.tone);
    elements.ruleTargetProbeMetric.textContent = targetMetric;
    elements.ruleTargetProbeMessage.textContent = result.target.message
      || "TCP 握手不代表应用协议、认证或 UDP 双向流量已通过。";
    elements.ruleTargetProbeTime.textContent = formatProbeTime(result.checked_at);
    elements.ruleTargetProbeAddress.hidden = !result.target_address;
    elements.ruleTargetProbeAddress.textContent = result.target_address
      ? `解析目标 ${result.target_address}`
      : "";
    setDraftProbeNotice(
      "入口只确认本机监听状态，不测量外部网络延迟；目标毫秒仅为本机到目标的 TCP 握手。UDP 需真实双向流量验证。",
    );
  }

  function renderDraftProbeError(message, time) {
    setProbeCard(elements.ruleListenProbe, "bad");
    setProbeBadge(elements.ruleListenProbeBadge, "检测失败", "bad");
    elements.ruleListenProbeMetric.textContent = "未取得入口结果";
    elements.ruleListenProbeMessage.textContent = message;
    elements.ruleListenProbeTime.textContent = time;

    setProbeCard(elements.ruleTargetProbe, "bad");
    setProbeBadge(elements.ruleTargetProbeBadge, "检测失败", "bad");
    elements.ruleTargetProbeMetric.textContent = "未取得目标结果";
    elements.ruleTargetProbeMessage.textContent = message;
    elements.ruleTargetProbeTime.textContent = time;
    elements.ruleTargetProbeAddress.hidden = true;
    elements.ruleTargetProbeAddress.textContent = "";
    setDraftProbeNotice(message, "error");
  }

  function renderDraftProbe() {
    const draft = state.ruleDraftProbe;
    const loading = draft.phase === "loading";
    elements.ruleProbe.disabled = loading || state.rulesSaving;
    const label = $("span", elements.ruleProbe);
    if (label) label.textContent = loading ? "检测中…" : "检测连通性";

    if (loading) {
      setProbeCard(elements.ruleListenProbe, "loading");
      setProbeBadge(elements.ruleListenProbeBadge, "检测中…", "neutral");
      elements.ruleListenProbeMetric.textContent = "正在检查本机监听";
      elements.ruleListenProbeMessage.textContent = "正在检查端口绑定与当前监听状态，不会主动连接入口。";
      elements.ruleListenProbeTime.textContent = "请求进行中";

      setProbeCard(elements.ruleTargetProbe, "loading");
      setProbeBadge(elements.ruleTargetProbeBadge, "检测中…", "neutral");
      elements.ruleTargetProbeMetric.textContent = "正在检查目标 TCP";
      elements.ruleTargetProbeMessage.textContent = "UDP 不使用随机测试报文，结果会明确标为未验证。";
      elements.ruleTargetProbeTime.textContent = "请求进行中";
      elements.ruleTargetProbeAddress.hidden = true;
      elements.ruleTargetProbeAddress.textContent = "";
      setDraftProbeNotice("检测只从面板所在主机发起，不能证明公网链路、应用认证或 UDP 双向流量可用。");
      return;
    }
    if (draft.phase === "result" && draft.result) {
      renderDraftProbeResult(draft.result);
      return;
    }
    if (draft.phase === "error") {
      renderDraftProbeError(draft.error || "连通性检测未能完成。", formatProbeTime(new Date().toISOString()));
      return;
    }
    renderDraftProbePlaceholder(
      draft.phase,
      draft.phase === "invalidated"
        ? "监听地址、目标、协议或探测相关配置已更改，旧检测结果已清除。"
        : "",
    );
  }

  function invalidateDraftProbeResult() {
    const draft = state.ruleDraftProbe;
    if (!elements.ruleDialog.open) return;
    const hadResult = ["loading", "result", "error"].includes(draft.phase);
    stopDraftProbeRequest();
    draft.key = currentDraftProbeKey();
    draft.phase = hadResult ? "invalidated" : "idle";
    draft.result = null;
    draft.error = "";
    renderDraftProbe();
  }

  async function probeDraftRule() {
    if (!elements.ruleDialog.open) return;
    const rule = readRuleForm();
    const validationError = validateRule(rule);
    if (validationError) {
      hintError(elements.ruleFormError, validationError);
      return;
    }
    hintError(elements.ruleFormError, "");
    const draft = state.ruleDraftProbe;
    stopDraftProbeRequest();
    const token = draft.token;
    const key = probeCacheKey(rule);
    const sequence = beginRuleProbe(key);
    const controller = new AbortController();
    draft.key = key;
    draft.phase = "loading";
    draft.result = null;
    draft.error = "";
    draft.controller = controller;
    renderDraftProbe();
    try {
      const response = await api("/api/rules/probe", {
        method: "POST",
        body: { rule: probePayload(rule) },
        signal: controller.signal,
      });
      if (draft.token !== token || !elements.ruleDialog.open) return;
      if (currentDraftProbeKey() !== key) return;
      const result = normalizedProbeResult(response);
      if (!rememberRuleProbe(key, sequence, {
        result,
        receivedAt: result.checked_at || new Date().toISOString(),
      })) {
        draft.phase = "invalidated";
        renderDraftProbe();
        return;
      }
      draft.phase = "result";
      draft.result = result;
      draft.error = "";
      renderDraftProbe();
    } catch (error) {
      if (draft.token !== token || !elements.ruleDialog.open) return;
      if (currentDraftProbeKey() !== key) return;
      if (!rememberRuleProbe(key, sequence, {
        error: errorMessage(error),
        receivedAt: new Date().toISOString(),
      })) {
        draft.phase = "invalidated";
        renderDraftProbe();
        return;
      }
      draft.phase = "error";
      draft.result = null;
      draft.error = errorMessage(error);
      renderDraftProbe();
    } finally {
      if (draft.token === token) {
        draft.controller = null;
        if (draft.phase === "loading") {
          draft.phase = "error";
          draft.error = "连通性检测未能完成，请稍后重试。";
          renderDraftProbe();
        }
      }
    }
  }

  function primeDraftProbe(rule) {
    const draft = state.ruleDraftProbe;
    stopDraftProbeRequest();
    draft.key = currentDraftProbeKey(rule);
    draft.phase = "idle";
    draft.result = null;
    draft.error = "";
    draft.controller = null;
    const cached = state.ruleProbeResults.get(draft.key);
    if (cached && cached.result) {
      draft.phase = "result";
      draft.result = cached.result;
    } else if (cached && cached.error) {
      draft.phase = "error";
      draft.error = cached.error;
    }
    renderDraftProbe();
  }

  function openRuleDialog(index = -1) {
    state.editingRuleIndex = Number.isInteger(index) ? index : -1;
    const rule = state.editingRuleIndex >= 0 ? state.rules[state.editingRuleIndex] : null;
    elements.ruleForm.reset();
    byId("rule-id").value = rule ? rule.id : "";
    byId("rule-name").value = rule ? rule.name : "";
    byId("rule-enabled").checked = rule ? rule.enabled : true;
    byId("rule-listen").value = rule ? rule.listen : "";
    byId("rule-target").value = rule ? rule.target : "";
    byId("rule-cidrs").value = rule ? rule.allowed_cidrs.join("\n") : "";
    byId("rule-max-connections").value = rule ? String(rule.max_connections) : String(DEFAULT_MAX_CONNECTIONS);
    byId("rule-udp-timeout").value = rule ? String(rule.udp_timeout) : "60";
    const network = rule ? rule.network : "both";
    const networkInput = $(`input[name="rule-network"][value="${network}"]`, elements.ruleForm);
    if (networkInput) networkInput.checked = true;
    elements.ruleDialogTitle.textContent = rule ? "编辑转发规则" : "新建转发规则";
    hintError(elements.ruleFormError, "");
    primeDraftProbe(rule);
    elements.ruleDialog.showModal();
    window.setTimeout(() => byId("rule-name").focus(), 20);
  }

  function closeRuleDialog() {
    stopDraftProbeRequest();
    state.ruleDraftProbe.phase = "idle";
    state.ruleDraftProbe.result = null;
    state.ruleDraftProbe.error = "";
    if (elements.ruleDialog.open) elements.ruleDialog.close();
    state.editingRuleIndex = -1;
  }

  function findSavedRuleForProbe(rule, previousIDs, editRuleID) {
    if (editRuleID) {
      const byID = state.rules.find((item) => item.id === editRuleID);
      if (byID) return byID;
    }
    const matches = state.rules.filter((item) => (
      item.name === rule.name
      && item.network === rule.network
      && item.listen === rule.listen
      && item.target === rule.target
    ));
    return matches.find((item) => item.id && !previousIDs.has(item.id))
      || matches[matches.length - 1]
      || null;
  }

  async function submitRule(event) {
    event.preventDefault();
    const rule = readRuleForm();
    const validationError = validateRule(rule);
    if (validationError) {
      hintError(elements.ruleFormError, validationError);
      return;
    }
    hintError(elements.ruleFormError, "");
    elements.ruleSubmit.disabled = true;
    const label = $("span", elements.ruleSubmit);
    if (label) label.textContent = "保存中…";

    const previousIDs = new Set(state.rules.map((item) => item.id).filter(Boolean));
    let candidate;
    const editIndex = state.editingRuleIndex;
    const editRuleID = editIndex >= 0 && state.rules[editIndex] ? state.rules[editIndex].id : "";
    if (editIndex >= 0 && state.rules[editIndex]) {
      candidate = state.rules.map((item) => ({ ...item }));
      candidate[editIndex] = { ...candidate[editIndex], ...rule, id: candidate[editIndex].id || rule.id };
    } else {
      candidate = [...state.rules.map((item) => ({ ...item })), rule];
    }

    try {
      const saved = await saveRules(
        candidate,
        editIndex >= 0 ? `已保存“${rule.name}”` : `已创建“${rule.name}”`,
      );
      if (saved) {
        const savedRule = findSavedRuleForProbe(rule, previousIDs, editRuleID);
        closeRuleDialog();
        if (savedRule) {
          window.setTimeout(() => probeRule(savedRule, { announce: false }), 0);
        }
      }
    } finally {
      elements.ruleSubmit.disabled = false;
      if (label) label.textContent = "保存规则";
    }
  }

  function askConfirm({ title, message, actionLabel = "确认", tone = "danger" }) {
    if (state.confirmResolve) {
      state.confirmResolve(false);
      state.confirmResolve = null;
    }
    elements.confirmTitle.textContent = title;
    elements.confirmMessage.textContent = message;
    elements.confirmAction.textContent = actionLabel;
    elements.confirmAction.className = `button ${tone === "primary" ? "primary" : "danger"}`;
    elements.confirmAction.disabled = false;
    hintError(elements.confirmError, "");
    elements.confirmDialog.showModal();
    return new Promise((resolve) => {
      state.confirmResolve = resolve;
    });
  }

  function finishConfirm(result) {
    if (elements.confirmDialog.open) elements.confirmDialog.close();
    elements.confirmAction.disabled = false;
    const resolve = state.confirmResolve;
    state.confirmResolve = null;
    if (resolve) resolve(result);
  }

  function openPasswordDialog() {
    elements.passwordForm.reset();
    hintError(elements.passwordFormError, "");
    elements.passwordDialog.showModal();
    window.setTimeout(() => byId("password-old").focus(), 20);
  }

  function closePasswordDialog() {
    if (!elements.passwordDialog.open) return;
    elements.passwordForm.reset();
    hintError(elements.passwordFormError, "");
    elements.passwordDialog.close();
  }

  async function submitPassword(event) {
    event.preventDefault();
    hintError(elements.passwordFormError, "");
    const oldPassword = byId("password-old").value;
    const newPassword = byId("password-new").value;
    const confirmation = byId("password-confirm").value;
    if (!oldPassword || !newPassword) {
      hintError(elements.passwordFormError, "请填写当前密码和新密码。");
      return;
    }
    if (newPassword !== confirmation) {
      hintError(elements.passwordFormError, "两次输入的新密码不一致。");
      byId("password-confirm").focus();
      return;
    }
    if (newPassword === oldPassword) {
      hintError(elements.passwordFormError, "新密码不能与当前密码相同。");
      return;
    }

    const submitLabel = $("span", elements.passwordSubmit);
    elements.passwordSubmit.disabled = true;
    if (submitLabel) submitLabel.textContent = "更新中…";
    try {
      await api("/api/password", {
        method: "POST",
        body: {
          old_password: oldPassword,
          new_password: newPassword,
        },
        authRedirect: false,
      });
      closePasswordDialog();
      showLogin();
      showToast("密码已修改，所有管理会话已失效，请使用新密码登录。", "success", 6500);
    } catch (error) {
      byId("password-old").value = "";
      byId("password-new").value = "";
      byId("password-confirm").value = "";
      hintError(elements.passwordFormError, errorMessage(error));
      byId("password-old").focus();
    } finally {
      elements.passwordSubmit.disabled = false;
      if (submitLabel) submitLabel.textContent = "更新密码";
    }
  }

  function exportConfig() {
    downloadConfiguration().catch((error) => {
      showToast(`导出失败：${errorMessage(error)}`, "error");
    });
  }

  async function downloadConfiguration() {
    const config = await api("/api/config/export");
    const blob = new Blob([`${JSON.stringify(config, null, 2)}\n`], {
      type: "application/json;charset=utf-8",
    });
    const now = new Date();
    const stamp = [
      now.getFullYear(),
      String(now.getMonth() + 1).padStart(2, "0"),
      String(now.getDate()).padStart(2, "0"),
      "-",
      String(now.getHours()).padStart(2, "0"),
      String(now.getMinutes()).padStart(2, "0"),
    ].join("");
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = `relaydeck-config-${stamp}.json`;
    document.body.append(anchor);
    anchor.click();
    anchor.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
    showToast("配置已导出，含转发节点凭据但不含管理员密码，请妥善保存。", "success", 6500);
  }

  async function importRules(file) {
    if (!file) return;
    if (file.size > 2 * 1024 * 1024) {
      showToast("导入文件不能超过 2 MB。", "error");
      return;
    }
    try {
      const text = await file.text();
      let parsed;
      try {
        parsed = JSON.parse(text);
      } catch {
        throw new Error("JSON 解析失败，请检查文件格式。");
      }
      const candidateRaw = Array.isArray(parsed)
        ? parsed
        : parsed && Array.isArray(parsed.rules)
          ? parsed.rules
          : null;
      if (!candidateRaw) throw new Error("JSON 中没有可识别的 rules 数组。");
      if (candidateRaw.length > 1000) throw new Error("单次最多导入 1000 条规则。");

      const candidate = candidateRaw.map(normalizedRule);
      for (let index = 0; index < candidate.length; index += 1) {
        const problem = validateRule(candidate[index]);
        if (problem) throw new Error(`第 ${index + 1} 条规则无效：${problem}`);
      }

      const approved = await askConfirm({
        title: "替换全部转发规则？",
        message: `将导入 ${candidate.length} 条规则，并整体替换现有 ${state.rules.length} 条规则。服务端将先校验，再应用新配置。`,
        actionLabel: "确认导入",
        tone: "primary",
      });
      if (!approved) return;
      await saveRules(candidate, `已导入 ${candidate.length} 条转发规则`);
    } catch (error) {
      showToast(`导入失败：${errorMessage(error)}`, "error", 6500);
    } finally {
      elements.importFile.value = "";
    }
  }

  function prettyJSON(value) {
    if (value === undefined || value === null || value === "") return "{}";
    try {
      return JSON.stringify(value, null, 2);
    } catch {
      return "{}";
    }
  }

  function setEngineDirty(dirty) {
    state.engineDirty = dirty;
    elements.engineDirty.hidden = !dirty;
  }

  function invalidateEngineRequests() {
    state.engineRequestEpoch += 1;
    state.engineRequestSequence += 1;
    state.engineLoading = false;
  }

  function isCurrentEngineRequest(epoch, sequence) {
    return state.authenticated &&
      epoch === state.engineRequestEpoch &&
      sequence === state.engineRequestSequence;
  }

  function normalizeEngineInstallation(value) {
    if (!value || typeof value !== "object" || Array.isArray(value)) return null;
    const validPhases = new Set([
      "idle",
      "downloading",
      "verifying",
      "extracting",
      "checking",
      "complete",
      "failed",
    ]);
    const phase = validPhases.has(value.phase) ? value.phase : "idle";
    return {
      supported: value.supported === true,
      version: typeof value.version === "string" && value.version ? value.version : "1.14.4",
      phase,
      running: value.running === true,
      downloaded_bytes: Number.isFinite(Number(value.downloaded_bytes))
        ? Math.max(0, Number(value.downloaded_bytes))
        : 0,
      total_bytes: Number.isFinite(Number(value.total_bytes))
        ? Math.max(0, Number(value.total_bytes))
        : 0,
      error: typeof value.error === "string" ? value.error : "",
    };
  }

  function installationIsActive(installation) {
    if (!installation) return false;
    return installation.running || [
      "downloading",
      "verifying",
      "extracting",
      "checking",
    ].includes(installation.phase);
  }

  function installationByteProgress(installation) {
    if (!installation || installation.downloaded_bytes <= 0) return "";
    if (installation.total_bytes > 0) {
      return `（${formatBytes(installation.downloaded_bytes)} / ${formatBytes(installation.total_bytes)}）`;
    }
    return `（已下载 ${formatBytes(installation.downloaded_bytes)}）`;
  }

  function renderEngineInstallation() {
    const installation = state.engineInstallation;
    const active = installationIsActive(installation);
    const phase = installation ? installation.phase : "idle";
    let message = "";

    if (state.engineInstallRequestError) {
      message = `安装请求失败：${state.engineInstallRequestError}`;
    } else if (state.engineInstallRequesting) {
      message = "正在提交安装任务并校验运行环境…";
    } else if (!installation) {
      message = "服务端未返回安装能力；可展开“手动安装（SSH 备用）”。";
    } else if (!installation.supported) {
      message = installation.error
        || "当前系统架构或自定义内核路径不支持一键安装，可展开备用命令。";
    } else if (phase === "downloading") {
      message = `正在下载 sing-box ${installation.version}${installationByteProgress(installation)}…`;
    } else if (phase === "verifying") {
      message = `正在校验官方 Release 的 SHA-256${installationByteProgress(installation)}…`;
    } else if (phase === "extracting") {
      message = `校验通过，正在解压内核文件${installationByteProgress(installation)}…`;
    } else if (phase === "checking") {
      message = "正在检测 sing-box 版本信息和当前安装阶段…";
    } else if (phase === "complete") {
      message = `sing-box ${installation.version} 安装完成，正在等待内核状态刷新。`;
    } else if (phase === "failed") {
      message = `安装失败：${installation.error || "未知错误"}。可点击“重试安装 sing-box”。`;
    } else if (active) {
      message = "安装任务运行中…";
    } else {
      message = "准备就绪。点击“一键安装 sing-box”开始，无需 SSH 或重启。";
    }

    elements.engineInstallStatus.textContent = message;
    elements.engineInstallStatus.dataset.phase = state.engineInstallRequestError ? "failed" : phase;
    elements.engineInstallHelp.setAttribute("aria-busy", String(state.engineInstallRequesting || active));
  }

  function updateEngineControls() {
    const available = Boolean(state.engine && state.engine.available);
    const running = Boolean(state.engine && state.engine.running);
    const saving = state.engineSaving;
    const installation = state.engineInstallation;
    const installActive = installationIsActive(installation);
    const installRequesting = state.engineInstallRequesting;
    const installSupported = Boolean(installation && installation.supported);
    const missingReason = "sing-box 未安装，无法保存或启用。请先点击上方“一键安装 sing-box”，完成安装并填写参数后再保存。";
    const saveLabel = $("span", elements.engineSave);
    const installLabel = $("span", elements.engineInstall);

    elements.engineEnabled.disabled = !available || saving;
    elements.engineSave.disabled = !available || saving;
    elements.engineStop.disabled = !running || saving;
    elements.engineConfig.disabled = saving;
    elements.engineTemplate.disabled = state.templates.length === 0 || saving;
    elements.engineApplyTemplate.disabled = !elements.engineTemplate.value || saving;
    elements.engineCopyInstall.disabled = saving;
    elements.engineRecheck.disabled = saving || state.engineLoading;
    elements.engineRefresh.disabled = saving || state.engineLoading;
    elements.engineInstall.disabled = !installSupported || available || saving || installRequesting || installActive;

    if (installLabel) {
      if (installRequesting) {
        installLabel.textContent = "正在启动安装…";
      } else if (installActive) {
        installLabel.textContent = "正在安装 sing-box…";
      } else if (installation && installation.phase === "failed") {
        installLabel.textContent = "重试安装 sing-box";
      } else if (available) {
        installLabel.textContent = "sing-box 已安装";
      } else {
        installLabel.textContent = "一键安装 sing-box";
      }
    }

    if (saveLabel) {
      if (saving) {
        saveLabel.textContent = elements.engineEnabled.checked ? "校验并启用中…" : "校验并保存中…";
      } else if (!state.engine) {
        saveLabel.textContent = "读取内核状态…";
      } else if (!available) {
        saveLabel.textContent = "需先安装 sing-box";
      } else {
        saveLabel.textContent = elements.engineEnabled.checked ? "保存并启用" : "校验并保存";
      }
    }

    elements.engineSave.title = saving
      ? "正在校验并保存配置，请稍候。"
      : !available
        ? missingReason
        : elements.engineEnabled.checked
          ? "校验配置并保存为启用状态。勾选本身不会立即启动内核。"
          : "校验并保存配置，保持当前停用状态。";
    elements.engineEnabled.title = !available
      ? missingReason
      : "勾选仅表示保存后启用；需点击保存按钮才会校验并应用。";
    elements.engineInstall.title = !installation
      ? "服务端未返回安装能力，请使用折叠的手动安装作为备用。"
      : !installSupported
        ? "当前系统架构或自定义内核路径不支持一键安装，可展开备用命令。"
        : available
          ? "sing-box 已安装。"
          : "通过面板下载、校验并安装 sing-box；不会自动启用或修改配置。";
    elements.engineInstallHelp.hidden = !state.engine || available;
    renderEngineInstallation();
  }

  function renderTemplates() {
    const previous = elements.engineTemplate.value;
    elements.engineTemplate.replaceChildren();
    elements.engineTemplate.append(createElement("option", "", "选择内置模板"));
    const placeholder = elements.engineTemplate.firstElementChild;
    placeholder.value = "";
    state.templates.forEach((template) => {
      if (!template || typeof template.id !== "string") return;
      const option = createElement("option", "", template.name || template.id);
      option.value = template.id;
      elements.engineTemplate.append(option);
    });
    if (state.templates.some((template) => template.id === previous)) {
      elements.engineTemplate.value = previous;
    }
    const empty = state.templates.length === 0;
    updateEngineControls();
    if (empty) {
      elements.engineTemplateDescription.textContent = "当前服务没有提供内置模板，可直接编辑 JSON 配置。";
    }
  }

  function renderEngine(engine, { replaceEditor = true } = {}) {
    state.engine = engine;
    state.engineLoaded = true;
    state.engineInstallation = normalizeEngineInstallation(engine && engine.installation);
    state.engineInstallRequestError = "";
    const available = Boolean(engine && engine.available);
    const running = Boolean(engine && engine.running);
    const enabled = Boolean(engine && engine.enabled);
    const version = engine && typeof engine.version === "string" ? engine.version : "";
    const error = engine && typeof engine.error === "string" ? engine.error : "";

    setText("engine-available", available ? "已安装" : "未安装");
    setText("engine-bin-version", version || (available ? "版本信息未返回" : "需要单独安装 sing-box"));
    setBadge(
      byId("engine-available-badge"),
      available ? "READY" : "OFF",
      available ? "" : "bad",
    );
    setText("engine-running", running ? "运行中" : enabled ? "已启用，未运行" : "已停用");
    setText(
      "engine-enabled-note",
      enabled ? "配置保存为启用状态" : "配置保存为停用状态",
    );
    setBadge(
      byId("engine-running-badge"),
      running ? "RUNNING" : enabled ? "PENDING" : "STOPPED",
      running ? "" : enabled ? "neutral" : "neutral",
    );

    if (!state.engineDirty) {
      elements.engineEnabled.checked = enabled;
      if (replaceEditor) {
        elements.engineConfig.value = prettyJSON(engine && engine.config);
      }
    }

    let logs = [];
    if (engine && Array.isArray(engine.logs)) {
      logs = engine.logs.slice(-200).map((entry) => {
        if (typeof entry === "string") return entry;
        try {
          return JSON.stringify(entry);
        } catch {
          return String(entry);
        }
      });
    }
    elements.engineLogs.textContent = logs.length > 0 ? logs.join("\n") : "暂无日志";
    hintError(elements.engineAlert, error);
    updateEngineControls();
  }

  async function loadEngine({ silent = false, replaceEditor = false } = {}) {
    if (
      state.engineLoading ||
      state.engineSaving ||
      state.engineInstallRequesting ||
      !state.authenticated
    ) {
      return false;
    }
    const requestEpoch = state.engineRequestEpoch;
    const requestSequence = ++state.engineRequestSequence;
    state.engineLoading = true;
    const firstLoad = !state.engineLoaded;
    updateEngineControls();
    try {
      const requests = [api("/api/engine", { authRedirect: false })];
      if (!state.templatesLoaded) requests.push(api("/api/engine/templates", { authRedirect: false }));
      const results = await Promise.allSettled(requests);
      if (!isCurrentEngineRequest(requestEpoch, requestSequence)) return false;
      if (results[0].status === "rejected") throw results[0].reason;
      const engine = results[0].value;
      if (!engine || typeof engine !== "object") {
        throw new ApiError("内核接口返回格式无效。", 0);
      }
      if (results[1] && results[1].status === "fulfilled" && Array.isArray(results[1].value)) {
        state.templates = results[1].value.filter(
          (template) => template && typeof template.id === "string",
        );
        state.templatesLoaded = true;
        renderTemplates();
      } else if (results[1] && results[1].status === "rejected") {
        state.templates = [];
        state.templatesLoaded = true;
        renderTemplates();
      }
      renderEngine(engine, { replaceEditor: firstLoad || replaceEditor });
      if (state.engineDirty && !silent) {
        showToast("已刷新运行状态，未保存的 JSON 配置与启用勾选已保留。", "success");
      }
      return true;
    } catch (error) {
      if (isCurrentEngineRequest(requestEpoch, requestSequence) && error instanceof ApiError && error.status === 401) {
        showLogin("登录状态已失效，请重新登录。");
        return false;
      }
      if (
        isCurrentEngineRequest(requestEpoch, requestSequence) &&
        !silent &&
        !(error instanceof ApiError && error.status === 401)
      ) {
        hintError(elements.engineAlert, errorMessage(error));
        showToast(errorMessage(error), "error");
      }
      return false;
    } finally {
      if (isCurrentEngineRequest(requestEpoch, requestSequence)) {
        state.engineLoading = false;
        updateEngineControls();
      }
    }
  }

  function selectedTemplate() {
    const id = elements.engineTemplate.value;
    return state.templates.find((template) => template.id === id) || null;
  }

  function updateTemplateDescription() {
    const template = selectedTemplate();
    elements.engineTemplateDescription.textContent = template
      ? template.description || "已选择模板，应用后可继续编辑。"
      : "选择模板后可继续编辑。";
    updateEngineControls();
  }

  async function applyTemplate() {
    const template = selectedTemplate();
    if (!template) return;
    if (state.engineDirty) {
      const approved = await askConfirm({
        title: "替换当前 JSON？",
        message: "应用模板会覆盖编辑器中尚未保存的配置。",
        actionLabel: "应用模板",
        tone: "primary",
      });
      if (!approved) return;
    }
    elements.engineConfig.value = prettyJSON(template.config);
    setEngineDirty(true);
    hintError(elements.engineFormError, "");
    const templateName = template.name || template.id;
    if (state.engine && state.engine.available) {
      showToast(`已载入模板“${templateName}”，保存后生效。`);
    } else {
      showToast(`已载入模板“${templateName}”。sing-box 尚未安装，请先点击“一键安装 sing-box”，安装后再保存并启用。`, "error", 7000);
    }
    updateEngineControls();
  }

  function parseEngineConfig() {
    const text = elements.engineConfig.value.trim();
    if (!text) return {};
    try {
      return JSON.parse(text);
    } catch (error) {
      throw new Error(`JSON 格式错误：${error.message}`);
    }
  }

  function selectEngineInstallCommand() {
    elements.engineInstallCommand.focus();
    const selection = window.getSelection();
    if (!selection) return;
    const range = document.createRange();
    range.selectNodeContents(elements.engineInstallCommand);
    selection.removeAllRanges();
    selection.addRange(range);
  }

  async function copyEngineInstallCommand() {
    try {
      if (!navigator.clipboard || typeof navigator.clipboard.writeText !== "function") {
        throw new Error("Clipboard API unavailable");
      }
      await navigator.clipboard.writeText(ENGINE_INSTALL_COMMAND);
      showToast("安装命令已复制，请以 SSH root 身份粘贴运行。");
      return;
    } catch {
      selectEngineInstallCommand();
    }

    let copied = false;
    try {
      copied = typeof document.execCommand === "function" && document.execCommand("copy");
    } catch {
      copied = false;
    }
    if (copied) {
      showToast("安装命令已复制，请以 SSH root 身份粘贴运行。");
      return;
    }
    showToast("自动复制失败，安装命令已选中，请按 Ctrl+C 复制。", "error", 7000);
  }

  async function installEngine() {
    const installation = state.engineInstallation;
    if (
      state.engineInstallRequesting ||
      state.engineSaving ||
      !state.engine ||
      state.engine.available ||
      !installation ||
      !installation.supported ||
      installationIsActive(installation)
    ) {
      return false;
    }

    state.engineInstallRequesting = true;
    state.engineInstallRequestError = "";
    invalidateEngineRequests();
    const requestEpoch = state.engineRequestEpoch;
    updateEngineControls();
    try {
      const status = await api("/api/engine/install", {
        method: "POST",
        authRedirect: false,
        body: {},
      });
      if (!state.authenticated || requestEpoch !== state.engineRequestEpoch) return false;
      const nextInstallation = normalizeEngineInstallation(status);
      if (!nextInstallation) {
        throw new ApiError("安装接口没有返回有效的任务状态。", 0);
      }
      state.engineInstallRequesting = false;
      state.engineInstallation = nextInstallation;
      updateEngineControls();
      showToast(
        nextInstallation.phase === "complete"
          ? "sing-box 已安装，正在刷新内核状态。"
          : "sing-box 安装任务已开始，可在当前页查看进度。",
      );
      await loadEngine({ silent: true });
      return true;
    } catch (error) {
      if (!state.authenticated || requestEpoch !== state.engineRequestEpoch) return false;
      if (error instanceof ApiError && error.status === 401) {
        showLogin("登录状态已失效，请重新登录。");
        return false;
      }
      state.engineInstallRequesting = false;
      const message = errorMessage(error);
      if (error instanceof ApiError && error.status === 409) {
        await loadEngine({ silent: true, replaceEditor: false });
        if (!state.authenticated || requestEpoch !== state.engineRequestEpoch) return false;
        if (state.engine && state.engine.available) {
          showToast("检测到 sing-box 已安装，内核状态已刷新。");
        } else {
          state.engineInstallRequestError = message;
          updateEngineControls();
          showToast(`无法开始安装：${message}`, "error", 7000);
        }
        return false;
      }
      state.engineInstallRequestError = message;
      if (state.engineInstallation) {
        state.engineInstallation = {
          ...state.engineInstallation,
          phase: "failed",
          running: false,
          error: message,
        };
      }
      updateEngineControls();
      showToast(`sing-box 安装失败：${message}`, "error", 7000);
      return false;
    } finally {
      if (requestEpoch === state.engineRequestEpoch) {
        state.engineInstallRequesting = false;
        updateEngineControls();
      }
    }
  }

  async function saveEngine() {
    if (state.engineSaving || !state.engine) return;
    if (!state.engine.available) {
      const message = "未检测到 sing-box，当前无法校验或保存内核配置。请先点击上方“一键安装 sing-box”，安装完成后填写参数并保存。";
      hintError(elements.engineFormError, message);
      showToast(message, "error", 7000);
      return;
    }
    let config;
    try {
      config = parseEngineConfig();
    } catch (error) {
      hintError(elements.engineFormError, error.message);
      return;
    }
    const enabledAtClick = Boolean(elements.engineEnabled.checked);
    hintError(elements.engineFormError, "");
    state.engineSaving = true;
    invalidateEngineRequests();
    const requestEpoch = state.engineRequestEpoch;
    updateEngineControls();
    try {
      const updated = await api("/api/engine", {
        method: "PUT",
        authRedirect: false,
        body: {
          enabled: enabledAtClick,
          config,
        },
      });
      if (!state.authenticated || requestEpoch !== state.engineRequestEpoch) return false;
      if (!updated || typeof updated !== "object" || Array.isArray(updated)) {
        throw new ApiError("内核接口没有返回有效状态，请刷新页面确认保存结果。", 0);
      }
      setEngineDirty(false);
      renderEngine(updated, { replaceEditor: true });
      showToast(
        enabledAtClick
          ? updated.running
            ? "内核配置已校验并启用。"
            : "配置已保存为启用状态，但内核未运行，请查看运行状态和日志。"
          : "内核配置已校验并保存，保持停用状态。",
        enabledAtClick && !updated.running ? "error" : "success",
      );
      return true;
    } catch (error) {
      if (!state.authenticated || requestEpoch !== state.engineRequestEpoch) return false;
      if (error instanceof ApiError && error.status === 401) {
        showLogin("登录状态已失效，请重新登录。");
        return false;
      }
      hintError(elements.engineFormError, errorMessage(error));
      showToast(`内核保存失败：${errorMessage(error)}`, "error", 6500);
      return false;
    } finally {
      if (requestEpoch === state.engineRequestEpoch) {
        state.engineSaving = false;
        updateEngineControls();
      }
    }
  }

  function applyTheme(preference) {
    const valid = ["dark", "light", "system"].includes(preference) ? preference : "dark";
    state.themePreference = valid;
    const systemLight = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches;
    const resolved = valid === "system" ? (systemLight ? "light" : "dark") : valid;
    document.documentElement.dataset.theme = resolved;
    const labels = { dark: "深色", light: "浅色", system: "跟随系统" };
    const icons = { dark: "moon", light: "sun", system: "monitor" };
    setText("theme-label", `主题：${labels[valid]}`);
    const use = byId("theme-icon-use");
    if (use) use.setAttribute("href", `#i-${icons[valid]}`);
    elements.themeToggle.setAttribute("aria-label", `当前主题：${labels[valid]}，点击切换`);
    try {
      localStorage.setItem(THEME_KEY, valid);
    } catch {
      // Theme persistence is optional when browser storage is unavailable.
    }
  }

  function cycleTheme() {
    const order = ["dark", "light", "system"];
    const next = order[(order.indexOf(state.themePreference) + 1) % order.length];
    applyTheme(next);
  }

  function closeNavigation() {
    elements.sidebar.classList.remove("open");
    elements.navScrim.hidden = true;
  }

  function openNavigation() {
    elements.sidebar.classList.add("open");
    elements.navScrim.hidden = false;
    elements.navClose.focus();
  }

  function setRouteView(route) {
    const safeRoute = ROUTES[route] ? route : "overview";
    state.route = safeRoute;
    $$(".view").forEach((view) => {
      view.hidden = view.dataset.view !== safeRoute;
    });
    $$(".nav-item").forEach((item) => {
      const active = item.dataset.route === safeRoute;
      item.classList.toggle("active", active);
      if (active) item.setAttribute("aria-current", "page");
      else item.removeAttribute("aria-current");
    });
    const [title, subtitle] = ROUTES[safeRoute];
    elements.pageTitle.textContent = title;
    elements.pageSubtitle.textContent = subtitle;
    document.title = `${title} · RelayDeck`;
  }

  async function navigate(route, { updateHash = true } = {}) {
    if (!state.authenticated) return;
    const safeRoute = ROUTES[route] ? route : "overview";
    setRouteView(safeRoute);
    closeNavigation();
    if (updateHash && window.location.hash !== `#${safeRoute}`) {
      history.replaceState(null, "", `#${safeRoute}`);
    }
    if (safeRoute === "overview") await loadOverview();
    if (safeRoute === "rules") await loadRules();
    if (safeRoute === "engine") await loadEngine();
  }

  function routeFromHash() {
    const route = window.location.hash.replace(/^#/, "");
    return ROUTES[route] ? route : "overview";
  }

  function updateClock() {
    elements.clockText = elements.clockText || byId("clock");
    if (!elements.clockText) return;
    elements.clockText.textContent = new Intl.DateTimeFormat("zh-CN", {
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    }).format(new Date());
  }

  function startClock() {
    if (state.clockTimer) window.clearInterval(state.clockTimer);
    updateClock();
    state.clockTimer = window.setInterval(updateClock, 30000);
  }

  async function refreshVisiblePage() {
    if (!state.authenticated || document.visibilityState === "hidden") return;
    if (state.route === "overview") await loadStatus({ silent: true });
    if (
      state.route === "rules" &&
      !elements.ruleDialog.open &&
      !elements.passwordDialog.open &&
      !state.rulesSaving
    ) {
      await loadRules({ silent: true });
    }
    if (state.route === "engine") await loadEngine({ silent: true });
  }

  function stopPolling() {
    if (state.pollTimer) {
      window.clearInterval(state.pollTimer);
      state.pollTimer = null;
    }
  }

  function startPolling() {
    stopPolling();
    if (!state.authenticated || document.visibilityState === "hidden") return;
    state.pollTimer = window.setInterval(refreshVisiblePage, POLL_INTERVAL);
  }

  function showLogin(message = "") {
    state.authenticated = false;
    invalidateEngineRequests();
    state.engineSaving = false;
    state.engineInstallRequesting = false;
    state.engineInstallation = null;
    state.engineInstallRequestError = "";
    state.ruleProbeLatest.clear();
    state.ruleProbeResults.clear();
    state.ruleProbePending.clear();
    stopPolling();
    if (state.clockTimer) {
      window.clearInterval(state.clockTimer);
      state.clockTimer = null;
    }
    closeRuleDialog();
    closePasswordDialog();
    if (elements.confirmDialog.open) finishConfirm(false);
    elements.loginPassword.value = "";
    elements.loginPassword.setAttribute("autocomplete", "current-password");
    elements.appView.hidden = true;
    elements.loginView.hidden = false;
    hintError(elements.loginError, message);
    document.title = "登录 · RelayDeck";
    window.setTimeout(() => elements.loginUsername.focus(), 20);
  }

  async function enterApp() {
    state.authenticated = true;
    state.rulesLoaded = false;
    state.rules = [];
    state.engineLoaded = false;
    state.engine = null;
    state.engineDirty = false;
    state.engineInstallation = null;
    state.engineInstallRequesting = false;
    state.engineInstallRequestError = "";
    elements.engineDirty.hidden = true;
    state.templatesLoaded = false;
    elements.loginView.hidden = true;
    elements.appView.hidden = false;
    elements.loginPassword.value = "";
    startClock();
    startPolling();
    await navigate(routeFromHash(), { updateHash: false });
    if (!state.status) await loadStatus({ silent: true });
  }

  async function submitLogin(event) {
    event.preventDefault();
    hintError(elements.loginError, "");
    const username = elements.loginUsername.value.trim();
    const password = elements.loginPassword.value;
    if (!username || !password) {
      hintError(elements.loginError, "请输入用户名和密码。");
      return;
    }
    elements.loginSubmit.disabled = true;
    const label = $("span", elements.loginSubmit);
    if (label) label.textContent = "正在验证…";
    try {
      await api("/api/login", {
        method: "POST",
        body: { username, password },
        authRedirect: false,
      });
      elements.loginPassword.value = "";
      await enterApp();
    } catch (error) {
      elements.loginPassword.value = "";
      hintError(elements.loginError, errorMessage(error));
      elements.loginPassword.focus();
    } finally {
      elements.loginSubmit.disabled = false;
      if (label) label.textContent = "安全登录";
    }
  }

  async function logout() {
    try {
      await api("/api/logout", { method: "POST" });
      showLogin();
      showToast("已安全退出登录。");
    } catch (error) {
      if (!(error instanceof ApiError && error.status === 401)) {
        showToast(`退出登录失败：${errorMessage(error)}`, "error");
      }
    }
  }

  function bindEvents() {
    elements.loginForm.addEventListener("submit", submitLogin);
    byId("logout-button").addEventListener("click", logout);
    byId("sidebar-change-password").addEventListener("click", openPasswordDialog);
    byId("help-change-password").addEventListener("click", openPasswordDialog);
    elements.themeToggle.addEventListener("click", cycleTheme);
    elements.passwordForm.addEventListener("submit", submitPassword);
    byId("password-cancel").addEventListener("click", closePasswordDialog);
    byId("password-cancel-x").addEventListener("click", closePasswordDialog);
    elements.passwordDialog.addEventListener("cancel", (event) => {
      event.preventDefault();
      closePasswordDialog();
    });

    $$("[data-route]").forEach((link) => {
      link.addEventListener("click", (event) => {
        event.preventDefault();
        navigate(link.dataset.route);
      });
    });
    window.addEventListener("hashchange", () => {
      if (state.authenticated) navigate(routeFromHash(), { updateHash: false });
    });

    elements.navOpen.addEventListener("click", openNavigation);
    elements.navClose.addEventListener("click", closeNavigation);
    elements.navScrim.addEventListener("click", closeNavigation);
    document.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && elements.sidebar.classList.contains("open")) {
        closeNavigation();
      }
    });

    $$("[data-action='new-rule']").forEach((button) => {
      button.addEventListener("click", () => openRuleDialog());
    });
    byId("overview-refresh").addEventListener("click", async () => {
      const button = byId("overview-refresh");
      button.disabled = true;
      await loadOverview();
      button.disabled = false;
    });
    byId("rules-refresh").addEventListener("click", () => loadRules());
    elements.engineRefresh.addEventListener("click", () => loadEngine());
    elements.engineRecheck.addEventListener("click", () => loadEngine());
    elements.engineInstall.addEventListener("click", installEngine);
    elements.engineCopyInstall.addEventListener("click", copyEngineInstallCommand);

    elements.ruleSearch.addEventListener("input", renderRules);
    byId("rules-export").addEventListener("click", exportConfig);
    byId("rules-import").addEventListener("click", () => elements.importFile.click());
    elements.importFile.addEventListener("change", () => importRules(elements.importFile.files[0]));

    elements.ruleForm.addEventListener("submit", submitRule);
    elements.ruleProbe.addEventListener("click", probeDraftRule);
    ["rule-listen", "rule-target", "rule-cidrs", "rule-max-connections", "rule-udp-timeout"].forEach((id) => {
      byId(id).addEventListener("input", invalidateDraftProbeResult);
    });
    $$('input[name="rule-network"]', elements.ruleForm).forEach((input) => {
      input.addEventListener("change", invalidateDraftProbeResult);
    });
    byId("rule-enabled").addEventListener("change", invalidateDraftProbeResult);
    byId("rule-cancel").addEventListener("click", closeRuleDialog);
    byId("rule-cancel-x").addEventListener("click", closeRuleDialog);
    elements.ruleDialog.addEventListener("cancel", (event) => {
      event.preventDefault();
      closeRuleDialog();
    });

    elements.confirmCancel.addEventListener("click", () => finishConfirm(false));
    elements.confirmDialog.addEventListener("cancel", (event) => {
      event.preventDefault();
      finishConfirm(false);
    });
    elements.confirmAction.addEventListener("click", () => {
      hintError(elements.confirmError, "");
      elements.confirmAction.disabled = true;
      finishConfirm(true);
    });

    elements.engineConfig.addEventListener("input", () => {
      setEngineDirty(true);
      hintError(elements.engineFormError, "");
    });
    elements.engineEnabled.addEventListener("change", () => {
      setEngineDirty(true);
      updateEngineControls();
    });
    elements.engineTemplate.addEventListener("change", updateTemplateDescription);
    elements.engineApplyTemplate.addEventListener("click", applyTemplate);
    elements.engineSave.addEventListener("click", saveEngine);
    elements.engineStop.addEventListener("click", async () => {
      const approved = await askConfirm({
        title: "停用协议内核？",
        message: state.engineDirty
          ? "sing-box 进程将停止，已保存的 JSON 配置会保留；编辑器中未保存的 JSON 草稿也会保留。"
          : "sing-box 进程将停止，已保存的 JSON 配置会保留。",
        actionLabel: "停用内核",
        tone: "danger",
      });
      if (approved) await performStopEngine();
    });

    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "hidden") {
        stopPolling();
      } else if (state.authenticated) {
        startPolling();
        refreshVisiblePage();
      }
    });

    if (window.matchMedia) {
      const media = window.matchMedia("(prefers-color-scheme: light)");
      const listener = () => {
        if (state.themePreference === "system") applyTheme("system");
      };
      if (media.addEventListener) media.addEventListener("change", listener);
      else if (media.addListener) media.addListener(listener);
    }
  }

  async function performStopEngine() {
    if (state.engineSaving || !state.engine) return false;
    const draftWasDirty = state.engineDirty;
    const configAtClick = state.engine.config || {};
    state.engineSaving = true;
    invalidateEngineRequests();
    const requestEpoch = state.engineRequestEpoch;
    updateEngineControls();
    hintError(elements.engineFormError, "");
    try {
      const updated = await api("/api/engine", {
        method: "PUT",
        authRedirect: false,
        body: {
          enabled: false,
          config: configAtClick,
        },
      });
      if (!state.authenticated || requestEpoch !== state.engineRequestEpoch) return false;
      if (!updated || typeof updated !== "object" || Array.isArray(updated)) {
        throw new ApiError("内核接口没有返回有效状态，请刷新页面确认停用结果。", 0);
      }
      elements.engineEnabled.checked = false;
      renderEngine(updated, { replaceEditor: !draftWasDirty });
      showToast(draftWasDirty
        ? "sing-box 内核已停用；未保存的 JSON 草稿已保留。"
        : "sing-box 内核已停用。");
      return true;
    } catch (error) {
      if (!state.authenticated || requestEpoch !== state.engineRequestEpoch) return false;
      if (error instanceof ApiError && error.status === 401) {
        showLogin("登录状态已失效，请重新登录。");
        return false;
      }
      hintError(elements.engineFormError, errorMessage(error));
      showToast(`内核停用失败：${errorMessage(error)}`, "error");
      return false;
    } finally {
      if (requestEpoch === state.engineRequestEpoch) {
        state.engineSaving = false;
        updateEngineControls();
      }
    }
  }

  async function initialize() {
    let preference = "dark";
    try {
      preference = localStorage.getItem(THEME_KEY) || "dark";
    } catch {
      preference = "dark";
    }
    applyTheme(preference);
    bindEvents();
    try {
      const requestStarted = window.performance.now();
      const status = await api("/api/status", { authRedirect: false });
      updateBrowserPanelRtt(window.performance.now() - requestStarted);
      state.status = status;
      await enterApp();
    } catch (error) {
      const message = error instanceof ApiError && error.status === 401
        ? ""
        : `无法读取登录状态：${errorMessage(error)}`;
      showLogin(message);
    }
  }

  initialize();
})();
