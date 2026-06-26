let messages = {};
let currentLang = "en";
let authMessageKey = "";
const syncingSourceIDs = new Set();
const activeSyncSourceIDs = new Set();
const sourcesByID = new Map();
const credentialsByID = new Map();
const sourceArtifactPages = new Map();
const ARTIFACT_PAGE_SIZE = 50;
const SYNC_HISTORY_PAGE_SIZE = 20;
const AUTH_TOKEN_STORAGE_KEY = "docgraph.auth.token";
const WEB_PREFIX = detectWebPrefix();
let syncHistoryOffset = 0;
let syncSchedulesBySourceID = new Map();
let activeSyncTab = "history";

const sourceHelpKeys = {
  local: "source.dsn.help.local",
  static: "source.dsn.help.static",
  html: "source.dsn.help.html",
  sftp: "source.dsn.help.sftp",
  openapi: "source.dsn.help.openapi",
  git: "source.dsn.help.git",
  confluence: "source.dsn.help.confluence",
  webdocs: "source.dsn.help.webdocs",
};

const sourceScheduleKeys = {
  "": "source.schedule.manual",
  manual: "source.schedule.manual",
  hourly: "source.schedule.hourly",
  daily: "source.schedule.daily",
  weekly: "source.schedule.weekly",
  "every 6h": "source.schedule.every_6h",
  every_6h: "source.schedule.every_6h",
};

async function init() {
  await loadI18n(detectLanguage());
  bindEvents();
  syncRouteFromHash();
  updateSourceFormForKind();
  const status = await loadStatus();
  if (status === "unauthorized") {
    return;
  }
  hideLogin();
  await loadAppData();
}

function detectLanguage() {
  const saved = localStorage.getItem("docgraph.lang");
  if (saved) return saved;
  return (navigator.language || "en").toLowerCase().startsWith("zh") ? "zh-CN" : "en";
}

async function loadI18n(lang) {
  currentLang = lang === "zh-CN" ? "zh-CN" : "en";
  document.documentElement.lang = currentLang;
  document.querySelector("#language-select").value = currentLang;
  try {
    const response = await fetch(appPath(`/i18n/${currentLang}.json`));
    messages = response.ok ? await response.json() : {};
  } catch {
    messages = {};
  }
  applyI18n();
}

function applyI18n(root = document) {
  root.querySelectorAll("[data-i18n]").forEach((el) => {
    el.textContent = t(el.dataset.i18n);
  });
  root.querySelectorAll("[data-i18n-placeholder]").forEach((el) => {
    el.placeholder = t(el.dataset.i18nPlaceholder);
  });
  root.querySelectorAll("[data-i18n-title]").forEach((el) => {
    el.title = t(el.dataset.i18nTitle);
  });
  root.querySelectorAll("[data-i18n-html]").forEach((el) => {
    el.innerHTML = t(el.dataset.i18nHtml);
  });
  renderAuthMessage();
  updateSourceFormForKind();
}

function t(key, values = {}) {
  let text = messages[key] || key;
  Object.entries(values).forEach(([name, value]) => {
    text = text.replaceAll(`{${name}}`, String(value));
  });
  return text;
}

function bindEvents() {
  document.querySelector("#language-select").addEventListener("change", async (event) => {
    localStorage.setItem("docgraph.lang", event.currentTarget.value);
    await loadI18n(event.currentTarget.value);
    const status = await loadStatus();
    if (status !== "unauthorized") {
      await loadAppData();
    }
  });
  document.querySelector("#auth-form")?.addEventListener("submit", onAuthSubmit);
  document.querySelector("#logout-button")?.addEventListener("click", onLogoutClick);
  document.querySelectorAll("[data-route]").forEach((button) => {
    button.addEventListener("click", () => navigate(button.dataset.route));
  });
  window.addEventListener("hashchange", syncRouteFromHash);

  document.querySelector("#source-add-button")?.addEventListener("click", () => openSourceDialog());
  document.querySelector("[data-close-source-dialog]")?.addEventListener("click", closeSourceDialog);
  document.querySelector("#credential-add-button")?.addEventListener("click", () => openCredentialDialog());
  document.querySelector("[data-close-credential-dialog]")?.addEventListener("click", closeCredentialDialog);
  document.querySelector("#sync-task-tabs")?.addEventListener("click", onSyncTaskTabClick);
  document.querySelector("#sync-history-refresh")?.addEventListener("click", () => loadSyncJobHistory());
  document.querySelector("#sync-schedule-add-button")?.addEventListener("click", () => openSyncScheduleDialog());
  document.querySelector("[data-close-sync-schedule-dialog]")?.addEventListener("click", closeSyncScheduleDialog);
  document.querySelector('#source-form select[name="kind"]').addEventListener("change", updateSourceFormForKind);

  document.querySelector("#source-form").addEventListener("submit", onSourceSubmit);
  document.querySelector("#credential-form")?.addEventListener("submit", onCredentialSubmit);
  document.querySelector("#sync-task-filters")?.addEventListener("submit", onSyncTaskFiltersSubmit);
  document.querySelector("#sync-job-history")?.addEventListener("click", onSyncJobHistoryClick);
  document.querySelector("#sync-job-pagination")?.addEventListener("click", onSyncJobPaginationClick);
  document.querySelector("#sync-schedules")?.addEventListener("click", onSyncSchedulesClick);
  document.querySelector("#sync-schedule-form")?.addEventListener("submit", onSyncScheduleSubmit);
  document.querySelector('#sync-schedule-form select[name="source_id"]')?.addEventListener("change", updateSyncScheduleCredentialField);
  document.querySelector("#sources").addEventListener("click", onSourcesClick);
  document.querySelector("#credentials")?.addEventListener("click", onCredentialsClick);
  document.querySelector("#search-form").addEventListener("submit", onSearchSubmit);
  document.querySelector("#results").addEventListener("click", onSearchFeedbackClick);
  document.querySelector("#relation-proposals-refresh")?.addEventListener("click", () => loadKnowledgeRelationProposals());
  document.querySelector("#relation-proposal-filters")?.addEventListener("submit", onKnowledgeRelationProposalFiltersSubmit);
  document.querySelector("#relation-proposals")?.addEventListener("click", onKnowledgeRelationProposalClick);
  document.querySelector("#node-search-form").addEventListener("submit", onNodeSearchSubmit);
  document.querySelector("#node-search-results").addEventListener("click", onNodeSearchResultsClick);
  document.querySelector("#node-form").addEventListener("submit", onNodeSubmit);
  document.querySelector("#related").addEventListener("click", onRelatedFeedbackClick);
  document.querySelector("#manual-relation-form").addEventListener("submit", onManualRelationSubmit);
  document.querySelector("#merge-node-form").addEventListener("submit", onMergeNodeSubmit);
  document.querySelector("#impact-button").addEventListener("click", onImpactClick);
}

