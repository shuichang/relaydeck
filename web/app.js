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

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));
  const byId = (id) => document.getElementById(id);

  const state = {
    authenticated: false,
    route: "overview",
    status: null,
    statusLoading: false,
    rules: [],
    rulesLoaded: false,
    rulesLoading: false,
    rulesSaving: false,
    editingRuleIndex: -1,
    engine: null,
    engineLoaded: false,
    engineLoading: false,
    engineSaving: false,
    engineDirty: false,
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
      const status = await api("/api/status");
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
    const wrapper = createElement("div", "endpoint");
    wrapper.append(createElement("code", "", rule.listen || "—"));
    wrapper.append(createIcon("chevron"));
    wrapper.append(createElement("code", "", rule.target || "—"));
    return wrapper;
  }

  function makeLimits(rule) {
    const wrapper = createElement("div", "limit-list");
    wrapper.append(createElement(
      "span",
      "",
      rule.max_connections > 0
        ? `连接上限 ${formatNumber(rule.max_connections)}`
        : "连接上限默认 256",
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
      rule.max_connections > 4096
    ) {
      return "最大连接数必须在 0 到 4096 之间；0 表示采用默认值 256。";
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
    byId("rule-max-connections").value = rule ? String(rule.max_connections) : "256";
    byId("rule-udp-timeout").value = rule ? String(rule.udp_timeout) : "60";
    const network = rule ? rule.network : "both";
    const networkInput = $(`input[name="rule-network"][value="${network}"]`, elements.ruleForm);
    if (networkInput) networkInput.checked = true;
    elements.ruleDialogTitle.textContent = rule ? "编辑转发规则" : "新建转发规则";
    hintError(elements.ruleFormError, "");
    elements.ruleDialog.showModal();
    window.setTimeout(() => byId("rule-name").focus(), 20);
  }

  function closeRuleDialog() {
    if (elements.ruleDialog.open) elements.ruleDialog.close();
    state.editingRuleIndex = -1;
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

    let candidate;
    const editIndex = state.editingRuleIndex;
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
      if (saved) closeRuleDialog();
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
    elements.engineApplyTemplate.disabled = !elements.engineTemplate.value;
    const empty = state.templates.length === 0;
    elements.engineTemplate.disabled = empty || state.engineSaving;
    if (empty) {
      elements.engineTemplateDescription.textContent = "当前服务没有提供内置模板，可直接编辑 JSON 配置。";
    }
  }

  function renderEngine(engine, { replaceEditor = true } = {}) {
    state.engine = engine;
    state.engineLoaded = true;
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

    elements.engineEnabled.checked = enabled;
    elements.engineEnabled.disabled = !available || state.engineSaving;
    elements.engineSave.disabled = !available || state.engineSaving;
    elements.engineStop.disabled = !running || state.engineSaving;
    elements.engineConfig.disabled = state.engineSaving;
    elements.engineApplyTemplate.disabled = !elements.engineTemplate.value || state.engineSaving;

    if (replaceEditor && !state.engineDirty) {
      elements.engineConfig.value = prettyJSON(engine && engine.config);
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
  }

  async function loadEngine({ silent = false, replaceEditor = false } = {}) {
    if (state.engineLoading || !state.authenticated) return false;
    state.engineLoading = true;
    const firstLoad = !state.engineLoaded;
    try {
      const requests = [api("/api/engine")];
      if (!state.templatesLoaded) requests.push(api("/api/engine/templates"));
      const results = await Promise.allSettled(requests);
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
        showToast("已刷新运行状态，未保存的 JSON 配置已保留。", "success");
      }
      return true;
    } catch (error) {
      if (!silent && !(error instanceof ApiError && error.status === 401)) {
        hintError(elements.engineAlert, errorMessage(error));
        showToast(errorMessage(error), "error");
      }
      return false;
    } finally {
      state.engineLoading = false;
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
    elements.engineApplyTemplate.disabled = !template || state.engineSaving;
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
    showToast(`已载入模板“${template.name || template.id}”，保存后生效。`);
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

  async function saveEngine() {
    if (state.engineSaving || !state.engine) return;
    if (!state.engine.available) {
      hintError(elements.engineFormError, "未检测到 sing-box，当前无法校验或保存内核配置。");
      return;
    }
    let config;
    try {
      config = parseEngineConfig();
    } catch (error) {
      hintError(elements.engineFormError, error.message);
      return;
    }
    hintError(elements.engineFormError, "");
    state.engineSaving = true;
    elements.engineSave.disabled = true;
    const saveLabel = $("span", elements.engineSave);
    if (saveLabel) saveLabel.textContent = "校验并保存中…";
    try {
      await api("/api/engine", {
        method: "PUT",
        body: {
          enabled: elements.engineEnabled.checked,
          config,
        },
      });
      setEngineDirty(false);
      showToast(
        elements.engineEnabled.checked
          ? "内核配置已校验并启用。"
          : "内核配置已保存。",
      );
      await loadEngine({ silent: true, replaceEditor: true });
    } catch (error) {
      hintError(elements.engineFormError, errorMessage(error));
      showToast(`内核保存失败：${errorMessage(error)}`, "error", 6500);
    } finally {
      state.engineSaving = false;
      elements.engineSave.disabled = !state.engine || !state.engine.available;
      elements.engineEnabled.disabled = !state.engine || !state.engine.available;
      elements.engineConfig.disabled = false;
      elements.engineApplyTemplate.disabled = !elements.engineTemplate.value;
      if (state.engine) elements.engineStop.disabled = !state.engine.running;
      if (saveLabel) saveLabel.textContent = "保存配置";
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
    byId("engine-refresh").addEventListener("click", () => loadEngine());

    elements.ruleSearch.addEventListener("input", renderRules);
    byId("rules-export").addEventListener("click", exportConfig);
    byId("rules-import").addEventListener("click", () => elements.importFile.click());
    elements.importFile.addEventListener("change", () => importRules(elements.importFile.files[0]));

    elements.ruleForm.addEventListener("submit", submitRule);
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
    elements.engineEnabled.addEventListener("change", () => setEngineDirty(true));
    elements.engineTemplate.addEventListener("change", updateTemplateDescription);
    elements.engineApplyTemplate.addEventListener("click", applyTemplate);
    elements.engineSave.addEventListener("click", saveEngine);
    elements.engineStop.addEventListener("click", async () => {
      const approved = await askConfirm({
        title: "停用协议内核？",
        message: "sing-box 进程将停止，已保存的 JSON 配置会保留。",
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
    state.engineSaving = true;
    elements.engineStop.disabled = true;
    hintError(elements.engineFormError, "");
    try {
      await api("/api/engine", {
        method: "PUT",
        body: {
          enabled: false,
          config: state.engine.config || {},
        },
      });
      setEngineDirty(false);
      showToast("sing-box 内核已停用。");
      await loadEngine({ silent: true });
      return true;
    } catch (error) {
      hintError(elements.engineFormError, errorMessage(error));
      showToast(`内核停用失败：${errorMessage(error)}`, "error");
      return false;
    } finally {
      state.engineSaving = false;
      elements.engineStop.disabled = !state.engine || !state.engine.running;
      elements.engineSave.disabled = !state.engine || !state.engine.available;
      elements.engineEnabled.disabled = !state.engine || !state.engine.available;
      elements.engineConfig.disabled = false;
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
      const status = await api("/api/status", { authRedirect: false });
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