function syncRouteFromHash() {
  const route = String(location.hash || "#dashboard").replace(/^#/, "");
  navigate(route || "dashboard", false);
}

function navigate(route, updateHash = true) {
  const knownRoutes = new Set(["dashboard", "connectors", "sync-tasks", "credentials", "search", "governance", "nodes"]);
  const nextRoute = knownRoutes.has(route) ? route : "dashboard";
  document.querySelectorAll("[data-view]").forEach((view) => {
    view.classList.toggle("active", view.dataset.view === nextRoute);
  });
  document.querySelectorAll("[data-route]").forEach((button) => {
    button.classList.toggle("active", button.dataset.route === nextRoute);
  });
  if (updateHash && location.hash !== `#${nextRoute}`) {
    history.replaceState(null, "", `#${nextRoute}`);
  }
  if (nextRoute === "sync-tasks" && sourcesByID.size > 0) {
    loadSyncTasksView().catch((error) => {
      if (!isAuthError(error)) console.error(error);
    });
  }
  if (nextRoute === "governance") {
    loadKnowledgeRelationProposals().catch((error) => {
      if (!isAuthError(error)) console.error(error);
    });
  }
}

function updateSourceFormForKind() {
  const kind = document.querySelector('#source-form select[name="kind"]')?.value || "local";
  document.querySelectorAll("#source-form [data-kinds]").forEach((field) => {
    const kinds = String(field.dataset.kinds || "").split(/\s+/);
    field.hidden = !(kinds.includes("all") || kinds.includes(kind));
  });
  const help = document.querySelector("[data-source-dsn-help]");
  if (help) {
    help.textContent = t(sourceHelpKeys[kind] || "source.dsn.help.local");
  }
  populateCredentialSelect();
}

function openSourceDialog(source = null) {
  const dialog = document.querySelector("#source-dialog");
  const form = document.querySelector("#source-form");
  if (!dialog || !form) return;
  resetSourceForm(form);
  if (source) {
    fillSourceForm(form, source);
  }
  updateSourceDialogMode(Boolean(source));
  if (dialog?.showModal) {
    dialog.showModal();
    updateSourceFormForKind();
  }
}

function closeSourceDialog() {
  const dialog = document.querySelector("#source-dialog");
  if (dialog?.open) {
    dialog.close();
  }
}

function openCredentialDialog(credential = null) {
  const dialog = document.querySelector("#credential-dialog");
  const form = document.querySelector("#credential-form");
  if (!dialog || !form) return;
  resetCredentialForm(form);
  if (credential) {
    fillCredentialForm(form, credential);
  }
  updateCredentialDialogMode(Boolean(credential));
  if (dialog?.showModal) {
    dialog.showModal();
  }
}

function closeCredentialDialog() {
  const dialog = document.querySelector("#credential-dialog");
  if (dialog?.open) {
    dialog.close();
  }
}

function resetSourceForm(form) {
  form.reset();
  form.elements.source_id.value = "";
}

function resetCredentialForm(form) {
  form.reset();
  form.elements.credential_id.value = "";
}

function updateSourceDialogMode(editing) {
  const title = document.querySelector("[data-source-dialog-title]");
  const submit = document.querySelector("[data-source-submit-label]");
  const key = editing ? "source.edit" : "source.add";
  if (title) {
    title.dataset.i18n = key;
    title.textContent = t(key);
  }
  if (submit) {
    submit.dataset.i18n = editing ? "source.save" : "source.add";
    submit.textContent = t(editing ? "source.save" : "source.add");
  }
}

function updateCredentialDialogMode(editing) {
  const title = document.querySelector("[data-credential-dialog-title]");
  const submit = document.querySelector("[data-credential-submit-label]");
  const key = editing ? "credentials.edit" : "credentials.add";
  if (title) {
    title.dataset.i18n = key;
    title.textContent = t(key);
  }
  if (submit) {
    submit.dataset.i18n = editing ? "credentials.save" : "credentials.add";
    submit.textContent = t(editing ? "credentials.save" : "credentials.add");
  }
}

async function loadStatus() {
  const health = document.querySelector("#health");
  try {
    const response = await apiFetch("/api/status", { skipAuthRedirect: true });
    if (response.status === 401) {
      const hadToken = Boolean(getAuthToken());
      clearAuthToken();
      health.textContent = t("auth.required_status");
      health.className = "status warn";
      showLogin(hadToken ? "auth.session_expired" : "auth.required");
      return "unauthorized";
    }
    if (!response.ok) {
      throw new Error(`status ${response.status}`);
    }
    const status = await response.json();
    health.textContent = t("health.ok");
    health.className = "status ok";

    const values = [
      status.sources,
      status.documents,
      status.sections,
      status.nodes,
      status.edges,
      status.jobs,
    ];
    document.querySelectorAll("#stats dd").forEach((dd, index) => {
      dd.textContent = values[index] ?? 0;
    });
    return "ok";
  } catch (error) {
    health.textContent = t("health.error");
    health.className = "status warn";
    return "error";
  }
}

async function loadAppData() {
  try {
    await loadCredentials();
    await loadSources();
    if (currentRoute() === "sync-tasks") {
      await loadSyncTasksView();
    }
    if (currentRoute() === "governance") {
      await loadKnowledgeRelationProposals();
    }
  } catch (error) {
    if (!isAuthError(error)) {
      console.error(error);
    }
  }
}

function currentRoute() {
  return String(location.hash || "#dashboard").replace(/^#/, "") || "dashboard";
}

async function apiFetch(path, options = {}) {
  const { skipAuthRedirect = false, ...fetchOptions } = options;
  const headers = new Headers(fetchOptions.headers || {});
  const token = getAuthToken();
  if (isAPIPath(path) && token) {
    headers.set("X-DocGraph-Token", token);
  }
  const response = await fetch(appPath(path), {
    ...fetchOptions,
    headers,
  });
  if (response.status === 401 && isAPIPath(path) && !skipAuthRedirect) {
    clearAuthToken();
    showLogin("auth.session_expired");
  }
  return response;
}

async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (!headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const response = await apiFetch(path, {
    ...options,
    headers,
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    if (response.status === 401) {
      const error = new Error(t("auth.required"));
      error.auth = true;
      throw error;
    }
    if (response.status === 409) {
      throw new Error(t("source.sync_in_progress"));
    }
    throw new Error(body.error?.message || t("error.request_failed", { status: response.status }));
  }
  return body;
}

async function onAuthSubmit(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const token = String(new FormData(form).get("token") || "").trim();
  if (!token) {
    showLogin("auth.required");
    return;
  }
  setAuthToken(token);
  setAuthMessage("auth.checking");
  const status = await loadStatus();
  if (status === "unauthorized") {
    clearAuthToken();
    showLogin("auth.invalid");
    return;
  }
  hideLogin();
  await loadAppData();
}

async function onLogoutClick() {
  clearAuthToken();
  const status = await loadStatus();
  if (status === "unauthorized") {
    showLogin("auth.logged_out");
    return;
  }
  hideLogin();
  await loadAppData();
}

function isAPIPath(path) {
  return String(path || "").startsWith("/api/");
}

function appPath(path) {
  const value = String(path || "");
  if (!value.startsWith("/")) {
    return value;
  }
  return `${WEB_PREFIX}${value}`;
}

function detectWebPrefix() {
  const script = document.currentScript;
  const src = script?.getAttribute("src") || "";
  try {
    const url = new URL(src, document.baseURI);
    const path = url.pathname;
    if (!path.endsWith("/main.js")) {
      return "";
    }
    const prefix = path.slice(0, -"/main.js".length);
    return prefix === "/" ? "" : prefix;
  } catch {
    return "";
  }
}

function getAuthToken() {
  return sessionStorage.getItem(AUTH_TOKEN_STORAGE_KEY) || "";
}

function setAuthToken(token) {
  sessionStorage.setItem(AUTH_TOKEN_STORAGE_KEY, token);
  renderLogoutButton();
}

function clearAuthToken() {
  sessionStorage.removeItem(AUTH_TOKEN_STORAGE_KEY);
  renderLogoutButton();
}

function showLogin(messageKey = "") {
  const gate = document.querySelector("#auth-gate");
  const shell = document.querySelector(".app-shell");
  if (gate) {
    gate.hidden = false;
  }
  shell?.classList.add("auth-locked");
  setAuthMessage(messageKey);
  renderLogoutButton();
  window.requestAnimationFrame(() => {
    document.querySelector("#auth-token")?.focus();
  });
}

function hideLogin() {
  const gate = document.querySelector("#auth-gate");
  const shell = document.querySelector(".app-shell");
  if (gate) {
    gate.hidden = true;
  }
  shell?.classList.remove("auth-locked");
  setAuthMessage("");
  renderLogoutButton();
}

function setAuthMessage(messageKey) {
  authMessageKey = messageKey;
  renderAuthMessage();
}

function renderAuthMessage() {
  const message = document.querySelector("#auth-message");
  if (!message) return;
  message.textContent = authMessageKey ? t(authMessageKey) : "";
}

function renderLogoutButton() {
  const button = document.querySelector("#logout-button");
  if (!button) return;
  button.hidden = !getAuthToken();
}

function isAuthError(error) {
  return Boolean(error?.auth);
}

async function loadCredentials() {
  const container = document.querySelector("#credentials");
  const body = await request("/api/confluence-cookie-credentials");
  credentialsByID.clear();
  const credentials = body.credentials || [];
  credentials.forEach((credential) => credentialsByID.set(credential.id, credential));
  populateCredentialSelect();
  if (!container) return;
  if (!credentials.length) {
    container.innerHTML = `<p class="muted">${escapeHTML(t("credentials.empty"))}</p>`;
    return;
  }
  container.innerHTML = credentials.map(renderCredential).join("");
}

function renderCredential(credential) {
  const statusKey = `credentials.status.${credential.status || "unknown"}`;
  return `
    <div class="item">
      <strong>${escapeHTML(credential.name || "")}</strong>
      <small>${escapeHTML(credential.base_url || "")}</small>
      <div class="source-meta">
        <span>${escapeHTML(t("credentials.status"))}: ${escapeHTML(t(statusKey))}</span>
        <span>${escapeHTML(t("credentials.source_count", { count: credential.source_count || 0 }))}</span>
        ${credential.cookie_preview ? `<span>${escapeHTML(credential.cookie_preview)}</span>` : ""}
        ${credential.updated_at ? `<span>${escapeHTML(formatJobTime(credential.updated_at))}</span>` : ""}
      </div>
      ${credential.last_error ? `<small class="warn-text">${escapeHTML(credential.last_error)}</small>` : ""}
      <div class="source-actions">
        <button type="button" data-edit-credential="${escapeAttr(credential.id)}">${escapeHTML(t("credentials.edit"))}</button>
        <button type="button" data-attach-credential="${escapeAttr(credential.id)}">${escapeHTML(t("credentials.attach"))}</button>
        <button type="button" data-resume-credential="${escapeAttr(credential.id)}">${escapeHTML(t("credentials.resume"))}</button>
        <button type="button" data-delete-credential="${escapeAttr(credential.id)}">${escapeHTML(t("source.delete"))}</button>
      </div>
    </div>
  `;
}

function populateCredentialSelect() {
  const selects = document.querySelectorAll('#source-form select[name="cookie_credential_id"], #sync-schedule-form select[name="cookie_credential_id"]');
  selects.forEach((select) => {
    const current = select.value;
    const options = [`<option value="">${escapeHTML(t("source.cookie_credential.none"))}</option>`];
    credentialsByID.forEach((credential) => {
      options.push(`<option value="${escapeAttr(credential.id)}">${escapeHTML(credential.name || credential.base_url || credential.id)}</option>`);
    });
    select.innerHTML = options.join("");
    if (current && credentialsByID.has(current)) {
      select.value = current;
    }
  });
}

function populateSyncSourceSelects() {
  const filter = document.querySelector("#sync-source-filter");
  if (filter) {
    const current = filter.value;
    const options = [`<option value="">${escapeHTML(t("sync.source.all"))}</option>`];
    sourcesByID.forEach((source) => {
      options.push(`<option value="${escapeAttr(source.id)}">${escapeHTML(source.name || source.id)}</option>`);
    });
    filter.innerHTML = options.join("");
    if (current && sourcesByID.has(current)) {
      filter.value = current;
    }
  }
  const scheduleSelect = document.querySelector('#sync-schedule-form select[name="source_id"]');
  if (scheduleSelect) {
    const current = scheduleSelect.value;
    const options = [];
    sourcesByID.forEach((source) => {
      options.push(`<option value="${escapeAttr(source.id)}">${escapeHTML(source.name || source.id)} (${escapeHTML(source.kind || "")})</option>`);
    });
    scheduleSelect.innerHTML = options.join("");
    if (current && sourcesByID.has(current)) {
      scheduleSelect.value = current;
    }
  }
}

async function loadSources() {
  const container = document.querySelector("#sources");
  const body = await request("/api/sources");
  container.innerHTML = "";
  sourcesByID.clear();
  activeSyncSourceIDs.clear();
  if (!body.sources?.length) {
    populateSyncSourceSelects();
    container.innerHTML = `<p class="muted">${escapeHTML(t("sources.empty"))}</p>`;
    return;
  }
  await loadActiveSyncSourceIDs();
  const sources = [...body.sources].sort((a, b) => String(b.created_at || "").localeCompare(String(a.created_at || "")) || String(b.id || "").localeCompare(String(a.id || "")));
  sources.forEach((source) => {
    sourcesByID.set(source.id, source);
    const syncing = syncingSourceIDs.has(source.id) || activeSyncSourceIDs.has(source.id);
    const paused = source.sync_status === "paused";
    const item = document.createElement("div");
    item.className = "item";
    item.innerHTML = `
      <strong>${escapeHTML(source.name)}</strong>
      <small>${escapeHTML(source.kind)} · ${escapeHTML(source.id)}</small>
      ${source.created_at ? `<small>${escapeHTML(formatJobTime(source.created_at))}</small>` : ""}
      <small>${escapeHTML(source.dsn)}</small>
      <div class="source-meta">
        <span>${escapeHTML(t("source.schedule"))}: ${escapeHTML(formatSourceSchedule(source.sync_schedule))}</span>
        <span class="${escapeAttr(paused ? "source-state warn" : "source-state")}">${escapeHTML(formatSourceSyncStatus(source))}</span>
      </div>
      <div class="source-actions">
        <button data-edit="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.edit"))}</button>
        <button data-sync="${escapeAttr(source.id)}" type="button" ${syncing ? "disabled" : ""}>${escapeHTML(t(syncing ? "source.syncing" : "source.sync"))}</button>
        <button data-sync-tasks-source="${escapeAttr(source.id)}" type="button">${escapeHTML(t("sync.view_tasks"))}</button>
        <button data-artifacts="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.artifacts"))}</button>
        <button data-delete="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.delete"))}</button>
      </div>
      <div class="source-artifacts" data-artifacts-list="${escapeAttr(source.id)}"></div>
    `;
    container.appendChild(item);
  });
  populateSyncSourceSelects();
}

async function loadActiveSyncSourceIDs() {
  const statuses = ["queued", "running"];
  await Promise.all(statuses.map(async (status) => {
    const body = await request(`/api/jobs?kind=sync_source&status=${encodeURIComponent(status)}&limit=100`);
    (body.jobs || []).forEach((job) => {
      if (job.source_id) {
        activeSyncSourceIDs.add(job.source_id);
      }
    });
  }));
}

async function onSourceSubmit(event) {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const sourceID = String(form.get("source_id") || "").trim();
  const payload = buildSourcePayload(form);
  try {
    await request(sourceID ? `/api/sources/${encodeURIComponent(sourceID)}` : "/api/sources", {
      method: sourceID ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    formElement.reset();
    updateSourceDialogMode(false);
    updateSourceFormForKind();
    closeSourceDialog();
    await loadSources();
    await loadStatus();
  } catch (error) {
    alert(error.message);
  }
}

async function onCredentialSubmit(event) {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const credentialID = String(form.get("credential_id") || "").trim();
  const cookie = String(form.get("cookie") || "").trim();
  const payload = {
    name: String(form.get("name") || "").trim(),
    base_url: String(form.get("base_url") || "").trim(),
    notes: String(form.get("notes") || "").trim(),
  };
  if (cookie) {
    payload.cookie = cookie;
  }
  try {
    await request(credentialID ? `/api/confluence-cookie-credentials/${encodeURIComponent(credentialID)}` : "/api/confluence-cookie-credentials", {
      method: credentialID ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    formElement.reset();
    updateCredentialDialogMode(false);
    closeCredentialDialog();
    await loadCredentials();
    await loadSources();
  } catch (error) {
    alert(error.message);
  }
}

function buildSourcePayload(form) {
  const kind = String(form.get("kind") || "local").trim();
  return {
    kind,
    name: String(form.get("name") || "").trim(),
    dsn: String(form.get("dsn") || "").trim(),
    product_hint: String(form.get("product_hint") || "").trim(),
    module_hint: String(form.get("module_hint") || "").trim(),
    sync_schedule: String(form.get("sync_schedule") || "").trim(),
    config_json: JSON.stringify(buildSourceConfig(form, kind)),
  };
}

function buildSourceConfig(form, kind) {
  const config = {};
  const addString = (key, formKey = key) => {
    const value = String(form.get(formKey) || "").trim();
    if (value) config[key] = value;
  };
  if (kind === "git") {
    addString("branch");
    addString("path", "source_path");
    addString("cache");
  }
  if (kind === "static" || kind === "sftp") {
    addString("include");
    addString("exclude");
  }
  if (kind === "static" || kind === "sftp" || kind === "html") {
    addString("url_prefix");
  }
  if (kind === "sftp") {
    addString("identity_file");
    addString("password");
    addString("passphrase");
    addString("known_hosts");
    if (form.get("strict_host_key") === "on") {
      config.strict_host_key = true;
    }
  }
  if (kind === "confluence") {
    addString("page_id");
    addString("space_key");
    addString("token");
    addString("cookie_credential_id");
    addString("cookie");
    if (form.get("include_children") === "on") {
      config.include_children = true;
    }
  }
  if (kind === "webdocs") {
    addString("max_pages");
    addString("max_depth");
    addString("bearer_token");
    addString("cookie");
    addString("headers_json");
    if (form.get("is_spa") === "on") {
      config.is_spa = true;
    }
  }
  return config;
}

function fillSourceForm(form, source) {
  const config = parsePayload(source.config_json);
  form.elements.source_id.value = source.id || "";
  form.elements.kind.value = source.kind || "local";
  form.elements.name.value = source.name || "";
  form.elements.dsn.value = source.dsn || "";
  form.elements.product_hint.value = source.product_hint || "";
  form.elements.module_hint.value = source.module_hint || "";
  setFormValue(form, "sync_schedule", normalizeSourceScheduleValue(source.sync_schedule));
  setFormValue(form, "branch", config.branch);
  setFormValue(form, "source_path", config.path);
  setFormValue(form, "cache", config.cache);
  setFormValue(form, "include", config.include);
  setFormValue(form, "exclude", config.exclude);
  setFormValue(form, "url_prefix", config.url_prefix);
  setFormValue(form, "identity_file", config.identity_file);
  setFormValue(form, "password", config.password);
  setFormValue(form, "passphrase", config.passphrase);
  setFormValue(form, "known_hosts", config.known_hosts);
  setFormValue(form, "page_id", config.page_id);
  setFormValue(form, "space_key", config.space_key);
  setFormValue(form, "token", config.token);
  setFormValue(form, "cookie_credential_id", config.cookie_credential_id);
  setFormValue(form, "max_pages", config.max_pages);
  setFormValue(form, "max_depth", config.max_depth);
  setFormValue(form, "bearer_token", config.bearer_token);
  setFormValue(form, "cookie", config.cookie);
  setFormValue(form, "headers_json", config.headers_json);
  if (form.elements.include_children) {
    form.elements.include_children.checked = config.include_children === true;
  }
  if (form.elements.strict_host_key) {
    form.elements.strict_host_key.checked = config.strict_host_key === true;
  }
  if (form.elements.is_spa) {
    form.elements.is_spa.checked = config.is_spa === true;
  }
}

function fillCredentialForm(form, credential) {
  form.elements.credential_id.value = credential.id || "";
  form.elements.name.value = credential.name || "";
  form.elements.base_url.value = credential.base_url || "";
  form.elements.cookie.value = "";
  form.elements.notes.value = credential.notes || "";
}

async function onCredentialsClick(event) {
  const button = event.target.closest("button[data-edit-credential], button[data-delete-credential], button[data-attach-credential], button[data-resume-credential]");
  if (!button) return;

  if (button.dataset.editCredential) {
    const credential = credentialsByID.get(button.dataset.editCredential);
    if (credential) {
      openCredentialDialog(credential);
    }
    return;
  }

  button.disabled = true;
  try {
    if (button.dataset.deleteCredential) {
      await request(`/api/confluence-cookie-credentials/${encodeURIComponent(button.dataset.deleteCredential)}`, { method: "DELETE" });
    } else if (button.dataset.attachCredential) {
      await request(`/api/confluence-cookie-credentials/${encodeURIComponent(button.dataset.attachCredential)}/attach-sources`, { method: "POST", body: "{}" });
    } else if (button.dataset.resumeCredential) {
      await request(`/api/confluence-cookie-credentials/${encodeURIComponent(button.dataset.resumeCredential)}/resume-sources`, { method: "POST", body: "{}" });
    }
    await loadCredentials();
    await loadSources();
  } catch (error) {
    alert(error.message);
  } finally {
    button.disabled = false;
  }
}

function setFormValue(form, name, value) {
  if (!form.elements[name]) return;
  form.elements[name].value = value == null ? "" : String(value);
}

function normalizeSourceScheduleValue(value) {
  const schedule = String(value || "").trim();
  if (schedule === "every_6h") {
    return "every 6h";
  }
  return schedule === "manual" ? "" : schedule;
}

function formatSourceSchedule(value) {
  const schedule = String(value || "").trim();
  const key = sourceScheduleKeys[schedule] || "";
  if (key) {
    return t(key);
  }
  return schedule || t("source.schedule.manual");
}

function formatSourceSyncStatus(source) {
  const status = String(source.sync_status || "active").trim();
  if (status !== "paused") {
    return `${t("source.status")}: ${t("source.status.active")}`;
  }
  const reason = String(source.sync_status_reason || "credential_required").trim();
  const reasonLabel = reason === "credential_required" ? t("source.status.credential_required") : humanizeToken(reason);
  const pausedAt = String(source.sync_paused_at || "").trim();
  if (pausedAt) {
    return `${t("source.status")}: ${reasonLabel} · ${t("source.status.paused_at", { time: formatJobTime(pausedAt) })}`;
  }
  return `${t("source.status")}: ${reasonLabel}`;
}

async function onSourcesClick(event) {
  const button = event.target.closest("button[data-edit], button[data-sync], button[data-sync-tasks-source], button[data-artifacts], button[data-artifact-page], button[data-delete], button[data-open-node], button[data-save-desc]");
  if (!button) return;

  if (button.dataset.edit) {
    const source = sourcesByID.get(button.dataset.edit);
    if (source) {
      openSourceDialog(source);
    }
    return;
  }

  if (button.dataset.openNode) {
    openNodeByID(button.dataset.openNode);
    return;
  }

  if (button.dataset.saveDesc) {
    const documentID = button.dataset.saveDesc;
    const row = button.closest(".artifact-row");
    const input = row?.querySelector("[data-document-desc]");
    button.disabled = true;
    try {
      await request(`/api/documents/${encodeURIComponent(documentID)}/profile`, {
        method: "PUT",
        body: JSON.stringify({ desc: input?.value || "" }),
      });
      button.textContent = t("document.desc.saved");
    } catch (error) {
      alert(error.message);
    } finally {
      window.setTimeout(() => {
        button.disabled = false;
        button.textContent = t("document.desc.save");
      }, 900);
    }
    return;
  }

  if (button.dataset.artifactPage) {
    button.disabled = true;
    const sourceID = button.dataset.artifactPage;
    const page = parseInt(button.dataset.page, 10) || 0;
    sourceArtifactPages.set(sourceID, page);
    const limit = ARTIFACT_PAGE_SIZE;
    try {
      const body = await request(`/api/sources/${encodeURIComponent(sourceID)}/artifacts?limit=${limit}&offset=${page * limit}`);
      await hydrateDocumentProfiles(body.documents || []);
      const container = document.querySelector(`[data-artifacts-list="${CSS.escape(sourceID)}"]`);
      container.innerHTML = renderSourceArtifacts(body, page, limit);
    } catch (error) {
      alert(error.message);
    } finally {
      button.disabled = false;
    }
    return;
  }

  if (button.dataset.delete) {
    button.disabled = true;
    try {
      await request(`/api/sources/${button.dataset.delete}`, { method: "DELETE" });
      await loadSources();
      await loadStatus();
    } catch (error) {
      alert(error.message);
    } finally {
      button.disabled = false;
    }
    return;
  }

  if (button.dataset.syncTasksSource) {
    const filter = document.querySelector("#sync-source-filter");
    if (filter) {
      filter.value = button.dataset.syncTasksSource;
    }
    switchSyncTaskTab("history");
    syncHistoryOffset = 0;
    navigate("sync-tasks");
    await loadSyncTasksView();
    return;
  }

  if (button.dataset.artifacts) {
    button.disabled = true;
    try {
      sourceArtifactPages.set(button.dataset.artifacts, 0);
      const limit = ARTIFACT_PAGE_SIZE;
      const body = await request(`/api/sources/${button.dataset.artifacts}/artifacts?limit=${limit}&offset=0`);
      await hydrateDocumentProfiles(body.documents || []);
      const container = document.querySelector(`[data-artifacts-list="${CSS.escape(button.dataset.artifacts)}"]`);
      container.innerHTML = renderSourceArtifacts(body, 0, limit);
    } catch (error) {
      alert(error.message);
    } finally {
      button.disabled = false;
    }
    return;
  }

  const sourceID = button.dataset.sync;
  if (!sourceID || syncingSourceIDs.has(sourceID) || activeSyncSourceIDs.has(sourceID)) {
    alert(t("source.sync_in_progress"));
    return;
  }
  syncingSourceIDs.add(sourceID);
  button.disabled = true;
  button.textContent = t("source.syncing");
  try {
    await request("/api/sync-tasks", {
      method: "POST",
      body: JSON.stringify({ source_id: sourceID }),
    });
    activeSyncSourceIDs.add(sourceID);
    button.textContent = t("source.syncing");
    syncingSourceIDs.delete(sourceID);
    await loadSources();
    await loadSyncTasksView();
    await loadStatus();
  } catch (error) {
    syncingSourceIDs.delete(sourceID);
    button.disabled = false;
    button.textContent = t("source.sync");
    alert(error.message);
  }
}

function renderBrokenLinks(links) {
  return `
    <details class="job-broken-links">
      <summary>${escapeHTML(t("jobs.broken_links"))} (${links.length})</summary>
      ${links.slice(0, 20).map((link) => `
        <small>${escapeHTML(link.source_document || "")}${link.source_section ? ` / ${escapeHTML(link.source_section)}` : ""} · ${escapeHTML(link.href || "")}${link.resolved_target ? ` -> ${escapeHTML(link.resolved_target)}` : ""}</small>
      `).join("")}
    </details>
  `;
}

async function loadSyncTasksView() {
  populateSyncSourceSelects();
  if (activeSyncTab === "schedules") {
    await loadSyncSchedules();
    return;
  }
  await loadSyncJobHistory();
}

async function onSyncTaskTabClick(event) {
  const button = event.target.closest("button[data-sync-tab]");
  if (!button) return;
  switchSyncTaskTab(button.dataset.syncTab || "history");
  try {
    await loadSyncTasksView();
  } catch (error) {
    alert(error.message);
  }
}

function switchSyncTaskTab(tab) {
  activeSyncTab = tab === "schedules" ? "schedules" : "history";
  document.querySelectorAll("[data-sync-tab]").forEach((button) => {
    const active = button.dataset.syncTab === activeSyncTab;
    button.classList.toggle("active", active);
    button.setAttribute("aria-selected", active ? "true" : "false");
  });
  document.querySelectorAll("[data-sync-tab-panel]").forEach((panel) => {
    panel.classList.toggle("active", panel.dataset.syncTabPanel === activeSyncTab);
  });
}

async function onSyncTaskFiltersSubmit(event) {
  event.preventDefault();
  syncHistoryOffset = 0;
  await loadSyncJobHistory();
}

async function onSyncJobPaginationClick(event) {
  const button = event.target.closest("button[data-sync-page-offset]");
  if (!button) return;
  syncHistoryOffset = Math.max(0, parseInt(button.dataset.syncPageOffset, 10) || 0);
  button.disabled = true;
  try {
    await loadSyncJobHistory();
  } catch (error) {
    alert(error.message);
  } finally {
    button.disabled = false;
  }
}

async function onSyncJobHistoryClick(event) {
  const button = event.target.closest("button[data-delete-sync-job], button[data-sync-history-source]");
  if (!button) return;
  if (button.dataset.syncHistorySource) {
    const filter = document.querySelector("#sync-source-filter");
    if (filter) {
      filter.value = button.dataset.syncHistorySource;
    }
    syncHistoryOffset = 0;
    await loadSyncJobHistory();
    return;
  }
  const sourceID = button.dataset.sourceId || "";
  const jobID = button.dataset.deleteSyncJob || "";
  if (!sourceID || !jobID) return;
  button.disabled = true;
  try {
    await request(`/api/sources/${encodeURIComponent(sourceID)}/jobs/${encodeURIComponent(jobID)}`, { method: "DELETE" });
    await loadSyncJobHistory();
    await loadStatus();
  } catch (error) {
    alert(error.message);
  } finally {
    button.disabled = false;
  }
}

async function loadSyncJobHistory() {
  const container = document.querySelector("#sync-job-history");
  const pagination = document.querySelector("#sync-job-pagination");
  if (!container || !pagination) return;
  const form = document.querySelector("#sync-task-filters");
  const data = form ? new FormData(form) : new FormData();
  const limit = parseInt(data.get("limit"), 10) || SYNC_HISTORY_PAGE_SIZE;
  const params = new URLSearchParams();
  params.set("kind", "sync_source");
  params.set("limit", String(limit));
  params.set("offset", String(syncHistoryOffset));
  const sourceID = String(data.get("source_id") || "").trim();
  const status = String(data.get("status") || "").trim();
  if (sourceID) params.set("source_id", sourceID);
  if (status) params.set("status", status);
  const body = await request(`/api/jobs?${params.toString()}`);
  container.innerHTML = renderSyncJobHistory(body.jobs || []);
  pagination.innerHTML = renderSyncPagination(body);
}

function renderSyncJobHistory(jobs) {
  if (!jobs.length) {
    return `<p class="muted">${escapeHTML(t("jobs.empty"))}</p>`;
  }
  return jobs.map(renderSyncJobRow).join("");
}

function renderSyncJobRow(job) {
  const source = sourcesByID.get(job.source_id);
  const sourceName = source?.name || job.source_id || "";
  const payload = parsePayload(job.payload_json);
  const result = parsePayload(job.result_json);
  const summary = { ...payload, ...result };
  const brokenLinks = Array.isArray(summary.broken_links) ? summary.broken_links : [];
  const error = String(job.last_error || "").trim();
  return `
    <div class="job-row">
      <div class="job-main">
        <strong>${escapeHTML(formatJobStatus(job.status))}</strong>
        <small>${escapeHTML(formatJobTime(job.updated_at || job.created_at))}</small>
      </div>
      <small>${escapeHTML(sourceName)} · ${escapeHTML(job.id || "")}</small>
      <div class="job-meta">
        <span>${escapeHTML(t("jobs.documents"))}: ${escapeHTML(summary.documents ?? 0)}</span>
        <span>${escapeHTML(t("jobs.broken_links"))}: ${escapeHTML(brokenLinks.length)}</span>
        <span>${escapeHTML(t("sync.attempts"))}: ${escapeHTML(job.attempts ?? 0)}</span>
      </div>
      ${error ? `<p class="job-error">${escapeHTML(error)}</p>` : ""}
      ${brokenLinks.length ? renderBrokenLinks(brokenLinks) : ""}
      <div class="item-actions">
        <button type="button" data-sync-history-source="${escapeAttr(job.source_id || "")}">${escapeHTML(t("sync.filter_source"))}</button>
        <button type="button" data-source-id="${escapeAttr(job.source_id || "")}" data-delete-sync-job="${escapeAttr(job.id || "")}">${escapeHTML(t("jobs.delete"))}</button>
      </div>
    </div>
  `;
}

function renderSyncPagination(body) {
  const limit = body.limit || SYNC_HISTORY_PAGE_SIZE;
  const offset = body.offset || 0;
  const total = body.total || 0;
  const count = Array.isArray(body.jobs) ? body.jobs.length : 0;
  if (total <= limit && offset === 0) return "";
  const start = total === 0 ? 0 : offset + 1;
  const end = Math.min(offset + count, total);
  const prevOffset = Math.max(0, offset - limit);
  const nextOffset = offset + limit;
  return `
    <button type="button" data-sync-page-offset="${prevOffset}" ${offset > 0 ? "" : "disabled"}>${escapeHTML(t("artifacts.prev_page"))}</button>
    <span>${escapeHTML(t("artifacts.showing", { start, end, total }))}</span>
    <button type="button" data-sync-page-offset="${nextOffset}" ${body.has_more ? "" : "disabled"}>${escapeHTML(t("artifacts.next_page"))}</button>
  `;
}

async function loadSyncSchedules() {
  const container = document.querySelector("#sync-schedules");
  if (!container) return;
  const body = await request("/api/sync-schedules");
  const schedules = body.schedules || [];
  syncSchedulesBySourceID = new Map(schedules.map((schedule) => [schedule.source_id, schedule]));
  container.innerHTML = schedules.length ? schedules.map(renderSyncSchedule).join("") : `<p class="muted">${escapeHTML(t("sync.schedules.empty"))}</p>`;
}

function renderSyncSchedule(schedule) {
  const statusParts = [];
  statusParts.push(schedule.enabled ? formatSourceSchedule(schedule.sync_schedule) : t("source.schedule.manual"));
  if (schedule.in_progress) statusParts.push(t("sync.status.running"));
  if (schedule.paused) statusParts.push(t("source.status.credential_required"));
  return `
    <div class="item">
      <strong>${escapeHTML(schedule.source_name || schedule.source_id)}</strong>
      <small>${escapeHTML(schedule.source_kind || "")} · ${escapeHTML(schedule.source_id || "")}</small>
      <div class="source-meta">
        <span>${escapeHTML(t("source.schedule"))}: ${escapeHTML(statusParts.join(" · "))}</span>
        <span>${escapeHTML(t("sync.last_run"))}: ${escapeHTML(formatJobTime(schedule.last_run_at) || "-")}</span>
        <span>${escapeHTML(t("sync.next_run"))}: ${escapeHTML(formatJobTime(schedule.next_run_at) || "-")}</span>
      </div>
      ${schedule.last_error ? `<p class="job-error">${escapeHTML(schedule.last_error)}</p>` : ""}
      <div class="source-actions">
        <button type="button" data-run-schedule="${escapeAttr(schedule.source_id || "")}" ${schedule.in_progress ? "disabled" : ""}>${escapeHTML(t("sync.run_now"))}</button>
        <button type="button" data-edit-schedule="${escapeAttr(schedule.source_id || "")}">${escapeHTML(t("sync.schedule.edit"))}</button>
        <button type="button" data-delete-schedule="${escapeAttr(schedule.source_id || "")}" ${schedule.enabled ? "" : "disabled"}>${escapeHTML(t("sync.schedule.delete"))}</button>
      </div>
    </div>
  `;
}

function openSyncScheduleDialog(schedule = null) {
  const dialog = document.querySelector("#sync-schedule-dialog");
  const form = document.querySelector("#sync-schedule-form");
  if (!dialog || !form) return;
  populateSyncSourceSelects();
  populateCredentialSelect();
  form.reset();
  if (schedule) {
    setFormValue(form, "source_id", schedule.source_id);
    setFormValue(form, "sync_schedule", normalizeSourceScheduleValue(schedule.sync_schedule) || "daily");
    setFormValue(form, "cookie_credential_id", schedule.cookie_credential_id);
  }
  updateSyncScheduleDialogMode(Boolean(schedule));
  updateSyncScheduleCredentialField();
  if (dialog.showModal) {
    dialog.showModal();
  }
}

function closeSyncScheduleDialog() {
  const dialog = document.querySelector("#sync-schedule-dialog");
  if (dialog?.open) {
    dialog.close();
  }
}

function updateSyncScheduleDialogMode(editing) {
  const title = document.querySelector("[data-sync-schedule-dialog-title]");
  const submit = document.querySelector("[data-sync-schedule-submit-label]");
  const key = editing ? "sync.schedule.edit" : "sync.schedule.add";
  if (title) {
    title.dataset.i18n = key;
    title.textContent = t(key);
  }
  if (submit) {
    submit.dataset.i18n = "sync.schedule.save";
    submit.textContent = t("sync.schedule.save");
  }
}

function updateSyncScheduleCredentialField() {
  const form = document.querySelector("#sync-schedule-form");
  const sourceID = form?.elements.source_id?.value || "";
  const source = sourcesByID.get(sourceID);
  const field = form?.querySelector('select[name="cookie_credential_id"]')?.closest(".field");
  if (!field) return;
  field.hidden = source?.kind !== "confluence";
}

async function onSyncSchedulesClick(event) {
  const button = event.target.closest("button[data-edit-schedule], button[data-delete-schedule], button[data-run-schedule]");
  if (!button) return;
  const sourceID = button.dataset.editSchedule || button.dataset.deleteSchedule || button.dataset.runSchedule || "";
  if (!sourceID) return;
  if (button.dataset.editSchedule) {
    openSyncScheduleDialog(syncSchedulesBySourceID.get(sourceID));
    return;
  }
  button.disabled = true;
  try {
    if (button.dataset.deleteSchedule) {
      await request(`/api/sync-schedules/${encodeURIComponent(sourceID)}`, { method: "DELETE" });
      await loadSources();
    } else if (button.dataset.runSchedule) {
      const schedule = syncSchedulesBySourceID.get(sourceID) || {};
      await request("/api/sync-tasks", {
        method: "POST",
        body: JSON.stringify({
          source_id: sourceID,
          cookie_credential_id: selectedCredentialForSource(sourceID, schedule.cookie_credential_id),
        }),
      });
    }
    await loadSyncTasksView();
    await loadStatus();
  } catch (error) {
    alert(error.message);
  } finally {
    button.disabled = false;
  }
}

async function onSyncScheduleSubmit(event) {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const sourceID = String(form.get("source_id") || "").trim();
  const payload = {
    source_id: sourceID,
    sync_schedule: String(form.get("sync_schedule") || "").trim(),
  };
  const credentialID = selectedCredentialForSource(sourceID, String(form.get("cookie_credential_id") || "").trim());
  if (credentialID) {
    payload.cookie_credential_id = credentialID;
  }
  try {
    await request("/api/sync-schedules", {
      method: "POST",
      body: JSON.stringify(payload),
    });
    closeSyncScheduleDialog();
    await loadSources();
    await loadSyncSchedules();
  } catch (error) {
    alert(error.message);
  }
}

function selectedCredentialForSource(sourceID, credentialID) {
  const source = sourcesByID.get(sourceID);
  if (source?.kind !== "confluence") {
    return "";
  }
  return credentialsByID.has(credentialID) ? credentialID : "";
}

function formatJobStatus(status) {
  const key = `sync.status.${String(status || "").trim()}`;
  const label = t(key);
  return label === key ? humanizeToken(status) : label;
}

function parsePayload(value) {
  try {
    const parsed = JSON.parse(value || "{}");
    return parsed && typeof parsed === "object" ? parsed : {};
  } catch {
    return {};
  }
}

function formatJobTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value || "";
  return date.toLocaleString(currentLang === "zh-CN" ? "zh-CN" : "en-US");
}

function humanizeToken(value) {
  return String(value || "")
    .trim()
    .replaceAll("_", " ")
    .replace(/\s+/g, " ");
}

function renderSourceArtifacts(body, page, limit) {
  const counts = body.counts || {};
  const sourceID = body.source_id || "";
  const documents = body.documents || [];
  const sections = body.sections || [];
  const nodes = body.nodes || [];
  const edges = body.edges || [];
  const totalDocs = counts.documents ?? 0;
  const start = page * limit;
  const end = Math.min(start + documents.length, totalDocs);
  const hasMore = start + documents.length < totalDocs;
  const hasPrev = page > 0;

  const paginationBar = (totalDocs > limit) ? `
    <div class="artifact-pagination">
      <button type="button" data-artifact-page="${sourceID}" data-page="${page - 1}" ${hasPrev ? "" : "disabled"}>${escapeHTML(t("artifacts.prev_page"))}</button>
      <span>${escapeHTML(t("artifacts.showing", { start: start + 1, end, total: totalDocs }))}</span>
      <button type="button" data-artifact-page="${sourceID}" data-page="${page + 1}" ${hasMore ? "" : "disabled"}>${escapeHTML(t("artifacts.next_page"))}</button>
    </div>
  ` : "";
  return `
    <div class="artifact-summary">
      <span>${escapeHTML(t("status.documents"))}: ${escapeHTML(counts.documents ?? 0)}</span>
      <span>${escapeHTML(t("status.sections"))}: ${escapeHTML(counts.sections ?? 0)}</span>
      <span>${escapeHTML(t("status.nodes"))}: ${escapeHTML(counts.nodes ?? 0)}</span>
      <span>${escapeHTML(t("status.edges"))}: ${escapeHTML(counts.edges ?? 0)}</span>
    </div>
    ${paginationBar}
    ${renderArtifactList(t("artifacts.documents"), documents.map((doc) => `
      <div class="artifact-row">
        <strong>${escapeHTML(doc.title || doc.external_id)}</strong>
        <small>${escapeHTML(doc.id)} · ${escapeHTML(t("node.id"))}: ${escapeHTML(doc.node_id)} · ${escapeHTML(t("status.sections"))}: ${escapeHTML(doc.section_count)}</small>
        <small>${escapeHTML(doc.url || doc.external_id)}</small>
        <div class="document-desc-editor">
          <input data-document-desc value="${escapeAttr(doc.profile?.desc || "")}" placeholder="${escapeAttr(t("document.desc.placeholder"))}">
          <button type="button" data-save-desc="${escapeAttr(doc.id)}">${escapeHTML(t("document.desc.save"))}</button>
        </div>
        <button type="button" data-open-node="${escapeAttr(doc.node_id)}">${escapeHTML(t("node.open"))}</button>
      </div>
    `))}
    ${renderArtifactList(t("artifacts.sections"), sections.map((section) => `
      <div class="artifact-row">
        <strong>${escapeHTML(section.title || section.heading_path || section.id)}</strong>
        <small>${escapeHTML(section.document_title)} · ${escapeHTML(section.id)} · ${escapeHTML(t("node.id"))}: ${escapeHTML(section.node_id)}</small>
        <p>${escapeHTML(section.content_snippet || "")}</p>
        <button type="button" data-open-node="${escapeAttr(section.node_id)}">${escapeHTML(t("node.open"))}</button>
      </div>
    `))}
    ${renderArtifactList(t("artifacts.nodes"), nodes.map((node) => `
      <div class="artifact-row">
        <strong>${escapeHTML(node.name)}</strong>
        <small>${escapeHTML(node.kind)} · ${escapeHTML(node.id)}</small>
        <button type="button" data-open-node="${escapeAttr(node.id)}">${escapeHTML(t("node.open"))}</button>
      </div>
    `))}
    ${renderArtifactList(t("artifacts.edges"), edges.map((edge) => `
      <div class="artifact-row">
        <strong>${escapeHTML(edge.kind)}</strong>
        <small>${escapeHTML(edge.src_kind)}:${escapeHTML(edge.src_name)} -&gt; ${escapeHTML(edge.dst_kind)}:${escapeHTML(edge.dst_name)}</small>
        <small>${escapeHTML(edge.id)} · ${escapeHTML(edge.provenance)} ${escapeHTML(edge.evidence_section_id || "")}</small>
      </div>
    `))}
  `;
}

async function hydrateDocumentProfiles(documents) {
  await Promise.all(documents.map(async (doc) => {
    if (!doc?.id) return;
    try {
      doc.profile = await request(`/api/documents/${encodeURIComponent(doc.id)}/profile`);
    } catch {
      doc.profile = { desc: "" };
    }
  }));
}

function renderArtifactList(title, rows) {
  if (!rows.length) return "";
  return `
    <details class="artifact-section" open>
      <summary>${escapeHTML(title)} (${rows.length})</summary>
      <div class="artifact-list">${rows.join("")}</div>
    </details>
  `;
}

async function onSearchSubmit(event) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const container = document.querySelector("#results");
  const summary = document.querySelector("#search-summary");
  const query = String(form.get("query") || "").trim();
  container.innerHTML = "";
  summary.textContent = t("search.searching");
  try {
    const limit = parseInt(form.get("limit"), 10) || 0;
    const body = await request("/api/search", {
      method: "POST",
      body: JSON.stringify({ query, limit }),
    });
    if (!body.hits?.length) {
      summary.textContent = t("search.empty_for", { query });
      return;
    }
    summary.textContent = t("search.result_count", { count: body.hits.length, query });
    body.hits.forEach((hit) => {
      const item = document.createElement("div");
      item.className = "search-result";
      const title = hit.title || hit.heading_path || hit.document_title || hit.document_id;
      const url = hit.document_url || hit.document_id || "";
      const path = [hit.document_title, hit.heading_path].filter(Boolean).join(" / ");
      const tags = hit.profile?.top_tags || [];
      const matchedFields = hit.query_match?.matched_fields || [];
      const relationMatches = renderRelationMatches(hit.relation_matches || []);
      const scoreBreakdown = renderScoreBreakdown(hit.score_breakdown, hit.rank);
      item.innerHTML = `
        <a class="result-title" href="${escapeAttr(hit.document_url || "#")}" ${hit.document_url ? 'target="_blank" rel="noreferrer"' : ""}>${escapeHTML(title)}</a>
        <div class="result-url">${escapeHTML(url)}</div>
        <div class="result-path">${escapeHTML(path || hit.document_id)}</div>
        ${hit.desc ? `<p class="result-desc">${escapeHTML(hit.desc)}</p>` : ""}
        ${tags.length ? `<div class="result-tags">${tags.slice(0, 6).map((tag) => `<span>${escapeHTML(tag)}</span>`).join("")}</div>` : ""}
        ${matchedFields.length ? `<small class="result-match">${escapeHTML(t("search.matched_fields"))}: ${escapeHTML(matchedFields.join(", "))}</small>` : ""}
        ${relationMatches}
        ${scoreBreakdown}
        <p class="result-snippet">${safeSnippetHTML(hit.snippet || hit.content || "")}</p>
        <div class="result-actions">
          <button type="button" data-feedback-canonical>${escapeHTML(t("feedback.canonical"))}</button>
          <button type="button" data-feedback-stale>${escapeHTML(t("feedback.stale"))}</button>
        </div>
      `;
      item.querySelector("[data-feedback-canonical]").dataset.documentId = hit.document_id || "";
      item.querySelector("[data-feedback-stale]").dataset.documentId = hit.document_id || "";
      container.appendChild(item);
    });
  } catch (error) {
    summary.textContent = "";
    container.innerHTML = "";
    alert(error.message);
  }
}

function renderRelationMatches(matches) {
  if (!matches.length) return "";
  return `
    <div class="relation-match-list">
      <strong>${escapeHTML(t("search.relation_matches"))}</strong>
      ${matches.slice(0, 4).map((match) => `
        <div class="relation-match">
          <span>${escapeHTML(match.relation_type || "")}</span>
          <small>${escapeHTML(match.effect || "context_link")} · ${escapeHTML(match.direction || "")} · ${escapeHTML(match.target_document_id || "")}</small>
          ${match.reason ? `<p>${escapeHTML(match.reason)}</p>` : ""}
        </div>
      `).join("")}
    </div>
  `;
}

function renderScoreBreakdown(score, rank) {
  if (!score && rank === undefined) return "";
  const total = Number(score?.total ?? rank ?? 0);
  if (!score) {
    return `<div class="result-score"><span>Total ${formatScore(total)}</span></div>`;
  }
  const parts = [
    ["unicode", score.unicode_bm25_boost],
    ["trigram", score.trigram_bm25_boost],
    ["title", score.title_boost],
    ["section", score.section_boost],
    ["symbol", score.symbol_boost],
    ["exact", score.exact_match_boost],
    ["canonical", score.canonical_boost],
    ["coverage", score.coverage_boost],
    ["fallback", score.fallback_boost],
  ].filter(([, value]) => Number(value || 0) !== 0);
  const terms = score.matched_terms || [];
  const symbols = score.matched_symbols || [];
  const fields = score.matched_fields || [];
  return `
    <details class="result-score">
      <summary>
        <span>Score</span>
        <strong>${formatScore(total)}</strong>
        ${parts.length ? `<small>${parts.map(([name, value]) => `${escapeHTML(name)} ${formatScore(value)}`).join(" · ")}</small>` : ""}
      </summary>
      <div class="score-grid">
        ${parts.map(([name, value]) => `
          <span>${escapeHTML(name)}</span>
          <strong>${formatScore(value)}</strong>
        `).join("")}
      </div>
      ${fields.length ? `<p><b>fields</b> ${escapeHTML(fields.join(", "))}</p>` : ""}
      ${symbols.length ? `<p><b>symbols</b> ${escapeHTML(symbols.slice(0, 12).join(", "))}</p>` : ""}
      ${terms.length ? `<p><b>terms</b> ${escapeHTML(terms.slice(0, 16).join(", "))}</p>` : ""}
    </details>
  `;
}

function formatScore(value) {
  const n = Number(value || 0);
  if (!Number.isFinite(n)) return "0";
  return n.toFixed(Math.abs(n) >= 10 ? 1 : 2);
}

async function onSearchFeedbackClick(event) {
  const button = event.target.closest("button[data-feedback-canonical], button[data-feedback-stale]");
  if (!button) return;

  const documentId = button.dataset.documentId;
  if (!documentId) return;

  button.disabled = true;
  const previous = button.textContent;
  try {
    await createFeedback({
      target_kind: "document",
      target_id: documentId,
      feedback_kind: button.dataset.feedbackStale !== undefined ? "document_stale" : "document_canonical",
      payload: {},
    });
    button.textContent = t("feedback.marked");
  } catch (error) {
    button.disabled = false;
    button.textContent = previous;
    alert(error.message);
  }
}

async function onKnowledgeRelationProposalFiltersSubmit(event) {
  event.preventDefault();
  await loadKnowledgeRelationProposals();
}

async function loadKnowledgeRelationProposals() {
  const container = document.querySelector("#relation-proposals");
  const form = document.querySelector("#relation-proposal-filters");
  if (!container || !form) return;
  container.innerHTML = `<p class="muted">${escapeHTML(t("governance.loading"))}</p>`;
  const data = new FormData(form);
  const params = new URLSearchParams();
  const status = String(data.get("status") || "pending").trim();
  const documentId = String(data.get("document_id") || "").trim();
  if (status) params.set("status", status);
  if (documentId) params.set("document_id", documentId);
  params.set("limit", "100");
  try {
    const body = await request(`/api/knowledge-relation-proposals?${params.toString()}`);
    const proposals = body.proposals || [];
    if (!proposals.length) {
      container.innerHTML = `<p class="muted">${escapeHTML(t("governance.empty"))}</p>`;
      return;
    }
    container.innerHTML = proposals.map(renderKnowledgeRelationProposal).join("");
  } catch (error) {
    container.innerHTML = "";
    alert(error.message);
  }
}

function renderKnowledgeRelationProposal(proposal) {
  const evidence = formatEvidence(proposal.evidence_json);
  const pending = proposal.status === "pending";
  return `
    <div class="item relation-proposal" data-proposal-id="${escapeAttr(proposal.id)}">
      <div class="relation-proposal-main">
        <strong>${escapeHTML(proposal.relation_type)} · ${escapeHTML(t(`governance.status.${proposal.status}`))}</strong>
        <small>${escapeHTML(proposal.from_document_id)} -> ${escapeHTML(proposal.to_document_id)}</small>
        <small>${escapeHTML(proposal.proposed_effect || "context_link")} · ${escapeHTML(proposal.direction || "directed")} · ${escapeHTML(proposal.created_by_type || "")}${proposal.created_by_ref ? `:${escapeHTML(proposal.created_by_ref)}` : ""}</small>
      </div>
      <p>${escapeHTML(proposal.reason || "")}</p>
      ${proposal.from_anchor || proposal.to_anchor ? `<small>${escapeHTML([proposal.from_anchor, proposal.to_anchor].filter(Boolean).join(" -> "))}</small>` : ""}
      ${evidence ? `<pre class="relation-evidence">${escapeHTML(evidence)}</pre>` : ""}
      ${proposal.review_note ? `<small>${escapeHTML(t("governance.review_note"))}: ${escapeHTML(proposal.review_note)}</small>` : ""}
      ${pending ? `
        <div class="result-actions">
          <button type="button" data-proposal-approve>${escapeHTML(t("governance.approve"))}</button>
          <button type="button" data-proposal-reject>${escapeHTML(t("governance.reject"))}</button>
        </div>
      ` : ""}
    </div>
  `;
}

function formatEvidence(raw) {
  raw = String(raw || "").trim();
  if (!raw || raw === "{}") return "";
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

async function onKnowledgeRelationProposalClick(event) {
  const approve = event.target.closest("button[data-proposal-approve]");
  const reject = event.target.closest("button[data-proposal-reject]");
  if (!approve && !reject) return;
  const item = event.target.closest("[data-proposal-id]");
  const proposalId = item?.dataset.proposalId || "";
  if (!proposalId) return;
  const action = approve ? "approve" : "reject";
  const note = window.prompt(t(action === "approve" ? "governance.approve_note" : "governance.reject_note"), "");
  if (note === null) return;
  const button = approve || reject;
  button.disabled = true;
  try {
    await request(`/api/knowledge-relation-proposals/${encodeURIComponent(proposalId)}/${action}`, {
      method: "POST",
      body: JSON.stringify({
        reviewed_by: "web",
        review_note: note,
      }),
    });
    await loadKnowledgeRelationProposals();
  } catch (error) {
    button.disabled = false;
    alert(error.message);
  }
}

async function onNodeSearchSubmit(event) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const query = String(form.get("query") || "").trim();
  const container = document.querySelector("#node-search-results");
  container.innerHTML = "";
  if (!query) return;
  try {
    const body = await request(`/api/nodes?query=${encodeURIComponent(query)}&limit=20`);
    if (!body.nodes?.length) {
      container.innerHTML = `<p class="muted">${escapeHTML(t("node.search.empty"))}</p>`;
      return;
    }
    body.nodes.forEach((node) => {
      const item = document.createElement("div");
      item.className = "item node-result";
      item.innerHTML = `
        <strong>${escapeHTML(node.name)}</strong>
        <small>${escapeHTML(node.kind)} · ${escapeHTML(node.id)}</small>
        <small>${escapeHTML(node.canonical_name || "")}</small>
        <button type="button" data-open-node="${escapeAttr(node.id)}">${escapeHTML(t("node.open"))}</button>
      `;
      container.appendChild(item);
    });
  } catch (error) {
    alert(error.message);
  }
}

function onNodeSearchResultsClick(event) {
  const button = event.target.closest("button[data-open-node]");
  if (!button) return;
  openNodeByID(button.dataset.openNode);
}

function openNodeByID(id) {
  id = String(id || "").trim();
  if (!id) return;
  navigate("nodes");
  const input = document.querySelector('#node-form input[name="id"]');
  input.value = id;
  document.querySelector("#node-form").requestSubmit();
}

async function onNodeSubmit(event) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const id = String(form.get("id") || "").trim();
  const direction = String(form.get("direction") || "both").trim();
  const kind = String(form.get("kind") || "").trim();
  const detail = document.querySelector("#node-detail");
  const relatedContainer = document.querySelector("#related");

  try {
    const node = await request(`/api/nodes/${encodeURIComponent(id)}`);
    const params = new URLSearchParams();
    if (direction) params.set("direction", direction);
    if (kind) params.set("kind", kind);
    params.set("limit", "20");
    const related = await request(`/api/nodes/${encodeURIComponent(id)}/related?${params.toString()}`);

    detail.innerHTML = "";
    const nodeItem = document.createElement("div");
    nodeItem.className = "item";
    nodeItem.innerHTML = `
      <strong>${escapeHTML(node.name)}</strong>
      <small>${escapeHTML(node.kind)} · ${escapeHTML(node.id)}</small>
      <p>${escapeHTML(node.canonical_name)}</p>
    `;
    detail.appendChild(nodeItem);
    document.querySelector('#manual-relation-form input[name="src_id"]').value = id;
    document.querySelector('#merge-node-form input[name="target_id"]').value = id;

    relatedContainer.innerHTML = "";
    if (!related.related?.length) {
      relatedContainer.innerHTML = `<p class="muted">${escapeHTML(t("node.related.empty"))}</p>`;
      return;
    }
    related.related.forEach((entry) => {
      const item = document.createElement("div");
      item.className = "item";
      item.innerHTML = `
        <strong>${escapeHTML(entry.node?.name)}</strong>
        <small>${escapeHTML(entry.direction)} · ${escapeHTML(entry.edge?.kind)} · ${escapeHTML(entry.node?.kind)} · ${escapeHTML(entry.node?.id)}</small>
        <p>${escapeHTML(entry.edge?.evidence_section_id || entry.node?.canonical_name || "")}</p>
        <div class="item-actions">
          <button type="button" data-feedback-wrong-edge>${escapeHTML(t("feedback.wrong_relation"))}</button>
        </div>
      `;
      item.querySelector("[data-feedback-wrong-edge]").dataset.edgeId = entry.edge?.id || "";
      relatedContainer.appendChild(item);
    });
  } catch (error) {
    detail.innerHTML = "";
    relatedContainer.innerHTML = "";
    alert(error.message);
  }
}

async function onRelatedFeedbackClick(event) {
  const button = event.target.closest("button[data-feedback-wrong-edge]");
  if (!button) return;

  const edgeId = button.dataset.edgeId;
  if (!edgeId) return;

  const feedbackId = button.dataset.feedbackId;

  if (feedbackId) {
    // Already marked — try to unmark (delete the feedback event).
    button.disabled = true;
    const previous = button.textContent;
    try {
      await request(`/api/feedback/${encodeURIComponent(feedbackId)}`, { method: "DELETE" });
      delete button.dataset.feedbackId;
      button.disabled = false;
      button.textContent = t("feedback.wrong_relation");
    } catch (error) {
      button.disabled = false;
      button.textContent = previous;
      alert(error.message);
    }
    return;
  }

  // Mark the relationship as wrong.
  button.disabled = true;
  const previous = button.textContent;
  try {
    const event = await createFeedback({
      target_kind: "edge",
      target_id: edgeId,
      feedback_kind: "relationship_wrong",
      payload: {},
    });
    button.dataset.feedbackId = event.id;
    button.disabled = false;
    button.textContent = t("feedback.unmark_wrong_relation");
  } catch (error) {
    button.disabled = false;
    button.textContent = previous;
    alert(error.message);
  }
}

async function onManualRelationSubmit(event) {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const srcID = String(form.get("src_id") || "").trim();
  const dstID = String(form.get("dst_id") || "").trim();
  const kind = String(form.get("kind") || "").trim();
  try {
    await createFeedback({
      target_kind: "node",
      target_id: srcID,
      feedback_kind: "relationship_add",
      payload: { src_id: srcID, dst_id: dstID, kind },
    });
    formElement.reset();
    document.querySelector('#node-form input[name="id"]').value = srcID;
    document.querySelector("#node-form").requestSubmit();
  } catch (error) {
    alert(error.message);
  }
}

async function onMergeNodeSubmit(event) {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const targetID = String(form.get("target_id") || "").trim();
  const mergedInto = String(form.get("merged_into") || "").trim();
  try {
    await createFeedback({
      target_kind: "node",
      target_id: targetID,
      feedback_kind: "node_merge",
      payload: { merged_into: mergedInto },
    });
    formElement.reset();
    document.querySelector('#node-form input[name="id"]').value = targetID;
    document.querySelector("#node-form").requestSubmit();
  } catch (error) {
    alert(error.message);
  }
}

async function onImpactClick() {
  const form = new FormData(document.querySelector("#node-form"));
  const id = String(form.get("id") || "").trim();
  const direction = String(form.get("direction") || "out").trim();
  const kind = String(form.get("kind") || "").trim();
  const maxDepth = Number(form.get("max_depth") || 2);
  const container = document.querySelector("#impact");

  try {
    const body = await request("/api/impact", {
      method: "POST",
      body: JSON.stringify({
        id,
        direction,
        kind,
        max_depth: maxDepth,
        limit: 30,
      }),
    });
    container.innerHTML = "";
    if (!body.paths?.length) {
      container.innerHTML = `<p class="muted">${escapeHTML(t("impact.empty"))}</p>`;
      return;
    }
    body.paths.forEach((path, index) => {
      const item = document.createElement("div");
      item.className = "item";
      const nodeNames = (path.nodes || []).map((node) => `${node.kind}:${node.name}`).join(" -> ");
      const edgeKinds = (path.edges || []).map((edge) => edge.kind).join(" -> ");
      const evidence = (path.edges || []).map((edge) => edge.evidence_section_id).filter(Boolean).join(", ");
      item.innerHTML = `
        <strong>${escapeHTML(t("impact.path", { index: index + 1 }))}</strong>
        <small>${escapeHTML(edgeKinds)}</small>
        <p>${escapeHTML(nodeNames)}</p>
        <small>${escapeHTML(evidence)}</small>
      `;
      container.appendChild(item);
    });
  } catch (error) {
    container.innerHTML = "";
    alert(error.message);
  }
}

async function createFeedback(body) {
  return request("/api/feedback", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function escapeAttr(value) {
  return escapeHTML(value).replaceAll("`", "&#096;");
}

function safeSnippetHTML(value) {
  return escapeHTML(value)
    .replaceAll("&lt;mark&gt;", "<mark>")
    .replaceAll("&lt;/mark&gt;", "</mark>");
}

init();
