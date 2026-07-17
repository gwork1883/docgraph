let messages = {};
let currentLang = "en";
let authMessageKey = "";
const syncingSourceIDs = new Set();
const activeSyncSourceIDs = new Set();
const activeEmbeddingSourceIDs = new Set();
const sourcesByID = new Map();
const credentialsByID = new Map();
const sourceArtifactPages = new Map();
const ARTIFACT_PAGE_SIZE = 50;
const SYNC_HISTORY_PAGE_SIZE = 20;
const SEARCH_INLINE_PREVIEW_MAX_BYTES = 2 * 1024 * 1024;
const MEDIA_FETCH_CONCURRENCY = 4;
const AUTH_TOKEN_STORAGE_KEY = "docgraph.auth.token";
const THEME_STORAGE_KEY = "docgraph.theme";
const WEB_PREFIX = detectWebPrefix();
let syncHistoryOffset = 0;
let syncSchedulesBySourceID = new Map();
let activeSyncTab = "history";
let activeDocumentID = "";
let activeTopicID = "";
let previousTopicRoute = "search";
let activeProposalStatus = "pending";
let appReady = false;
let displayedRoute = "";
const activeTopicObjectURLs = new Set();
const activeSearchObjectURLs = new Set();
let topicMediaGeneration = 0;
let searchMediaGeneration = 0;
let activeTopicChildrenState = null;

/* ── Toast notification system ── */
let toastContainer = null;
function ensureToastContainer() {
  if (!toastContainer) {
    toastContainer = document.createElement("div");
    toastContainer.className = "toast-container";
    document.body.appendChild(toastContainer);
  }
  return toastContainer;
}
function showToast(message, type = "success", duration = 3500) {
  const container = ensureToastContainer();
  const toast = document.createElement("div");
  toast.className = `toast ${type}`;
  const icons = { success: "✓", error: "✕", warning: "⚠" };
  toast.innerHTML = `<span class="toast-icon">${icons[type] || icons.success}</span><span class="toast-msg">${escapeHTML(message)}</span>`;
  container.appendChild(toast);
  const remove = () => {
    if (!toast.parentNode) return;
    toast.classList.add("removing");
    setTimeout(() => { if (toast.parentNode) toast.remove(); }, 260);
  };
  const timer = setTimeout(remove, duration);
  toast.addEventListener("click", () => { clearTimeout(timer); remove(); });
  return toast;
}

/* ── Replace window.alert with toast for better UX ── */
const _originalAlert = window.alert;
window.alert = function(msg) {
  showToast(String(msg), msg.toLowerCase().includes("error") || msg.toLowerCase().includes("fail") ? "error" : "warning", 4000);
};

const sourceHelpKeys = {
  local: "source.dsn.help.local",
  static: "source.dsn.help.static",
  html: "source.dsn.help.html",
  sftp: "source.dsn.help.sftp",
  openapi: "source.dsn.help.openapi",
  git: "source.dsn.help.git",
  confluence: "source.dsn.help.confluence",
  webdocs: "source.dsn.help.webdocs",
  xmind: "source.dsn.help.xmind",
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
  initTheme();
  await loadI18n(detectLanguage());
  bindEvents();
  syncRouteFromHash();
  updateSourceFormForKind();
  const status = await loadStatus();
  if (status === "unauthorized") {
    appReady = false;
    return;
  }
  hideLogin();
  appReady = true;
  await loadAppData();
}

function detectLanguage() {
  const saved = localStorage.getItem("docgraph.lang");
  if (saved) return saved;
  return (navigator.language || "en").toLowerCase().startsWith("zh") ? "zh-CN" : "en";
}

function initTheme() {
  const saved = localStorage.getItem(THEME_STORAGE_KEY);
  const prefersDark = !saved && window.matchMedia("(prefers-color-scheme: dark)").matches;
  if (saved === "light" || (!saved && !prefersDark)) {
    document.documentElement.setAttribute("data-theme", "light");
  } else {
    document.documentElement.removeAttribute("data-theme");
  }
}

function toggleTheme() {
  const current = document.documentElement.getAttribute("data-theme");
  if (current === "light") {
    document.documentElement.removeAttribute("data-theme");
    localStorage.setItem(THEME_STORAGE_KEY, "dark");
  } else {
    document.documentElement.setAttribute("data-theme", "light");
    localStorage.setItem(THEME_STORAGE_KEY, "light");
  }
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
      appReady = true;
      await loadAppData();
    } else {
      appReady = false;
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
  document.querySelector("#results").addEventListener("click", onSearchResultOpenClick);
  document.querySelector("#document-back-search")?.addEventListener("click", () => navigate("search"));
  document.querySelector("#document-detail")?.addEventListener("click", onDocumentDetailClick);
  document.querySelector("#topic-back-search")?.addEventListener("click", () => navigate(previousTopicRoute || "search"));
  document.querySelector("#topic-detail")?.addEventListener("click", onTopicDetailClick);
  document.querySelector("#stale-documents-refresh")?.addEventListener("click", () => loadStaleDocuments());
  document.querySelector("#stale-documents")?.addEventListener("click", onStaleDocumentsClick);
  document.querySelector("#relation-proposals-refresh")?.addEventListener("click", () => loadKnowledgeRelationProposals());
  document.querySelector("#proposal-status-tabs")?.addEventListener("click", onProposalStatusTabClick);
  document.querySelector("#proposal-filter-apply")?.addEventListener("click", () => loadKnowledgeRelationProposals());
  document.querySelector("#relation-proposals")?.addEventListener("click", onKnowledgeRelationProposalClick);
  document.querySelectorAll("[data-governance-tab]").forEach((button) => {
    button.addEventListener("click", () => {
      const tab = button.dataset.governanceTab;
      switchGovernanceTab(tab);
    });
  });
  document.querySelector("#theme-toggle")?.addEventListener("click", toggleTheme);
  document.querySelector("#node-search-form").addEventListener("submit", onNodeSearchSubmit);
  document.querySelector("#node-search-results").addEventListener("click", onNodeSearchResultsClick);
  document.querySelector("#node-form").addEventListener("submit", onNodeSubmit);
  document.querySelector("#related").addEventListener("click", onRelatedFeedbackClick);
  document.querySelector("#manual-relation-form").addEventListener("submit", onManualRelationSubmit);
  document.querySelector("#merge-node-form").addEventListener("submit", onMergeNodeSubmit);
  document.querySelector("#impact-button").addEventListener("click", onImpactClick);
}

function syncRouteFromHash() {
  const route = parseHashRoute();
  if (route.name === "document") {
    activeDocumentID = route.value;
  } else if (route.name === "topic") {
    activeTopicID = route.value;
  }
  navigate(route.name || "dashboard", false);
}

function navigate(route, updateHash = true) {
  const current = displayedRoute || currentRoute();
  const knownRoutes = new Set(["dashboard", "connectors", "sync-tasks", "credentials", "search", "document", "topic", "governance", "nodes"]);
  const nextRoute = knownRoutes.has(route) ? route : "dashboard";
  if (current === "topic" && nextRoute !== "topic") {
    topicMediaGeneration++;
    revokeObjectURLs(activeTopicObjectURLs);
  }
  if (current === "search" && nextRoute !== "search") {
    searchMediaGeneration++;
    revokeObjectURLs(activeSearchObjectURLs);
  }
  document.querySelectorAll("[data-view]").forEach((view) => {
    view.classList.toggle("active", view.dataset.view === nextRoute);
  });
  document.querySelectorAll("[data-route]").forEach((button) => {
    button.classList.toggle("active", button.dataset.route === nextRoute);
  });
  displayedRoute = nextRoute;
  let nextHash = `#${nextRoute}`;
  if (nextRoute === "document" && activeDocumentID) {
    nextHash = `#document/${encodeURIComponent(activeDocumentID)}`;
  } else if (nextRoute === "topic" && activeTopicID) {
    nextHash = `#topic/${encodeURIComponent(activeTopicID)}`;
  }
  if (updateHash && location.hash !== nextHash) {
    history.replaceState(null, "", nextHash);
  }
  if (nextRoute === "governance") {
    switchGovernanceTab(activeGovernanceTab(), false);
  }
  if (!appReady) {
    return Promise.resolve();
  }
  return loadRouteData(nextRoute).catch((error) => {
    if (!isAuthError(error)) console.error(error);
  });
}

function switchGovernanceTab(tab, load = true) {
  document.querySelectorAll("[data-governance-tab]").forEach((button) => {
    button.classList.toggle("active", button.dataset.governanceTab === tab);
  });
  document.querySelectorAll("[data-governance-panel]").forEach((panel) => {
    panel.classList.toggle("active", panel.dataset.governancePanel === tab);
  });
  if (!load) return;
  loadGovernanceTab(tab).catch((error) => {
    if (!isAuthError(error)) console.error(error);
  });
}

function activeGovernanceTab() {
  return document.querySelector("[data-governance-tab].active")?.dataset.governanceTab || "stale";
}

async function loadGovernanceTab(tab) {
  if (tab === "stale") {
    await loadStaleDocuments();
  } else if (tab === "relations") {
    await loadKnowledgeRelationProposals();
  }
}

function parseHashRoute() {
  const raw = String(location.hash || "#dashboard").replace(/^#/, "");
  const [name, value = ""] = raw.split("/");
  return {
    name: name || "dashboard",
    value: decodeURIComponent(value || ""),
  };
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
    await loadRouteData(currentRoute());
  } catch (error) {
    if (!isAuthError(error)) {
      console.error(error);
    }
  }
}

async function loadRouteData(route) {
  switch (route) {
  case "connectors":
    await loadCredentials();
    await loadSources();
    break;
  case "credentials":
    await loadCredentials();
    break;
  case "sync-tasks":
    await loadSources();
    await loadSyncTasksView();
    break;
  case "document":
    await loadDocumentDetail(activeDocumentID);
    break;
  case "topic":
    await loadTopicDetail(activeTopicID);
    break;
  case "governance":
    await loadGovernanceTab(activeGovernanceTab());
    break;
  default:
    break;
  }
}

function currentRoute() {
  return parseHashRoute().name;
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

async function fetchAuthenticatedBlob(path) {
  const response = await apiFetch(path);
  if (!response.ok) {
    let message = t("error.request_failed", { status: response.status });
    try {
      const body = await response.clone().json();
      message = body.error?.message || message;
    } catch {
      // Binary endpoints may return an empty or plain-text error response.
    }
    const error = new Error(message);
    error.auth = response.status === 401;
    throw error;
  }
  return {
    blob: await response.blob(),
    filename: responseFilename(response.headers.get("Content-Disposition")),
  };
}

function responseFilename(contentDisposition) {
  const value = String(contentDisposition || "");
  const encoded = value.match(/filename\*=UTF-8''([^;]+)/i)?.[1];
  if (encoded) {
    try {
      return decodeURIComponent(encoded.replace(/^"|"$/g, ""));
    } catch {
      return encoded;
    }
  }
  return value.match(/filename="([^"]+)"/i)?.[1] || value.match(/filename=([^;]+)/i)?.[1]?.trim() || "";
}

function revokeObjectURLs(urls) {
  urls.forEach((url) => URL.revokeObjectURL(url));
  urls.clear();
}

async function authenticatedObjectURL(path, owner) {
  const { blob } = await fetchAuthenticatedBlob(path);
  const url = URL.createObjectURL(blob);
  owner.add(url);
  return url;
}

async function downloadAuthenticated(path, fallbackName = "download") {
  const { blob, filename } = await fetchAuthenticatedBlob(path);
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename || fallbackName;
  link.hidden = true;
  document.body.appendChild(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
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
    appReady = false;
    showLogin("auth.invalid");
    return;
  }
  hideLogin();
  appReady = true;
  await loadAppData();
}

async function onLogoutClick() {
  clearAuthToken();
  const status = await loadStatus();
  if (status === "unauthorized") {
    appReady = false;
    showLogin("auth.logged_out");
    return;
  }
  hideLogin();
  appReady = true;
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
  topicMediaGeneration++;
  searchMediaGeneration++;
  revokeObjectURLs(activeTopicObjectURLs);
  revokeObjectURLs(activeSearchObjectURLs);
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
  activeEmbeddingSourceIDs.clear();
  if (!body.sources?.length) {
    populateSyncSourceSelects();
    container.innerHTML = `<p class="muted">${escapeHTML(t("sources.empty"))}</p>`;
    return;
  }
  await loadActiveSyncSourceIDs();
  await loadActiveEmbeddingSourceIDs();
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
        <span class="source-state" data-embedding-status="${escapeAttr(source.id)}">${escapeHTML(t("source.vector_status_loading"))}</span>
      </div>
      <div class="source-actions">
        <button data-edit="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.edit"))}</button>
        <button data-sync="${escapeAttr(source.id)}" type="button" ${syncing ? "disabled" : ""}>${escapeHTML(t(syncing ? "source.syncing" : "source.sync"))}</button>
        <button data-embedding-ensure="${escapeAttr(source.id)}" type="button" ${activeEmbeddingSourceIDs.has(source.id) ? "disabled" : ""}>${escapeHTML(t(activeEmbeddingSourceIDs.has(source.id) ? "source.vector_ensuring" : "source.vector_ensure"))}</button>
        <button data-sync-tasks-source="${escapeAttr(source.id)}" type="button">${escapeHTML(t("sync.view_tasks"))}</button>
        <button data-artifacts="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.artifacts"))}</button>
        <button data-delete="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.delete"))}</button>
      </div>
      <div class="source-artifacts" data-artifacts-list="${escapeAttr(source.id)}"></div>
    `;
    container.appendChild(item);
  });
  populateSyncSourceSelects();
  await loadSourceEmbeddingStatuses(sources);
}

async function loadSourceEmbeddingStatuses(sources) {
  await Promise.all(sources.map(async (source) => {
    const target = document.querySelector(`[data-embedding-status="${CSS.escape(source.id)}"]`);
    if (!target) return;
    try {
      const status = await request(`/api/sources/${encodeURIComponent(source.id)}/embedding-status`);
      target.textContent = formatEmbeddingStatus(source.id, status);
      target.classList.toggle("warn", status.status === "disabled" || (status.status === "indexing" && !activeEmbeddingSourceIDs.has(source.id)));
    } catch (error) {
      target.textContent = t("source.vector_status_error");
      target.classList.add("warn");
    }
  }));
}

function formatEmbeddingStatus(sourceID, status) {
  const label = t("source.vector_status");
  if (!status?.enabled) {
    return `${label}: ${t("source.vector_disabled")}`;
  }
  const total = Number(status.total_sections || 0);
  const embedded = Number(status.embedded_sections || 0);
  const stale = Number(status.stale_sections || 0);
  const pending = Number(status.pending_sections || 0);
  const state = embeddingStateLabel(sourceID, status);
  return `${label}: ${state} ${embedded}/${total} (${pending} pending, ${stale} stale)`;
}

function embeddingStateLabel(sourceID, status) {
  if (!status?.enabled) return t("source.vector_disabled");
  if (activeEmbeddingSourceIDs.has(sourceID)) return t("source.vector_indexing");
  if (status.status === "ready") return t("source.vector_ready");
  const pending = Number(status.pending_sections || 0);
  const stale = Number(status.stale_sections || 0);
  if (pending > 0 && stale > 0) return t("source.vector_pending_stale");
  if (pending > 0) return t("source.vector_pending");
  if (stale > 0) return t("source.vector_stale");
  return t("source.vector_ready");
}

async function loadActiveSyncSourceIDs() {
  const statuses = ["queued", "running", "canceling"];
  await Promise.all(statuses.map(async (status) => {
    const body = await request(`/api/jobs?kind=sync_source&status=${encodeURIComponent(status)}&limit=100`);
    (body.jobs || []).forEach((job) => {
      if (job.source_id) {
        activeSyncSourceIDs.add(job.source_id);
      }
    });
  }));
}

async function loadActiveEmbeddingSourceIDs() {
  const statuses = ["queued", "running", "canceling"];
  await Promise.all(statuses.map(async (status) => {
    const body = await request(`/api/jobs?kind=maintenance_embedding_ensure&status=${encodeURIComponent(status)}&limit=100`);
    (body.jobs || []).forEach((job) => {
      if (job.source_id) {
        activeEmbeddingSourceIDs.add(job.source_id);
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
    const crawlMode = String(form.get("crawl_mode") || "").trim();
    if (crawlMode && crawlMode !== "auto") {
      config.crawl_mode = crawlMode;
    }
    if (crawlMode === "browser" || crawlMode === "in_page") {
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
  if (form.elements.crawl_mode) {
    form.elements.crawl_mode.value = normalizeWebDocsCrawlMode(config);
  }
}

function normalizeWebDocsCrawlMode(config) {
  const mode = String(config?.crawl_mode || "").trim();
  if (["auto", "static", "browser", "in_page"].includes(mode)) {
    return mode;
  }
  if (config?.is_spa === true) {
    return "browser";
  }
  return "auto";
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
  const button = event.target.closest("button[data-edit], button[data-sync], button[data-embedding-ensure], button[data-sync-tasks-source], button[data-artifacts], button[data-artifact-page], button[data-delete], button[data-open-node], button[data-open-document], button[data-save-desc], button[data-download-snapshot]");
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

  if (button.dataset.openDocument) {
    openDocumentByID(button.dataset.openDocument);
    return;
  }

  if (button.dataset.downloadSnapshot) {
    button.disabled = true;
    try {
      await downloadAuthenticated(`/api/sources/${encodeURIComponent(button.dataset.downloadSnapshot)}/snapshot`, "workbook.xmind");
    } catch (error) {
      alert(error.message);
    } finally {
      button.disabled = false;
    }
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
    try {
      await loadSourceArtifacts(sourceID, page);
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
    await navigate("sync-tasks");
    return;
  }

  if (button.dataset.embeddingEnsure) {
    const sourceID = button.dataset.embeddingEnsure;
    if (!sourceID || activeEmbeddingSourceIDs.has(sourceID)) {
      return;
    }
    button.disabled = true;
    button.textContent = t("source.vector_ensuring");
    try {
      await request(`/api/maintenance/embedding-ensure?source_id=${encodeURIComponent(sourceID)}`, { method: "POST" });
      activeEmbeddingSourceIDs.add(sourceID);
      showToast(t("source.vector_ensure_queued"));
      await loadSources();
      await loadSyncTasksView();
      await loadStatus();
    } catch (error) {
      button.disabled = false;
      button.textContent = t("source.vector_ensure");
      alert(error.message);
    }
    return;
  }

  if (button.dataset.artifacts) {
    button.disabled = true;
    try {
      sourceArtifactPages.set(button.dataset.artifacts, 0);
      await loadSourceArtifacts(button.dataset.artifacts, 0);
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
  const button = event.target.closest("button[data-delete-sync-job], button[data-cancel-sync-job], button[data-sync-history-source]");
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
  if (button.dataset.cancelSyncJob) {
    const jobID = button.dataset.cancelSyncJob || "";
    if (!sourceID || !jobID) return;
    button.disabled = true;
    try {
      await request(`/api/sources/${encodeURIComponent(sourceID)}/jobs/${encodeURIComponent(jobID)}/cancel`, { method: "POST" });
      activeSyncSourceIDs.add(sourceID);
      await loadSources();
      await loadSyncJobHistory();
      await loadStatus();
    } catch (error) {
      alert(error.message);
    } finally {
      button.disabled = false;
    }
    return;
  }
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
  params.set("limit", String(limit));
  params.set("offset", String(syncHistoryOffset));
  const kind = String(data.get("kind") || "").trim();
  const sourceID = String(data.get("source_id") || "").trim();
  const status = String(data.get("status") || "").trim();
  if (kind) params.set("kind", kind);
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
  const progress = parsePayload(job.progress_json);
  const summary = { ...payload, ...result };
  const connectorDiagnostics = parsePayload(summary.connector_diagnostics);
  const connectorWarnings = Array.isArray(connectorDiagnostics.warnings) ? connectorDiagnostics.warnings : [];
  const brokenLinks = Array.isArray(summary.broken_links) ? summary.broken_links : [];
  const error = String(job.last_error || "").trim();
  const active = isActiveJobStatus(job.status);
  const isSyncJob = job.kind === "sync_source";
  const actionButton = active && isSyncJob
    ? `<button type="button" data-source-id="${escapeAttr(job.source_id || "")}" data-cancel-sync-job="${escapeAttr(job.id || "")}">${escapeHTML(t("jobs.cancel"))}</button>`
    : isSyncJob ? `<button type="button" data-source-id="${escapeAttr(job.source_id || "")}" data-delete-sync-job="${escapeAttr(job.id || "")}">${escapeHTML(t("jobs.delete"))}</button>` : "";
  return `
    <div class="job-row">
      <div class="job-main">
        <strong>${escapeHTML(formatJobKind(job.kind))} · ${escapeHTML(formatJobStatus(job.status))}</strong>
        <small>${escapeHTML(formatJobTime(job.updated_at || job.created_at))}</small>
      </div>
      <small>${escapeHTML(sourceName)} · ${escapeHTML(job.id || "")}</small>
      <div class="job-meta">
        ${renderJobMetrics(job, summary, progress, brokenLinks, connectorDiagnostics)}
        <span>${escapeHTML(t("sync.attempts"))}: ${escapeHTML(job.attempts ?? 0)}</span>
        ${progress.phase ? `<span>${escapeHTML(t("jobs.progress"))}: ${escapeHTML(formatJobProgress(progress))}</span>` : ""}
      </div>
      ${error ? `<p class="job-error">${escapeHTML(error)}</p>` : ""}
      ${connectorWarnings.length ? `<div class="health-warning-list">${connectorWarnings.map((warning) => `<p>${escapeHTML(warning)}</p>`).join("")}</div>` : ""}
      ${brokenLinks.length ? renderBrokenLinks(brokenLinks) : ""}
      <div class="item-actions">
        <button type="button" data-sync-history-source="${escapeAttr(job.source_id || "")}">${escapeHTML(t("sync.filter_source"))}</button>
        ${actionButton}
      </div>
    </div>
  `;
}

function renderJobMetrics(job, summary, progress, brokenLinks, connectorDiagnostics = {}) {
  if (job.kind === "maintenance_embedding_ensure") {
    return `
      <span>${escapeHTML(t("jobs.embedding.embedded"))}: ${escapeHTML(summary.embedded_sections ?? progress.embedded_sections ?? 0)}</span>
      <span>${escapeHTML(t("jobs.embedding.skipped"))}: ${escapeHTML(summary.skipped_sections ?? progress.skipped_sections ?? 0)}</span>
      <span>${escapeHTML(t("jobs.embedding.scanned"))}: ${escapeHTML(summary.scanned_sections ?? progress.scanned_sections ?? 0)}</span>
      <span>${escapeHTML(t("jobs.embedding.embedded_chunks"))}: ${escapeHTML(summary.embedded_chunks ?? progress.embedded_chunks ?? 0)}</span>
      <span>${escapeHTML(t("jobs.embedding.scanned_chunks"))}: ${escapeHTML(summary.scanned_chunks ?? progress.scanned_chunks ?? 0)}</span>
      <span>${escapeHTML(t("jobs.embedding.pending_batch"))}: ${escapeHTML(progress.pending_batch_sections ?? 0)}</span>
      <span>${escapeHTML(t("jobs.embedding.pending_batch_chunks"))}: ${escapeHTML(progress.pending_batch_chunks ?? 0)}</span>
      <span>${escapeHTML(t("status.sections"))}: ${escapeHTML(summary.total_sections ?? progress.total_sections ?? 0)}</span>
    `;
  }
  return `
    <span>${escapeHTML(t("jobs.documents"))}: ${escapeHTML(summary.documents ?? 0)}</span>
    <span>${escapeHTML(t("jobs.broken_links"))}: ${escapeHTML(brokenLinks.length)}</span>
    ${connectorDiagnostics.parse_status ? `<span>${escapeHTML(t("jobs.parse_status"))}: ${escapeHTML(formatParseStatus(connectorDiagnostics.parse_status))}</span>` : ""}
    ${connectorDiagnostics.format_family ? `<span>${escapeHTML(t("jobs.format"))}: ${escapeHTML(connectorDiagnostics.format_family)}${connectorDiagnostics.format_version ? ` ${escapeHTML(connectorDiagnostics.format_version)}` : ""}</span>` : ""}
    ${Number.isFinite(Number(connectorDiagnostics.topics)) ? `<span>${escapeHTML(t("jobs.topics"))}: ${escapeHTML(connectorDiagnostics.topics)}</span>` : ""}
    ${Number.isFinite(Number(connectorDiagnostics.media_assets)) ? `<span>${escapeHTML(t("jobs.media_assets"))}: ${escapeHTML(connectorDiagnostics.media_assets)}</span>` : ""}
  `;
}

function formatParseStatus(status) {
  const key = `jobs.parse_status.${String(status || "").trim()}`;
  const label = t(key);
  return label === key ? humanizeToken(status) : label;
}

function formatJobKind(kind) {
  switch (kind) {
    case "sync_source":
      return t("jobs.kind.sync");
    case "maintenance_embedding_ensure":
      return t("jobs.kind.embedding");
    default:
      return humanizeToken(kind || "job");
  }
}

function isActiveJobStatus(status) {
  return ["queued", "running", "canceling"].includes(String(status || "").trim());
}

function formatJobProgress(progress) {
  const parts = [humanizeToken(progress.phase)];
  if (Number.isFinite(Number(progress.visited))) {
    parts.push(`${progress.visited} ${t("jobs.pages")}`);
  }
  if (progress.url) {
    parts.push(String(progress.url));
  }
  return parts.filter(Boolean).join(" · ");
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
  if (value && typeof value === "object") {
    return value;
  }
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

async function loadSourceArtifacts(sourceID, page) {
  const limit = ARTIFACT_PAGE_SIZE;
  const offset = page * limit;
  const [body, health] = await Promise.all([
    request(`/api/sources/${encodeURIComponent(sourceID)}/artifacts?limit=${limit}&offset=${offset}`),
    request(`/api/sources/${encodeURIComponent(sourceID)}/health`),
  ]);
  await hydrateDocumentProfiles(body.documents || []);
  body.health = health;
  const container = document.querySelector(`[data-artifacts-list="${CSS.escape(sourceID)}"]`);
  if (container) {
    container.innerHTML = renderSourceArtifacts(body, page, limit);
  }
}

function renderSourceArtifacts(body, page, limit) {
  const counts = body.counts || {};
  const health = body.health || {};
  const sourceID = body.source_id || "";
  const documents = body.documents || [];
  const sections = body.sections || [];
  const sectionEntities = body.section_entities || [];
  const nodes = body.nodes || [];
  const edges = body.edges || [];
  const embeddingStatus = body.embedding_status || null;
  const snapshot = body.active_snapshot || body.snapshot || health.active_snapshot || health.snapshot || null;
  const featureInventory = body.feature_inventory || health.feature_inventory || null;
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
      <span>${escapeHTML(t("health.section_entities"))}: ${escapeHTML(counts.section_entities ?? 0)}</span>
      <span>${escapeHTML(t("status.nodes"))}: ${escapeHTML(counts.nodes ?? 0)}</span>
      <span>${escapeHTML(t("status.edges"))}: ${escapeHTML(counts.edges ?? 0)}</span>
      <span>${escapeHTML(t("media.assets"))}: ${escapeHTML(counts.media_assets ?? 0)}</span>
      <span>${escapeHTML(t("media.unavailable"))}: ${escapeHTML(counts.unavailable_media_assets ?? ((counts.missing_media_assets ?? 0) + (counts.rejected_media_assets ?? 0)))}</span>
      <span>${escapeHTML(t("media.bytes"))}: ${escapeHTML(formatBytes(counts.referenced_media_bytes ?? 0))}</span>
    </div>
    ${snapshot ? `
      <div class="artifact-summary source-snapshot-summary">
        <span>${escapeHTML(t("snapshot.current"))}: ${escapeHTML(snapshot.id || snapshot.snapshot_id || snapshot.sha256 || snapshot.source_hash || "")}</span>
        <span>${escapeHTML(t("snapshot.size"))}: ${escapeHTML(formatBytes(snapshot.size_bytes ?? counts.snapshot_bytes ?? counts.snapshot_size_bytes ?? 0))}</span>
        <button type="button" data-download-snapshot="${escapeAttr(sourceID)}">${escapeHTML(t("snapshot.download"))}</button>
      </div>
    ` : ""}
    ${renderFeatureInventory(featureInventory)}
    ${renderEmbeddingArtifactSummary(sourceID, embeddingStatus)}
    ${renderSourceHealth(health)}
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
        <button type="button" data-open-document="${escapeAttr(doc.id)}">${escapeHTML(t("document.open"))}</button>
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
    ${renderArtifactList(t("artifacts.section_entities"), sectionEntities.map(renderSectionEntityRow))}
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

function renderEmbeddingArtifactSummary(sourceID, status) {
  if (!status) return "";
  const total = Number(status.total_sections || 0);
  const embedded = Number(status.embedded_sections || 0);
  const pending = Number(status.pending_sections || 0);
  const stale = Number(status.stale_sections || 0);
  const totalChunks = Number(status.total_chunks || 0);
  const embeddedChunks = Number(status.embedded_chunks || 0);
  const pendingChunks = Number(status.pending_chunks || 0);
  const staleChunks = Number(status.stale_chunks || 0);
  return `
    <div class="artifact-summary">
      <span>${escapeHTML(t("artifacts.embedding_status"))}: ${escapeHTML(embeddingStateLabel(sourceID, status))}</span>
      <span>${escapeHTML(t("source.vector_ready"))}: ${escapeHTML(`${embedded}/${total}`)}</span>
      <span>${escapeHTML(t("source.vector_pending"))}: ${escapeHTML(pending)}</span>
      <span>${escapeHTML(t("source.vector_stale"))}: ${escapeHTML(stale)}</span>
      <span>${escapeHTML(t("artifacts.embedding_chunks"))}: ${escapeHTML(`${embeddedChunks}/${totalChunks}`)}</span>
      <span>${escapeHTML(t("artifacts.embedding_chunk_pending"))}: ${escapeHTML(pendingChunks)}</span>
      <span>${escapeHTML(t("artifacts.embedding_chunk_stale"))}: ${escapeHTML(staleChunks)}</span>
      ${status.model ? `<span>${escapeHTML(t("artifacts.embedding_model"))}: ${escapeHTML(status.model)}</span>` : ""}
      ${status.tokenizer ? `<span>${escapeHTML(t("artifacts.embedding_tokenizer"))}: ${escapeHTML(status.tokenizer)}</span>` : ""}
      ${status.chunk_strategy ? `<span>${escapeHTML(t("artifacts.embedding_strategy"))}: ${escapeHTML(status.chunk_strategy)}</span>` : ""}
      ${status.generator_version ? `<span>${escapeHTML(t("artifacts.embedding_generator"))}: ${escapeHTML(status.generator_version)}</span>` : ""}
    </div>
  `;
}

function renderSourceHealth(health) {
  if (!health?.source_id) return "";
  const warnings = health.warnings || [];
  const latestJob = health.latest_job || {};
  const brokenLinks = health.broken_links || [];
  const zeroDocs = health.zero_section_documents || [];
  const lowSections = health.low_content_sections || [];
  const staleFeedback = health.stale_feedback || [];
  const entityDiagnostics = health.entity_diagnostics || {};
  const featureInventory = health.feature_inventory || null;
  const snapshot = health.active_snapshot || health.snapshot || null;
  const counts = health.counts || {};
  const healthClass = warnings.some((warning) => warning.severity === "error") ? "error" : warnings.length ? "warn" : "ok";
  return `
    <details class="source-health ${escapeAttr(healthClass)}" open>
      <summary>
        <span>${escapeHTML(t("health.source_health"))}</span>
        <strong>${escapeHTML(formatSourceHealthLabel(healthClass, warnings.length))}</strong>
      </summary>
      <div class="source-health-grid">
        <span>${escapeHTML(t("health.latest_sync"))}: ${escapeHTML(latestJob.id ? `${formatJobStatus(latestJob.status)} · ${formatJobTime(latestJob.updated_at || latestJob.created_at)}` : t("health.no_sync"))}</span>
        <span>${escapeHTML(t("health.broken_links"))}: ${escapeHTML(brokenLinks.length)}</span>
        <span>${escapeHTML(t("health.zero_section_documents"))}: ${escapeHTML(zeroDocs.length)}</span>
        <span>${escapeHTML(t("health.low_content_sections"))}: ${escapeHTML(lowSections.length)}</span>
        <span>${escapeHTML(t("health.section_entities"))}: ${escapeHTML(entityDiagnostics.total ?? health.counts?.section_entities ?? 0)}</span>
        <span>${escapeHTML(t("media.assets"))}: ${escapeHTML(counts.media_assets ?? 0)}</span>
        <span>${escapeHTML(t("media.unavailable"))}: ${escapeHTML(counts.unavailable_media_assets ?? ((counts.missing_media_assets ?? 0) + (counts.rejected_media_assets ?? 0)))}</span>
        <span>${escapeHTML(t("media.bytes"))}: ${escapeHTML(formatBytes(counts.referenced_media_bytes ?? 0))}</span>
      </div>
      ${snapshot ? `<div class="source-health-grid"><span>${escapeHTML(t("snapshot.current"))}: ${escapeHTML(snapshot.id || snapshot.snapshot_id || snapshot.sha256 || snapshot.source_hash || "")}</span><span>${escapeHTML(t("snapshot.size"))}: ${escapeHTML(formatBytes(snapshot.size_bytes ?? counts.snapshot_bytes ?? counts.snapshot_size_bytes ?? 0))}</span></div>` : ""}
      ${renderFeatureInventory(featureInventory)}
      ${warnings.length ? `<div class="health-warning-list">${warnings.map(renderHealthWarning).join("")}</div>` : `<p class="muted">${escapeHTML(t("health.no_warnings"))}</p>`}
      ${renderHealthDocumentList(t("health.zero_section_documents"), zeroDocs)}
      ${renderHealthSectionList(t("health.low_content_sections"), lowSections)}
      ${renderHealthBrokenLinks(brokenLinks)}
      ${renderHealthStaleFeedback(staleFeedback)}
    </details>
  `;
}

function formatBytes(value) {
  const bytes = Number(value || 0);
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const index = Math.min(units.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)));
  const amount = bytes / Math.pow(1024, index);
  return `${amount >= 10 || index === 0 ? amount.toFixed(0) : amount.toFixed(1)} ${units[index]}`;
}

function renderFeatureInventory(inventory) {
  const entries = normalizeFeatureInventory(inventory);
  if (!entries.length) return "";
  return `
    <details class="feature-inventory" open>
      <summary>${escapeHTML(t("feature_inventory.title"))} (${entries.reduce((sum, entry) => sum + entry.count, 0)})</summary>
      <div class="feature-inventory-grid">
        ${entries.map((entry) => `
          <div class="feature-inventory-row ${escapeAttr(entry.state)}">
            <strong>${escapeHTML(entry.feature)}</strong>
            <span>${escapeHTML(t(`feature_inventory.state.${entry.state}`))}</span>
            <span>${escapeHTML(entry.count)}</span>
            ${entry.detail ? `<small>${escapeHTML(entry.detail)}</small>` : ""}
          </div>
        `).join("")}
      </div>
    </details>
  `;
}

function normalizeFeatureInventory(inventory) {
  if (!inventory) return [];
  const direct = Array.isArray(inventory)
    ? inventory
    : (inventory.entries || (Array.isArray(inventory.features) ? [...inventory.features, ...(inventory.resources || [])] : inventory.features));
  if (Array.isArray(direct)) {
    return direct.map((entry) => {
      const metadata = parsePayload(entry.metadata_json);
      const samplePaths = Array.isArray(metadata.sample_paths) ? metadata.sample_paths.filter(Boolean).slice(0, 3) : [];
      const detailParts = [
        metadata.reason_code,
        entry.element_path || entry.detail || entry.reason || entry.reason_code || entry.location || entry.path,
        samplePaths.join(", "),
        metadata.details_truncated ? t("feature_inventory.samples_truncated") : "",
      ].filter(Boolean);
      return {
        feature: String(entry.feature_key || entry.feature || entry.name || entry.type || entry.path || t("feature_inventory.unknown")),
        state: normalizeFeatureState(entry.coverage_status || entry.state || entry.status),
        count: Math.max(1, Number(entry.count || entry.occurrences || 1)),
        detail: [...new Set(detailParts.map(String))].join(" · "),
      };
    });
  }
  const entries = [];
  ["indexed", "preserved", "rejected", "unsupported"].forEach((state) => {
    const group = inventory[state];
    if (group == null) return;
    if (Array.isArray(group)) {
      group.forEach((item) => {
        if (typeof item === "string") {
          entries.push({ feature: item, state, count: 1, detail: "" });
        } else {
          entries.push({ feature: String(item.feature || item.name || item.type || item.path || t("feature_inventory.unknown")), state, count: Math.max(1, Number(item.count || 1)), detail: String(item.detail || item.reason || item.path || "") });
        }
      });
    } else if (typeof group === "object") {
      Object.entries(group).forEach(([feature, value]) => {
        const count = typeof value === "number" ? value : Number(value?.count || 1);
        const detail = typeof value === "object" ? String(value.detail || value.reason || value.path || "") : "";
        entries.push({ feature, state, count: Math.max(0, count), detail });
      });
    } else {
      entries.push({ feature: state, state, count: Math.max(0, Number(group || 0)), detail: "" });
    }
  });
  return entries.filter((entry) => entry.count > 0);
}

function normalizeFeatureState(value) {
  const state = String(value || "unsupported").toLowerCase().replaceAll("-", "_");
  if (state === "preserved_only") return "preserved";
  return ["indexed", "preserved", "rejected", "unsupported"].includes(state) ? state : "unsupported";
}

function renderSectionEntityRow(entity) {
  const label = formatEntityLabel(entity);
  return `
    <div class="artifact-row">
      <strong>${escapeHTML(label)}</strong>
      <small>${escapeHTML(entity.kind || "")} · ${escapeHTML(entity.source || "")} · ${escapeHTML(entity.section_id || "")}</small>
      <small>${escapeHTML(t("search.matched_fields"))}: ${escapeHTML(entity.confidence ?? "")}</small>
    </div>
  `;
}

function formatEntityLabel(entity) {
  if (!entity) return "";
  if (entity.method && entity.path) return `${entity.method} ${entity.path}`;
  return entity.path || entity.operation || entity.canonical || entity.canonical_text || entity.id || "";
}

function formatSourceHealthLabel(kind, count) {
  if (kind === "error") return t("health.status.error", { count });
  if (kind === "warn") return t("health.status.warn", { count });
  return t("health.status.ok");
}

function renderHealthWarning(warning) {
  return `
    <div class="health-warning ${escapeAttr(warning.severity || "info")}">
      <strong>${escapeHTML(t(`health.issue.${warning.kind}`) === `health.issue.${warning.kind}` ? humanizeToken(warning.kind) : t(`health.issue.${warning.kind}`))}</strong>
      <span>${escapeHTML(warning.message || "")}${warning.count ? ` · ${escapeHTML(warning.count)}` : ""}</span>
    </div>
  `;
}

function renderHealthDocumentList(title, documents) {
  if (!documents?.length) return "";
  return `
    <details class="health-subsection">
      <summary>${escapeHTML(title)} (${documents.length})</summary>
      <div class="artifact-list">
        ${documents.slice(0, 20).map((doc) => `
          <div class="artifact-row">
            <strong>${escapeHTML(doc.title || doc.external_id || doc.id)}</strong>
            <small>${escapeHTML(doc.id)} · ${escapeHTML(doc.url || doc.external_id || "")}</small>
            <button type="button" data-open-document="${escapeAttr(doc.id)}">${escapeHTML(t("document.open"))}</button>
          </div>
        `).join("")}
      </div>
    </details>
  `;
}

function renderHealthSectionList(title, sections) {
  if (!sections?.length) return "";
  return `
    <details class="health-subsection">
      <summary>${escapeHTML(title)} (${sections.length})</summary>
      <div class="artifact-list">
        ${sections.slice(0, 20).map((section) => `
          <div class="artifact-row">
            <strong>${escapeHTML(section.title || section.heading_path || section.id)}</strong>
            <small>${escapeHTML(section.document_title || "")} · ${escapeHTML(section.id)}</small>
            <p>${escapeHTML(section.content_snippet || "")}</p>
            ${section.node_id ? `<button type="button" data-open-node="${escapeAttr(section.node_id)}">${escapeHTML(t("node.open"))}</button>` : ""}
          </div>
        `).join("")}
      </div>
    </details>
  `;
}

function renderHealthBrokenLinks(links) {
  if (!links?.length) return "";
  return `
    <details class="health-subsection">
      <summary>${escapeHTML(t("health.broken_links"))} (${links.length})</summary>
      <div class="artifact-list">
        ${links.slice(0, 20).map((link) => `
          <div class="artifact-row">
            <strong>${escapeHTML(link.href || "")}</strong>
            <small>${escapeHTML(link.source_document || "")}${link.source_section ? ` / ${escapeHTML(link.source_section)}` : ""}</small>
            ${link.text ? `<p>${escapeHTML(link.text)}</p>` : ""}
          </div>
        `).join("")}
      </div>
    </details>
  `;
}

function renderHealthStaleFeedback(events) {
  if (!events?.length) return "";
  return `
    <details class="health-subsection">
      <summary>${escapeHTML(t("health.stale_feedback"))} (${events.length})</summary>
      <div class="artifact-list">
        ${events.slice(0, 20).map((event) => `
          <div class="artifact-row">
            <strong>${escapeHTML(event.target_id || "")}</strong>
            <small>${escapeHTML(formatJobTime(event.created_at))}${event.actor ? ` · ${escapeHTML(event.actor)}` : ""}</small>
            <button type="button" data-open-document="${escapeAttr(event.target_id)}">${escapeHTML(t("document.open"))}</button>
          </div>
        `).join("")}
      </div>
    </details>
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
  const mediaGeneration = ++searchMediaGeneration;
  revokeObjectURLs(activeSearchObjectURLs);
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
    if (mediaGeneration !== searchMediaGeneration || currentRoute() !== "search") return;
    if (!body.hits?.length) {
      summary.textContent = t("search.empty_for", { query });
      return;
    }
    summary.textContent = t("search.result_count", { count: body.hits.length, query });
    container.insertAdjacentHTML("beforeend", renderHybridSearchMeta(body));
    container.insertAdjacentHTML("beforeend", renderSearchAttempts(body));
    body.hits.forEach((hit) => {
      const item = document.createElement("div");
      item.className = "search-result";
      const rawTitle = hit.title || hit.heading_path || hit.document_title || hit.document_id;
      const title = [hit.display_number, rawTitle].filter(Boolean).join(" ");
      const url = hit.document_url || hit.document_id || "";
      const ancestry = normalizeAncestry(hit.ancestry);
      const path = [hit.document_title, ...(ancestry.length ? ancestry.map(topicEntryTitle) : [hit.heading_path])].filter(Boolean).join(" / ");
      const tags = hit.profile?.top_tags || [];
      const matchedFields = hit.query_match?.matched_fields || [];
      const matchedEntities = hit.matched_entities || [];
      const relationMatches = renderRelationMatches(hit.relation_matches || []);
      const scoreBreakdown = renderScoreBreakdown(hit.score_breakdown, hit.rank, hit.rrf_contribution);
      const evidence = renderEvidence(hit);
      const media = renderSearchMediaAssets(hit.media_assets || []);
      const authoredRelationCount = Number(hit.authored_relation_count || 0);
      item.innerHTML = `
        <button class="result-title result-title-button" type="button" ${hit.section_id ? `data-open-topic="${escapeAttr(hit.section_id)}"` : `data-open-document="${escapeAttr(hit.document_id || "")}"`}>${escapeHTML(title)}</button>
        <div class="result-url">${escapeHTML(url)}</div>
        <details class="result-path" ${ancestry.length > 3 ? "" : "open"}><summary>${escapeHTML(t("topic.ancestry"))}</summary><span>${escapeHTML(path || hit.document_id)}</span></details>
        ${evidence}
        <div class="result-tags">
          ${hit.element_kind ? `<span>${escapeHTML(hit.element_kind)}</span>` : ""}
          ${hit.node_id ? `<span>${escapeHTML(t("node.id"))}: ${escapeHTML(hit.node_id)}</span>` : ""}
          ${authoredRelationCount ? `<span>${escapeHTML(t("topic.authored_relations"))}: ${escapeHTML(authoredRelationCount)}</span>` : ""}
        </div>
        ${hit.desc ? `<p class="result-desc">${escapeHTML(hit.desc)}</p>` : ""}
        ${tags.length ? `<div class="result-tags">${tags.slice(0, 6).map((tag) => `<span>${escapeHTML(tag)}</span>`).join("")}</div>` : ""}
        ${matchedFields.length ? `<small class="result-match">${escapeHTML(t("search.matched_fields"))}: ${escapeHTML(matchedFields.join(", "))}</small>` : ""}
        ${matchedEntities.length ? `<div class="result-tags">${matchedEntities.slice(0, 4).map((entity) => `<span>${escapeHTML(formatEntityLabel(entity))}</span>`).join("")}</div>` : ""}
        ${relationMatches}
        ${scoreBreakdown}
        <p class="result-snippet">${safeSnippetHTML(hit.snippet || hit.content || "")}</p>
        ${media}
        <div class="result-actions">
          ${hit.document_url ? `<a class="secondary-link-button" href="${escapeAttr(hit.document_url)}" target="_blank" rel="noreferrer">${escapeHTML(t("document.open_source"))}</a>` : ""}
          <button type="button" data-feedback-canonical>${escapeHTML(t("feedback.canonical"))}</button>
          <button type="button" data-feedback-stale>${escapeHTML(t("feedback.stale"))}</button>
        </div>
      `;
      item.querySelector("[data-feedback-canonical]").dataset.documentId = hit.document_id || "";
      item.querySelector("[data-feedback-stale]").dataset.documentId = hit.document_id || "";
      container.appendChild(item);
    });
    await hydrateMediaImages(container, activeSearchObjectURLs, "[data-media-image-asset]", mediaGeneration);
  } catch (error) {
    if (mediaGeneration !== searchMediaGeneration) return;
    summary.textContent = "";
    container.innerHTML = "";
    alert(error.message);
  }
}

function renderSearchMediaAssets(assets) {
  if (!Array.isArray(assets) || !assets.length) return "";
  const shown = assets.slice(0, 3);
  const remaining = Math.max(0, assets.length - shown.length);
  return `
    <div class="search-media-strip">
      ${shown.map((asset) => {
        const id = mediaAssetID(asset);
        const status = mediaAssetStatus(asset);
		if (mediaIsSearchPreviewable(asset) && mediaCanLoad(asset) && id) {
          return `<div class="search-media-thumb" data-media-image-asset="${escapeAttr(id)}"><span>${escapeHTML(t("media.loading"))}</span></div>`;
        }
        return `<div class="search-media-thumb media-placeholder ${escapeAttr(status)}"><strong>${escapeHTML(mediaAssetName(asset))}</strong><span>${escapeHTML(mediaStatusLabel(asset))}</span></div>`;
      }).join("")}
      ${remaining ? `<span class="search-media-remaining">+${escapeHTML(remaining)}</span>` : ""}
    </div>
  `;
}

async function hydrateMediaImages(root, owner, selector, generation) {
	const targets = [...root.querySelectorAll(selector)];
	const groups = new Map();
	for (const target of targets) {
		const assetID = target.dataset.mediaImageAsset || target.dataset.topicImageAsset;
		if (!assetID) continue;
		if (!groups.has(assetID)) groups.set(assetID, []);
		groups.get(assetID).push(target);
	}
	const entries = [...groups.entries()];
	let cursor = 0;
	async function worker() {
		while (cursor < entries.length) {
			const [assetID, assetTargets] = entries[cursor++];
		try {
			const url = await authenticatedObjectURL(`/api/media-assets/${encodeURIComponent(assetID)}/content`, owner);
			const routeChanged = (owner === activeTopicObjectURLs && currentRoute() !== "topic") || (owner === activeSearchObjectURLs && currentRoute() !== "search");
			const generationChanged = (owner === activeTopicObjectURLs && generation !== topicMediaGeneration) || (owner === activeSearchObjectURLs && generation !== searchMediaGeneration);
			const connectedTargets = assetTargets.filter((target) => target.isConnected);
			if (!connectedTargets.length || routeChanged || generationChanged) {
				URL.revokeObjectURL(url);
				owner.delete(url);
				continue;
			}
			for (const target of connectedTargets) {
				const image = document.createElement("img");
				image.src = url;
				image.alt = target.dataset.mediaAlt || "";
				image.loading = "lazy";
				target.replaceChildren(image);
			}
		} catch (error) {
			for (const target of assetTargets) {
				target.classList.add("media-placeholder", "missing");
				target.replaceChildren(document.createTextNode(error.message));
			}
		}
		}
	}
	await Promise.all(Array.from({ length: Math.min(MEDIA_FETCH_CONCURRENCY, entries.length) }, () => worker()));
}

function mediaAssetID(asset) {
  return String(asset?.id || asset?.asset_id || "").trim();
}

function mediaAssetName(asset) {
  const metadata = mediaMetadata(asset);
  return String(asset?.original_name || asset?.filename || asset?.name || asset?.external_id || metadata.original_name || metadata.filename || mediaAssetID(asset) || t("media.unnamed"));
}

function mediaAssetStatus(asset) {
  return String(asset?.status || (asset?.blob_sha256 ? "preserved" : "missing")).toLowerCase().replaceAll("-", "_");
}

function mediaCanLoad(asset) {
  const status = mediaAssetStatus(asset);
  return Boolean(mediaAssetID(asset)) && !["missing", "rejected", "external", "unsupported", "preserved_only"].includes(status) && asset?.has_content !== false;
}

function mediaIsPreviewableImage(asset) {
  const mediaType = String(asset?.media_type || asset?.sniffed_media_type || "").toLowerCase().split(";", 1)[0].trim();
  return ["image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp"].includes(mediaType);
}

function mediaIsSearchPreviewable(asset) {
	const size = Number(asset?.size_bytes || asset?.size || 0);
	return mediaIsPreviewableImage(asset) && size > 0 && size <= SEARCH_INLINE_PREVIEW_MAX_BYTES;
}

function mediaMetadata(asset) {
  return parsePayload(asset?.metadata_json || asset?.metadata || "{}");
}

function mediaStatusLabel(asset) {
  const status = mediaAssetStatus(asset);
  const key = `media.status.${status}`;
  const label = t(key);
  return label === key ? humanizeToken(status) : label;
}

function renderHybridSearchMeta(body) {
  const meta = body.hybrid_search_meta;
  if (!meta) return "";
  const routeLabel = { entity: "Entity/Code", conceptual: "Conceptual/Q&A", general: "General" };
  const route = routeLabel[meta.intent_route] || meta.intent_route || "general";
  const wText = Number(meta.w_text ?? 0.5);
  const wVector = Number(meta.w_vector ?? 0.5);
  const rrfK = Number(meta.rrf_k ?? 60);
  const textCount = Number(meta.text_candidates ?? 0);
  const vecCount = Number(meta.vector_candidates ?? 0);
  const minSim = Number(meta.vector_min_similarity ?? 0);
  return `
    <div class="hybrid-search-meta">
      <span class="meta-route">${escapeHTML(route)}</span>
      <span>w_text=${formatScore(wText)} w_vec=${formatScore(wVector)}</span>
      <span>k=${rrfK}</span>
      <span>text:${textCount} vec:${vecCount}</span>
      ${vecCount ? `<span>min_sim=${formatScore(minSim)}</span>` : ""}
    </div>
  `;
}

function renderSearchAttempts(body) {
  const attempts = body.attempts || [];
  if (!attempts.length && !body.searches_used) return "";
  return `
    <details class="result-score search-attempts" open>
      <summary>
        <span>${escapeHTML(t("search.attempts"))}</span>
        <strong>${escapeHTML(String(body.searches_used || attempts.length || 0))}</strong>
        ${attempts.length ? `<small>${attempts.map((attempt) => `${escapeHTML(attempt.kind || "")} ${Number(attempt.hits || 0)}`).join(" · ")}</small>` : ""}
      </summary>
      <div class="score-grid">
        ${attempts.map((attempt) => `
          <span>${escapeHTML([attempt.kind || "", attempt.query || (attempt.terms || []).join(", ")].filter(Boolean).join(": "))}</span>
          <strong>${escapeHTML(attempt.error ? `${Number(attempt.hits || 0)} · ${attempt.error}` : String(Number(attempt.hits || 0)))}</strong>
        `).join("")}
      </div>
    </details>
  `;
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

function renderEvidence(hit) {
  const trace = hit.trace || {};
  const evidenceLevel = hit.evidence_level || "";
  const traceParts = [
    trace.source_id ? `source ${trace.source_id}` : "",
    trace.document_id ? `doc ${trace.document_id}` : "",
    trace.section_id ? `section ${trace.section_id}` : "",
    trace.chunk_id ? `chunk ${trace.chunk_id}` : "",
    Number.isFinite(Number(trace.chunk_ordinal)) && Number(trace.chunk_ordinal) > 0 ? `chunk# ${trace.chunk_ordinal}` : "",
    trace.vector_trace_valid ? "trace valid" : "",
  ].filter(Boolean);
  if (!evidenceLevel && !traceParts.length) return "";
  return `
    <div class="result-tags">
      ${evidenceLevel ? `<span>${escapeHTML(evidenceLevel)}</span>` : ""}
      ${traceParts.length ? `<span>${escapeHTML(traceParts.join(" · "))}</span>` : ""}
    </div>
  `;
}

function renderScoreBreakdown(score, rank, rrf) {
  if (!score && rank === undefined && !rrf) return "";
  // When RRF contribution exists, show RRF score as primary, ScoreBreakdown as explanation
  if (rrf) {
    const rrfScore = Number(rrf.final_score ?? rrf.rrf_score ?? 0);
    const textRank = rrf.text_rank || "-";
    const vectorRank = rrf.vector_rank || "-";
    const rawSim = Number(rrf.raw_similarity ?? 0);
    const rawBM25 = Number(rrf.raw_bm25_score ?? 0);
    const multiplier = Number(rrf.multiplier ?? 1);
    const sources = (rrf.source_evidence || []).join(", ");
    const rrfSummaryParts = [
      rrf.text_rank ? `text#${textRank}` : "",
      rrf.vector_rank ? `vec#${vectorRank}` : "",
      rawSim ? `sim=${formatScore(rawSim)}` : "",
      rawBM25 ? `bm25=${formatScore(rawBM25)}` : "",
      multiplier !== 1 ? `×${formatScore(multiplier)}` : "",
    ].filter(Boolean);
    const breakdownParts = score ? [
      ["unicode", score.unicode_bm25_boost],
      ["trigram", score.trigram_bm25_boost],
      ["title", score.title_boost],
      ["section", score.section_boost],
      ["symbol", score.symbol_boost],
      ["exact", score.exact_match_boost],
      ["canonical", score.canonical_boost],
      ["coverage", score.coverage_boost],
      ["fallback", score.fallback_boost],
      ["vector", score.vector_boost],
    ].filter(([, value]) => Number(value || 0) !== 0) : [];
    const terms = score?.matched_terms || [];
    const symbols = score?.matched_symbols || [];
    const fields = score?.matched_fields || [];
    return `
      <details class="result-score">
        <summary>
          <span>RRF</span>
          <strong>${formatScore(rrfScore)}</strong>
          ${rrfSummaryParts.length ? `<small>${rrfSummaryParts.map(p => escapeHTML(p)).join(" · ")}</small>` : ""}
        </summary>
        <div class="score-grid">
          <span>rrf_score</span><strong>${formatScore(rrf.rrf_score)}</strong>
          <span>final_score</span><strong>${formatScore(rrfScore)}</strong>
          <span>text_rank</span><strong>${escapeHTML(String(textRank))}</strong>
          <span>vector_rank</span><strong>${escapeHTML(String(vectorRank))}</strong>
          <span>text_rrf</span><strong>${formatScore(rrf.text_rrf)}</strong>
          <span>vector_rrf</span><strong>${formatScore(rrf.vector_rrf)}</strong>
          ${rawSim ? `<span>raw_similarity</span><strong>${formatScore(rawSim)}</strong>` : ""}
          ${rawBM25 ? `<span>raw_bm25</span><strong>${formatScore(rawBM25)}</strong>` : ""}
          <span>multiplier</span><strong>${formatScore(multiplier)}</strong>
          ${sources ? `<span>sources</span><strong>${escapeHTML(sources)}</strong>` : ""}
        </div>
        ${breakdownParts.length ? `
          <div class="score-grid" style="margin-top:6px;border-top:1px solid var(--border);padding-top:6px">
            ${breakdownParts.map(([name, value]) => `
              <span>${escapeHTML(name)}</span>
              <strong>${formatScore(value)}</strong>
            `).join("")}
          </div>
        ` : ""}
        ${fields.length ? `<p><b>fields</b> ${escapeHTML(fields.join(", "))}</p>` : ""}
        ${symbols.length ? `<p><b>symbols</b> ${escapeHTML(symbols.slice(0, 12).join(", "))}</p>` : ""}
        ${terms.length ? `<p><b>terms</b> ${escapeHTML(terms.slice(0, 16).join(", "))}</p>` : ""}
      </details>
    `;
  }
  // No RRF data — show legacy ScoreBreakdown (text-only mode)
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
    ["vector", score.vector_boost],
    ["vector penalty", -Number(score.vector_only_penalty || 0)],
    ["stale embedding", -Number(score.stale_embedding_penalty || 0)],
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
  if (Math.abs(n) >= 100) return n.toFixed(0);
  if (Math.abs(n) >= 10) return n.toFixed(1);
  if (Math.abs(n) >= 0.1) return n.toFixed(2);
  if (Math.abs(n) >= 0.001) return n.toFixed(4);
  return n.toFixed(6);
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

function onSearchResultOpenClick(event) {
  const topicButton = event.target.closest("button[data-open-topic]");
  if (topicButton) {
    openTopicByID(topicButton.dataset.openTopic, "search");
    return;
  }
  const button = event.target.closest("button[data-open-document]");
  if (!button) return;
  openDocumentByID(button.dataset.openDocument);
}

function openDocumentByID(id) {
  activeDocumentID = String(id || "").trim();
  if (!activeDocumentID) return;
  navigate("document");
}

function openTopicByID(id, fromRoute = "") {
  activeTopicID = String(id || "").trim();
  if (!activeTopicID) return;
  const route = fromRoute || currentRoute();
  if (route && route !== "topic") {
    previousTopicRoute = route;
  }
  navigate("topic");
}

async function loadTopicDetail(sectionID) {
  const container = document.querySelector("#topic-detail");
  const subtitle = document.querySelector("#topic-detail-subtitle");
  if (!container) return;
	const mediaGeneration = ++topicMediaGeneration;
	activeTopicChildrenState = null;
  revokeObjectURLs(activeTopicObjectURLs);
  sectionID = String(sectionID || "").trim();
  if (!sectionID) {
    container.innerHTML = `<p class="muted">${escapeHTML(t("topic.empty"))}</p>`;
    if (subtitle) subtitle.textContent = "";
    return;
  }
  container.innerHTML = `<p class="muted">${escapeHTML(t("topic.loading"))}</p>`;
  try {
		const detail = await request(`/api/sections/${encodeURIComponent(sectionID)}/context`);
		if (mediaGeneration !== topicMediaGeneration || activeTopicID !== sectionID || currentRoute() !== "topic") return;
		activeTopicChildrenState = normalizeTopicChildrenState(detail, sectionID);
		detail.children = activeTopicChildrenState.sections;
		detail.children_page = activeTopicChildrenState;
    const section = topicRecord(detail.section);
    const documentInfo = detail.document || {};
    const source = detail.source || {};
    if (subtitle) {
      subtitle.textContent = [source.name || source.id, documentInfo.title || documentInfo.id, section.id || sectionID].filter(Boolean).join(" · ");
    }
    container.innerHTML = renderTopicDetail(detail);
    await hydrateMediaImages(container, activeTopicObjectURLs, "[data-topic-image-asset]", mediaGeneration);
  } catch (error) {
    if (mediaGeneration !== topicMediaGeneration) return;
    container.innerHTML = "";
    if (subtitle) subtitle.textContent = "";
    alert(error.message);
  }
}

function renderTopicDetail(detail) {
  const section = topicRecord(detail.section);
  const documentInfo = detail.document || {};
  const source = detail.source || {};
  const structure = detail.structure || section.structure || {};
  const ancestors = normalizeAncestry(detail.ancestors);
	const children = Array.isArray(detail.children) ? detail.children : [];
	const childrenPage = detail.children_page || {};
  const assets = Array.isArray(detail.media_assets) ? detail.media_assets : [];
  const relations = Array.isArray(detail.authored_relations) ? detail.authored_relations : [];
  const displayNumber = String(structure.display_number || section.display_number || "").trim();
  const rawTitle = String(section.raw_title || section.title || "").trim();
  const displayTitle = [displayNumber, rawTitle].filter(Boolean).join(" ") || section.id || t("topic.untitled");
  const metadata = topicMetadata(section, structure);
  const sourceID = source.id || section.source_id || documentInfo.source_id || detail.snapshot?.source_id || "";
  return `
    ${renderTopicBreadcrumb(ancestors, section)}
    <header class="topic-header">
      <div>
        <span class="topic-kind">${escapeHTML(structure.element_kind || section.element_kind || "Topic")}</span>
        <h3>${escapeHTML(displayTitle)}</h3>
        ${displayNumber && rawTitle ? `<p class="muted">${escapeHTML(t("topic.raw_title"))}: ${escapeHTML(rawTitle)}</p>` : ""}
      </div>
      <div class="topic-header-actions">
        ${sourceID && detail.snapshot ? `<button type="button" data-download-snapshot="${escapeAttr(sourceID)}">${escapeHTML(t("snapshot.download"))}</button>` : ""}
      </div>
    </header>
    <div class="topic-summary-grid">
      <div><span>${escapeHTML(t("topic.sheet"))}</span><strong>${escapeHTML(documentInfo.title || documentInfo.external_id || documentInfo.id || "")}</strong></div>
      <div><span>${escapeHTML(t("topic.depth"))}</span><strong>${escapeHTML(structure.depth ?? "")}</strong></div>
      <div><span>${escapeHTML(t("topic.sibling_order"))}</span><strong>${escapeHTML(structure.sibling_ordinal ?? structure.ordinal ?? "")}</strong></div>
      <div><span>${escapeHTML(t("topic.media"))}</span><strong>${escapeHTML(assets.length)}</strong></div>
      <div><span>${escapeHTML(t("topic.authored_relations"))}</span><strong>${escapeHTML(relations.length)}</strong></div>
    </div>
    ${section.content ? `<section class="topic-section"><h4>${escapeHTML(t("topic.notes"))}</h4><div class="topic-notes">${renderSourceText(section.content)}</div></section>` : ""}
    ${renderTopicMetadata(metadata)}
    ${renderTopicMedia(assets)}
		${renderTopicChildren(children, childrenPage)}
    ${renderTopicRelations(relations, section.id)}
    ${renderFeatureInventory(detail.feature_inventory)}
    <details class="topic-section topic-provenance" open>
      <summary>${escapeHTML(t("topic.provenance"))}</summary>
      <div class="document-meta-list">
        <small>${escapeHTML(t("topic.topic_id"))}: ${escapeHTML(structure.source_element_id || section.source_element_id || section.id || "")}</small>
        <small>${escapeHTML(t("document.field.id"))}: ${escapeHTML(section.id || "")}</small>
        <small>${escapeHTML(t("node.id"))}: ${escapeHTML(section.node_id || "")}</small>
        <small>${escapeHTML(t("topic.sheet_id"))}: ${escapeHTML(documentInfo.external_id || documentInfo.id || "")}</small>
        <small>${escapeHTML(t("topic.order_path"))}: ${escapeHTML(formatOrderPath(structure.order_path || structure.order_path_json))}</small>
        <small>${escapeHTML(t("snapshot.current"))}: ${escapeHTML(detail.snapshot?.id || detail.snapshot?.snapshot_id || detail.snapshot?.sha256 || detail.snapshot?.source_hash || "")}</small>
      </div>
    </details>
  `;
}

function topicRecord(value) {
  const section = value?.section || value || {};
  if (section.id || !section.section_id) return section;
  return { ...section, id: section.section_id };
}

function normalizeAncestry(value) {
  return Array.isArray(value) ? value.map(topicRecord) : [];
}

function topicEntryTitle(value) {
  const entry = topicRecord(value);
  const structure = value?.structure || entry.structure || {};
  const title = entry.raw_title || entry.title || entry.heading_path || entry.id || "";
  const number = structure.display_number || entry.display_number || "";
  return [number, title].filter(Boolean).join(" ");
}

function renderTopicBreadcrumb(ancestors, section) {
  const entries = [...ancestors, section].filter((entry) => entry?.id || topicEntryTitle(entry));
  if (!entries.length) return "";
  return `
    <nav class="topic-breadcrumb" aria-label="${escapeAttr(t("topic.ancestry"))}">
      ${entries.map((entry, index) => {
        const current = index === entries.length - 1;
        const title = topicEntryTitle(entry);
        return `${index ? `<span aria-hidden="true">›</span>` : ""}${!current && entry.id ? `<button type="button" data-open-topic="${escapeAttr(entry.id)}">${escapeHTML(title)}</button>` : `<strong>${escapeHTML(title)}</strong>`}`;
      }).join("")}
    </nav>
  `;
}

function topicMetadata(section, structure) {
  const raw = parsePayload(section.metadata_json || section.metadata || "{}");
  const presentation = parsePayload(structure.presentation_json || structure.presentation || "{}");
  return {
    labels: section.labels || raw.labels || [],
    markers: section.markers || raw.markers || [],
    task: section.task || section.tasks || raw.task || raw.tasks || null,
    links: section.links || raw.links || [],
    numbering: section.numbering || raw.numbering || presentation.numbering || null,
    position: section.position || raw.position || presentation.position || null,
  };
}

function renderTopicMetadata(metadata) {
  const rows = [
    [t("topic.labels"), metadata.labels],
    [t("topic.markers"), metadata.markers],
    [t("topic.tasks"), metadata.task],
    [t("topic.links"), metadata.links],
    [t("topic.numbering"), metadata.numbering],
    [t("topic.position"), metadata.position],
  ].filter(([, value]) => hasDisplayValue(value));
  if (!rows.length) return "";
  return `
    <section class="topic-section">
      <h4>${escapeHTML(t("topic.metadata"))}</h4>
      <div class="topic-metadata-grid">
        ${rows.map(([label, value]) => `<div><span>${escapeHTML(label)}</span><strong>${escapeHTML(formatMetadataValue(value))}</strong></div>`).join("")}
      </div>
    </section>
  `;
}

function hasDisplayValue(value) {
  if (Array.isArray(value)) return value.length > 0;
  if (value && typeof value === "object") return Object.keys(value).length > 0;
  return value !== null && value !== undefined && String(value).trim() !== "";
}

function formatMetadataValue(value) {
  if (Array.isArray(value)) return value.map((item) => formatMetadataValue(item)).filter(Boolean).join(", ");
  if (value && typeof value === "object") {
    return Object.entries(value).map(([key, item]) => `${humanizeToken(key)}: ${formatMetadataValue(item)}`).join(" · ");
  }
  return String(value ?? "");
}

function renderSourceText(value) {
  return String(value || "").split(/\n{2,}/).map((paragraph) => `<p>${escapeHTML(paragraph).replaceAll("\n", "<br>")}</p>`).join("");
}

function renderTopicChildren(children, page = {}) {
	const hasMore = page.has_more === true || page.hasMore === true;
	const loadedLabel = `${children.length}${hasMore ? "+" : ""}`;
	return `
		<section class="topic-section" data-topic-children-section>
			<h4>${escapeHTML(t("topic.children"))} (${escapeHTML(loadedLabel)})</h4>
			${children.length ? `<div class="topic-child-list">${children.map((child) => {
				const section = topicRecord(child);
				const structure = child.structure || section.structure || {};
				return `<button type="button" data-open-topic="${escapeAttr(section.id || child.section_id || "")}"><span>${escapeHTML(topicEntryTitle(child))}</span><small>${escapeHTML(structure.element_kind || section.element_kind || "Topic")}</small></button>`;
			}).join("")}</div>` : `<p class="muted">${escapeHTML(t("topic.children_empty"))}</p>`}
			${hasMore ? `<button type="button" data-load-topic-children>${escapeHTML(t("topic.children_load_more"))}</button>` : ""}
		</section>
	`;
}

function normalizeTopicChildrenState(detail, parentSectionID) {
	const page = detail?.children_page || {};
	const sections = Array.isArray(page.sections) ? page.sections : (Array.isArray(detail?.children) ? detail.children : []);
	return {
		document_id: String(page.document_id || detail?.document?.id || detail?.section?.document_id || ""),
		parent_section_id: String(page.parent_section_id || parentSectionID || ""),
		sections: mergeUniqueTopicChildren([], sections),
		limit: Number(page.limit) > 0 ? Number(page.limit) : 100,
		offset: Number(page.offset) >= 0 ? Number(page.offset) : 0,
		has_more: page.has_more === true || page.hasMore === true,
	};
}

function mergeTopicChildrenPage(state, page) {
	if (!state) return normalizeTopicChildrenState({ children_page: page }, page?.parent_section_id || "");
	const documentID = String(page?.document_id || state.document_id || "");
	const parentSectionID = String(page?.parent_section_id || state.parent_section_id || "");
	if (documentID !== state.document_id || parentSectionID !== state.parent_section_id) return state;
	return {
		...state,
		sections: mergeUniqueTopicChildren(state.sections, Array.isArray(page?.sections) ? page.sections : []),
		limit: Number(page?.limit) > 0 ? Number(page.limit) : state.limit,
		offset: Number(page?.offset) >= 0 ? Number(page.offset) : state.offset,
		has_more: page?.has_more === true || page?.hasMore === true,
	};
}

function mergeUniqueTopicChildren(existing, incoming) {
	const merged = [];
	const seen = new Set();
	for (const child of [...(existing || []), ...(incoming || [])]) {
		const id = String(topicRecord(child).id || "");
		if (!id || seen.has(id)) continue;
		seen.add(id);
		merged.push(child);
	}
	return merged;
}

async function loadMoreTopicChildren(button) {
	const state = activeTopicChildrenState;
	if (!state?.has_more || !state.document_id || !state.parent_section_id) return;
	const generation = topicMediaGeneration;
	button.disabled = true;
	try {
		const params = new URLSearchParams({
			parent_section_id: state.parent_section_id,
			limit: String(state.limit || 100),
			offset: String(state.sections.length),
		});
		const page = await request(`/api/documents/${encodeURIComponent(state.document_id)}/outline?${params}`);
		if (generation !== topicMediaGeneration || activeTopicID !== state.parent_section_id || currentRoute() !== "topic") return;
		activeTopicChildrenState = mergeTopicChildrenPage(state, page);
		const section = button.closest("[data-topic-children-section]");
		if (section) section.outerHTML = renderTopicChildren(activeTopicChildrenState.sections, activeTopicChildrenState);
	} catch (error) {
		alert(error.message);
	} finally {
		button.disabled = false;
	}
}

function renderTopicMedia(assets) {
  if (!assets.length) return `<section class="topic-section"><h4>${escapeHTML(t("topic.media"))} (0)</h4><p class="muted">${escapeHTML(t("media.empty"))}</p></section>`;
  return `
    <section class="topic-section">
      <h4>${escapeHTML(t("topic.media"))} (${assets.length})</h4>
      <div class="topic-media-gallery">
        ${assets.map((asset) => {
          const id = mediaAssetID(asset);
          const status = mediaAssetStatus(asset);
          const name = mediaAssetName(asset);
          const metadata = mediaMetadata(asset);
          const diagnostic = asset.diagnostic || asset.reason || asset.original_uri || asset.uri || metadata.diagnostic || metadata.reason || metadata.original_uri || metadata.uri || "";
          const caption = asset.caption || asset.alt || metadata.caption || metadata.alt || "";
          const canLoad = mediaCanLoad(asset);
          return `
            <article class="topic-media-card ${escapeAttr(status)}">
              ${mediaIsPreviewableImage(asset) && canLoad ? `<div class="topic-image" data-topic-image-asset="${escapeAttr(id)}" data-media-alt="${escapeAttr(caption || name)}"><span>${escapeHTML(t("media.loading"))}</span></div>` : `<div class="topic-media-placeholder"><strong>${escapeHTML(mediaStatusLabel(asset))}</strong><span>${escapeHTML(asset.media_type || asset.kind || "")}</span></div>`}
              <div class="topic-media-info">
                <strong>${escapeHTML(name)}</strong>
                <small>${escapeHTML(mediaStatusLabel(asset))}${asset.size_bytes ? ` · ${escapeHTML(formatBytes(asset.size_bytes))}` : ""}</small>
                ${caption ? `<p>${escapeHTML(caption)}</p>` : ""}
                ${diagnostic ? `<p class="warn-text">${escapeHTML(diagnostic)}</p>` : ""}
                ${canLoad ? `<button type="button" data-download-media="${escapeAttr(id)}" data-media-filename="${escapeAttr(name)}">${escapeHTML(t(mediaIsPreviewableImage(asset) ? "media.open" : "media.download"))}</button>` : ""}
              </div>
            </article>
          `;
        }).join("")}
      </div>
    </section>
  `;
}

function renderTopicRelations(relations, currentSectionID) {
  return `
    <section class="topic-section">
      <h4>${escapeHTML(t("topic.authored_relations"))} (${relations.length})</h4>
      ${relations.length ? `<div class="topic-relation-list">${relations.map((relation) => {
        const edge = relation.edge || relation;
        const edgeMetadata = parsePayload(edge.metadata_json || edge.metadata || "{}");
        const nodeMetadata = parsePayload(relation.node?.metadata_json || "{}");
        const direction = relation.direction || edge.direction || "both";
        const targetID = relation.target_section_id || relation.other_section_id || nodeMetadata.section_id || (edge.src_section_id === currentSectionID ? edge.dst_section_id : edge.src_section_id) || "";
        const targetTitle = relation.target_title || relation.other_title || relation.node?.name || targetID;
        const arrows = [edge.start_arrow || edgeMetadata.start_arrow, edge.end_arrow || edgeMetadata.end_arrow].filter(Boolean).join(" / ");
        return `
          <article class="topic-relation-row">
            <div><strong>${escapeHTML(edge.label || edgeMetadata.label || edge.kind || "related_to")}</strong><small>${escapeHTML(direction)} · ${escapeHTML(edge.provenance || "source_authored")}${arrows ? ` · ${escapeHTML(arrows)}` : ""}</small></div>
            <span>${escapeHTML(targetTitle)}</span>
            ${targetID ? `<button type="button" data-open-topic="${escapeAttr(targetID)}">${escapeHTML(t("topic.open_related"))}</button>` : ""}
          </article>
        `;
      }).join("")}</div>` : `<p class="muted">${escapeHTML(t("topic.relations_empty"))}</p>`}
    </section>
  `;
}

function formatOrderPath(value) {
  if (Array.isArray(value)) return `[${value.join(", ")}]`;
  if (typeof value === "string") {
    const parsed = parsePayload(value);
    if (Array.isArray(parsed)) return `[${parsed.join(", ")}]`;
    return value;
  }
  return value == null ? "" : formatMetadataValue(value);
}

async function onTopicDetailClick(event) {
  const topicButton = event.target.closest("button[data-open-topic]");
	if (topicButton) {
    openTopicByID(topicButton.dataset.openTopic, previousTopicRoute);
    return;
	}
	const loadChildrenButton = event.target.closest("button[data-load-topic-children]");
	if (loadChildrenButton) {
		await loadMoreTopicChildren(loadChildrenButton);
		return;
	}
  const mediaButton = event.target.closest("button[data-download-media]");
  if (mediaButton) {
    mediaButton.disabled = true;
    try {
      await downloadAuthenticated(`/api/media-assets/${encodeURIComponent(mediaButton.dataset.downloadMedia)}/content`, mediaButton.dataset.mediaFilename || "attachment");
    } catch (error) {
      alert(error.message);
    } finally {
      mediaButton.disabled = false;
    }
    return;
  }
  const snapshotButton = event.target.closest("button[data-download-snapshot]");
  if (!snapshotButton) return;
  snapshotButton.disabled = true;
  try {
    await downloadAuthenticated(`/api/sources/${encodeURIComponent(snapshotButton.dataset.downloadSnapshot)}/snapshot`, "workbook.xmind");
  } catch (error) {
    alert(error.message);
  } finally {
    snapshotButton.disabled = false;
  }
}

async function loadDocumentDetail(documentID) {
  const container = document.querySelector("#document-detail");
  const subtitle = document.querySelector("#document-detail-subtitle");
  if (!container) return;
  documentID = String(documentID || "").trim();
  if (!documentID) {
    container.innerHTML = `<p class="muted">${escapeHTML(t("document.empty"))}</p>`;
    if (subtitle) subtitle.textContent = "";
    return;
  }
  container.innerHTML = `<p class="muted">${escapeHTML(t("document.loading"))}</p>`;
  try {
    const detail = await request(`/api/documents/${encodeURIComponent(documentID)}`);
    const doc = detail.document || {};
    const source = detail.source || {};
    if (subtitle) {
      subtitle.textContent = [source.name || source.id, doc.external_id || doc.url || doc.id].filter(Boolean).join(" · ");
    }
    container.innerHTML = renderDocumentDetail(detail);
  } catch (error) {
    container.innerHTML = "";
    if (subtitle) subtitle.textContent = "";
    alert(error.message);
  }
}

function renderDocumentDetail(detail) {
  const doc = detail.document || {};
  const source = detail.source || {};
  const profile = detail.profile || {};
  const sections = detail.sections || [];
  const entities = detail.entities || detail.section_entities || [];
  const feedback = detail.feedback || [];
  const related = detail.related || [];
  const diagnostics = documentDiagnosticsForRender(detail);
  const brokenLinks = detail.broken_links || [];
  const latestJob = detail.latest_job || {};
  const retrieval = parsePayload(profile.retrieval_profile_json);
  const tags = retrieval.top_tags || retrieval.tags || [];
  const staleEvents = feedback.filter((event) => event.feedback_kind === "document_stale");
  return `
    <div class="document-summary-grid">
      <div><span>${escapeHTML(t("document.field.title"))}</span><strong>${escapeHTML(doc.title || doc.external_id || doc.id)}</strong></div>
      <div><span>${escapeHTML(t("document.field.source"))}</span><strong>${escapeHTML(source.name || source.id || "")}</strong></div>
      <div><span>${escapeHTML(t("document.field.sections"))}</span><strong>${escapeHTML(doc.section_count ?? sections.length)}</strong></div>
      <div><span>${escapeHTML(t("health.section_entities"))}</span><strong>${escapeHTML(entities.length)}</strong></div>
      <div><span>${escapeHTML(t("document.field.indexed_at"))}</span><strong>${escapeHTML(formatJobTime(doc.indexed_at))}</strong></div>
    </div>
    <div class="document-actions">
      ${doc.url ? `<a class="secondary-link-button" href="${escapeAttr(doc.url)}" target="_blank" rel="noreferrer">${escapeHTML(t("document.open_source"))}</a>` : ""}
      ${doc.node_id ? `<button type="button" data-open-node="${escapeAttr(doc.node_id)}">${escapeHTML(t("node.open"))}</button>` : ""}
      ${staleEvents.length ? staleEvents.map((event) => `<button type="button" data-delete-feedback="${escapeAttr(event.id)}">${escapeHTML(t("feedback.unmark_stale"))}</button>`).join("") : ""}
    </div>
    ${renderDocumentDiagnostics(diagnostics, latestJob, brokenLinks)}
    ${renderDocumentGraph(detail)}
    <details class="document-section" open>
      <summary>${escapeHTML(t("document.metadata"))}</summary>
      <div class="document-meta-list">
        <small>${escapeHTML(t("document.field.id"))}: ${escapeHTML(doc.id)}</small>
        <small>${escapeHTML(t("document.field.external_id"))}: ${escapeHTML(doc.external_id || "")}</small>
        <small>${escapeHTML(t("document.field.url"))}: ${escapeHTML(doc.url || "")}</small>
        <small>${escapeHTML(t("document.field.content_hash"))}: ${escapeHTML(doc.content_hash || "")}</small>
        <small>${escapeHTML(t("document.field.source_kind"))}: ${escapeHTML(source.kind || "")}</small>
        <small>${escapeHTML(t("document.field.source_dsn"))}: ${escapeHTML(source.dsn || "")}</small>
      </div>
    </details>
    <details class="document-section" open>
      <summary>${escapeHTML(t("document.profile"))}</summary>
      ${profile.desc ? `<p>${escapeHTML(profile.desc)}</p>` : `<p class="muted">${escapeHTML(t("document.profile.empty_desc"))}</p>`}
      ${tags.length ? `<div class="result-tags">${tags.slice(0, 12).map((tag) => `<span>${escapeHTML(tag)}</span>`).join("")}</div>` : ""}
      <div class="document-meta-list">
        <small>${escapeHTML(t("document.field.generated_from_hash"))}: ${escapeHTML(profile.generated_from_hash || "")}</small>
        <small>${escapeHTML(t("document.field.generated_at"))}: ${escapeHTML(formatJobTime(profile.generated_at))}</small>
      </div>
    </details>
    <details class="document-section" open>
      <summary>${escapeHTML(t("document.section_entities"))} (${entities.length})</summary>
      <div class="document-section-list">
        ${entities.length ? entities.map(renderSectionEntityRow).join("") : `<p class="muted">${escapeHTML(t("document.section_entities.empty"))}</p>`}
      </div>
    </details>
    <details class="document-section" open>
      <summary>${escapeHTML(t("document.sections"))} (${sections.length})</summary>
      <div class="document-section-list">
        ${sections.length ? sections.map(renderDocumentSection).join("") : `<p class="muted">${escapeHTML(t("document.sections.empty"))}</p>`}
      </div>
    </details>
    <details class="document-section" open>
      <summary>${escapeHTML(t("document.related"))} (${related.length})</summary>
      <div class="document-section-list">
        ${related.length ? related.map(renderDocumentRelated).join("") : `<p class="muted">${escapeHTML(t("node.related.empty"))}</p>`}
      </div>
    </details>
    <details class="document-section" open>
      <summary>${escapeHTML(t("document.feedback"))} (${feedback.length})</summary>
      <div class="document-section-list">
        ${feedback.length ? feedback.map(renderDocumentFeedback).join("") : `<p class="muted">${escapeHTML(t("document.feedback.empty"))}</p>`}
      </div>
    </details>
  `;
}

function documentDiagnosticsForRender(detail) {
  const doc = detail.document || {};
  const profile = detail.profile || {};
  const sections = detail.sections || [];
  const related = detail.related || [];
  const feedback = detail.feedback || [];
  const diagnostics = [...(detail.diagnostics || [])];
  if (!profile.generated_from_hash) {
    diagnostics.push({
      kind: "missing_retrieval_profile",
      severity: "warn",
      message: t("document.diagnostic.missing_retrieval_profile"),
      document_id: doc.id,
    });
  } else if (doc.content_hash && profile.generated_from_hash !== doc.content_hash) {
    diagnostics.push({
      kind: "profile_hash_mismatch",
      severity: "warn",
      message: t("document.diagnostic.profile_hash_mismatch"),
      document_id: doc.id,
    });
  }
  if (doc.node_id && !related.length) {
    diagnostics.push({
      kind: "no_related_graph",
      severity: "info",
      message: t("document.diagnostic.no_related_graph"),
      document_id: doc.id,
    });
  }
  if (!sections.length) {
    diagnostics.push({
      kind: "zero_sections",
      severity: "warn",
      message: t("document.diagnostic.zero_sections"),
      document_id: doc.id,
    });
  }
  if (feedback.some((event) => event.feedback_kind === "document_stale") && !diagnostics.some((item) => item.kind === "document_stale")) {
    diagnostics.push({
      kind: "document_stale",
      severity: "info",
      message: t("document.diagnostic.document_stale"),
      document_id: doc.id,
    });
  }
  return diagnostics;
}

function renderDocumentDiagnostics(diagnostics, latestJob, brokenLinks) {
  return `
    <details class="document-section document-diagnostics" open>
      <summary>${escapeHTML(t("document.diagnostics"))} (${diagnostics.length})</summary>
      <div class="document-meta-list">
        <small>${escapeHTML(t("health.latest_sync"))}: ${escapeHTML(latestJob?.id ? `${formatJobStatus(latestJob.status)} · ${formatJobTime(latestJob.updated_at || latestJob.created_at)}` : t("health.no_sync"))}</small>
        <small>${escapeHTML(t("health.broken_links"))}: ${escapeHTML(brokenLinks.length || 0)}</small>
      </div>
      ${diagnostics.length ? `<div class="health-warning-list">${diagnostics.map(renderHealthWarning).join("")}</div>` : `<p class="muted">${escapeHTML(t("health.no_warnings"))}</p>`}
      ${renderHealthBrokenLinks(brokenLinks)}
    </details>
  `;
}

function renderDocumentGraph(detail) {
  const graph = buildDocumentGraph(detail);
  if (graph.nodes.length <= 1) {
    return "";
  }
  const height = Math.max(260, 90 + Math.max(graph.leftCount, graph.rightCount) * 54);
  const nodeByID = new Map(graph.nodes.map((node) => [node.id, node]));
  return `
    <details class="document-section document-graph-section" open>
      <summary>${escapeHTML(t("document.graph"))} (${graph.nodes.length - 1})</summary>
      <div class="document-graph" style="--graph-height:${height}px">
        <svg viewBox="0 0 720 ${height}" role="img" aria-label="${escapeAttr(t("document.graph"))}">
          ${graph.edges.map((edge) => {
            const src = nodeByID.get(edge.src_id);
            const dst = nodeByID.get(edge.dst_id);
            if (!src || !dst) return "";
            return `<path class="graph-edge" d="M ${src.x} ${src.y} C ${(src.x + dst.x) / 2} ${src.y}, ${(src.x + dst.x) / 2} ${dst.y}, ${dst.x} ${dst.y}" /><text class="graph-edge-label" x="${(src.x + dst.x) / 2}" y="${(src.y + dst.y) / 2 - 4}">${escapeHTML(edge.kind || "")}</text>`;
          }).join("")}
          ${graph.nodes.map((node) => `
            <g class="graph-node ${escapeAttr(node.kind || "")}" transform="translate(${node.x - 74} ${node.y - 20})">
              <rect width="148" height="40" rx="6"></rect>
              <text x="74" y="16">${escapeHTML(truncateLabel(node.label || node.id, 22))}</text>
              <text x="74" y="30" class="graph-node-kind">${escapeHTML(node.kind || "")}</text>
            </g>
          `).join("")}
        </svg>
      </div>
      <div class="document-graph-actions">
        ${graph.nodes.filter((node) => node.id !== graph.root.id).slice(0, 12).map((node) => `<button type="button" data-open-node="${escapeAttr(node.id)}">${escapeHTML(truncateLabel(node.label || node.id, 28))}</button>`).join("")}
      </div>
    </details>
  `;
}

function buildDocumentGraph(detail) {
  const doc = detail.document || {};
  const sections = (detail.sections || []).slice(0, 10);
  const related = (detail.related || []).slice(0, 12);
  const heightBase = 90;
  const rowGap = 54;
  const root = {
    id: doc.node_id || doc.id || "document",
    kind: "Document",
    label: doc.title || doc.external_id || doc.id || "Document",
    x: 360,
    y: 70,
  };
  const nodes = [root];
  const edges = [];
  sections.forEach((section, index) => {
    if (!section.node_id) return;
    const node = {
      id: section.node_id,
      kind: "DocSection",
      label: section.title || section.heading_path || section.id,
      x: 150,
      y: heightBase + index * rowGap,
    };
    nodes.push(node);
    edges.push({ id: `${root.id}:${node.id}`, src_id: root.id, dst_id: node.id, kind: "contains" });
  });
  const seen = new Set(nodes.map((node) => node.id));
  related.forEach((entry, index) => {
    const relatedNode = entry.node || {};
    if (!relatedNode.id || seen.has(relatedNode.id)) return;
    seen.add(relatedNode.id);
    nodes.push({
      id: relatedNode.id,
      kind: relatedNode.kind || "",
      label: relatedNode.name || relatedNode.canonical_name || relatedNode.id,
      x: 570,
      y: heightBase + index * rowGap,
    });
    const edge = entry.edge || {};
    edges.push({
      id: edge.id || `${root.id}:${relatedNode.id}`,
      src_id: entry.direction === "in" ? relatedNode.id : root.id,
      dst_id: entry.direction === "in" ? root.id : relatedNode.id,
      kind: edge.kind || "",
    });
  });
  return { root, nodes, edges, leftCount: sections.length, rightCount: related.length };
}

function truncateLabel(value, max) {
  value = String(value || "");
  return value.length > max ? `${value.slice(0, Math.max(0, max - 3))}...` : value;
}

function renderDocumentSection(section) {
  return `
    <div class="artifact-row">
      <strong>${escapeHTML(section.title || section.heading_path || section.id)}</strong>
      <small>${escapeHTML(section.heading_path || "")} · ${escapeHTML(t("node.id"))}: ${escapeHTML(section.node_id || "")}</small>
      <p>${escapeHTML(section.content_snippet || "")}</p>
      ${section.node_id ? `<button type="button" data-open-node="${escapeAttr(section.node_id)}">${escapeHTML(t("node.open"))}</button>` : ""}
    </div>
  `;
}

function renderDocumentRelated(entry) {
  return `
    <div class="artifact-row">
      <strong>${escapeHTML(entry.node?.name || entry.node?.id || "")}</strong>
      <small>${escapeHTML(entry.direction || "")} · ${escapeHTML(entry.edge?.kind || "")} · ${escapeHTML(entry.node?.kind || "")}</small>
      <small>${escapeHTML(entry.edge?.id || "")} · ${escapeHTML(entry.edge?.provenance || "")} ${escapeHTML(entry.edge?.evidence_section_id || "")}</small>
      ${entry.node?.id ? `<button type="button" data-open-node="${escapeAttr(entry.node.id)}">${escapeHTML(t("node.open"))}</button>` : ""}
    </div>
  `;
}

function renderDocumentFeedback(event) {
  return `
    <div class="artifact-row">
      <strong>${escapeHTML(humanizeToken(event.feedback_kind))}</strong>
      <small>${escapeHTML(event.id)} · ${escapeHTML(formatJobTime(event.created_at))}${event.actor ? ` · ${escapeHTML(event.actor)}` : ""}</small>
      ${event.feedback_kind === "document_stale" ? `<button type="button" data-delete-feedback="${escapeAttr(event.id)}">${escapeHTML(t("feedback.unmark_stale"))}</button>` : ""}
    </div>
  `;
}

async function loadStaleDocuments() {
  const container = document.querySelector("#stale-documents");
  if (!container) return;
  container.innerHTML = `<p class="muted" style="padding:12px 0;">${escapeHTML(t("governance.stale_loading"))}</p>`;
  const body = await request("/api/feedback?target_kind=document&feedback_kind=document_stale&limit=100");
  const events = body.feedback || [];
  const badge = document.querySelector("#stale-count-badge");
  if (badge) {
    badge.textContent = events.length;
    badge.hidden = events.length === 0;
  }
  if (!events.length) {
    container.innerHTML = `<div class="empty-state"><p class="empty-icon">✓</p><p>${escapeHTML(t("governance.stale_empty"))}</p></div>`;
    return;
  }
  const rows = await Promise.all(events.map(async (event) => {
    try {
      const detail = await request(`/api/documents/${encodeURIComponent(event.target_id)}?summary=1`);
      return { event, detail };
    } catch (error) {
      return { event, error };
    }
  }));
  container.innerHTML = rows.map(renderStaleDocumentRow).join("");
}

function renderStaleDocumentRow(row) {
  const event = row.event || {};
  const doc = row.detail?.document || {};
  const source = row.detail?.source || {};
  const missing = !row.detail;
  return `
    <div class="stale-document">
      <div class="stale-document-main">
        <strong>${escapeHTML(doc.title || doc.external_id || event.target_id)}</strong>
        <small>${escapeHTML(source.name || source.id || t("document.missing"))}<span class="doc-id"> · ${escapeHTML(event.target_id || "")}</span></small>
        <small>${escapeHTML(formatJobTime(event.created_at))}${event.actor ? ` · ${escapeHTML(event.actor)}` : ""}</small>
        ${missing ? `<p>${escapeHTML(t("governance.stale_missing_document"))}</p>` : ""}
      </div>
      <div class="result-actions">
        ${doc.id ? `<button type="button" data-open-document="${escapeAttr(doc.id)}">${escapeHTML(t("document.open"))}</button>` : ""}
        ${doc.url ? `<a class="secondary-link-button" href="${escapeAttr(doc.url)}" target="_blank" rel="noreferrer">${escapeHTML(t("document.open_source"))}</a>` : ""}
        <button type="button" class="secondary-button" data-delete-feedback="${escapeAttr(event.id)}">${escapeHTML(t("feedback.unmark_stale"))}</button>
      </div>
    </div>
  `;
}

async function onStaleDocumentsClick(event) {
  const open = event.target.closest("button[data-open-document]");
  if (open) {
    openDocumentByID(open.dataset.openDocument);
    return;
  }
  const del = event.target.closest("button[data-delete-feedback]");
  if (!del) return;
  del.disabled = true;
  try {
    await deleteFeedback(del.dataset.deleteFeedback);
    await loadStaleDocuments();
    if (currentRoute() === "document" && activeDocumentID) {
      await loadDocumentDetail(activeDocumentID);
    }
  } catch (error) {
    del.disabled = false;
    alert(error.message);
  }
}

async function onDocumentDetailClick(event) {
  const node = event.target.closest("button[data-open-node]");
  if (node) {
    openNodeByID(node.dataset.openNode);
    return;
  }
  const del = event.target.closest("button[data-delete-feedback]");
  if (!del) return;
  del.disabled = true;
  try {
    await deleteFeedback(del.dataset.deleteFeedback);
    await loadDocumentDetail(activeDocumentID);
  } catch (error) {
    del.disabled = false;
    alert(error.message);
  }
}

async function onProposalStatusTabClick(event) {
  const tab = event.target.closest(".pill-tab");
  if (!tab) return;
  document.querySelectorAll("#proposal-status-tabs .pill-tab").forEach(t => t.classList.remove("active"));
  tab.classList.add("active");
  activeProposalStatus = tab.dataset.proposalStatus;
  await loadKnowledgeRelationProposals();
}

async function onKnowledgeRelationProposalFiltersSubmit(event) {
  event.preventDefault();
  await loadKnowledgeRelationProposals();
}

async function loadKnowledgeRelationProposals() {
  const container = document.querySelector("#relation-proposals");
  if (!container) return;
  container.innerHTML = `<p class="muted" style="padding:12px 0;">${escapeHTML(t("governance.loading"))}</p>`;
  const docFilter = document.querySelector("#proposal-doc-filter");
  const params = new URLSearchParams();
  params.set("status", activeProposalStatus || "pending");
  const documentId = (docFilter?.value || "").trim();
  if (documentId) params.set("document_id", documentId);
  params.set("limit", "100");
  try {
    const body = await request(`/api/knowledge-relation-proposals?${params.toString()}`);
    const proposals = body.proposals || [];
    if (!proposals.length) {
      container.innerHTML = `<div class="empty-state"><p class="empty-icon">∅</p><p>${escapeHTML(t("governance.empty"))}</p></div>`;
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
    <div class="relation-proposal ${escapeAttr(proposal.status)}" data-proposal-id="${escapeAttr(proposal.id)}">
      <div class="relation-proposal-main">
        <strong>
          <span class="status-badge ${escapeAttr(proposal.status)}">${escapeHTML(t(`governance.status.${proposal.status}`))}</span>
          ${escapeHTML(proposal.relation_type)}
        </strong>
        <span class="proposal-id-path">${escapeHTML(proposal.from_document_id)}  →  ${escapeHTML(proposal.to_document_id)}</span>
      </div>
      <div class="proposal-meta-pills">
        <span>${escapeHTML(proposal.proposed_effect || "context_link")}</span>
        <span>${escapeHTML(proposal.direction || "directed")}</span>
        ${proposal.created_by_type ? `<span>${escapeHTML(proposal.created_by_type)}${proposal.created_by_ref ? `:${escapeHTML(proposal.created_by_ref)}` : ""}</span>` : ""}
      </div>
      <p>${escapeHTML(proposal.reason || "")}</p>
      ${proposal.from_anchor || proposal.to_anchor ? `<small style="color:var(--text-muted);font-family:var(--font-mono);font-size:11px;">${escapeHTML([proposal.from_anchor, proposal.to_anchor].filter(Boolean).join(" → "))}</small>` : ""}
      ${evidence ? `<pre class="relation-evidence">${escapeHTML(evidence)}</pre>` : ""}
      ${proposal.review_note ? `<div class="proposal-review-note">${escapeHTML(t("governance.review_note"))}: ${escapeHTML(proposal.review_note)}</div>` : ""}
      ${pending ? `
        <div class="proposal-actions">
          <button type="button" class="approve-button" data-proposal-approve>${escapeHTML(t("governance.approve"))}</button>
          <button type="button" class="reject-button" data-proposal-reject>${escapeHTML(t("governance.reject"))}</button>
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

async function deleteFeedback(id) {
  return request(`/api/feedback/${encodeURIComponent(id)}`, {
    method: "DELETE",
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

globalThis.DocGraphWebModel = Object.freeze({
  apiFetch,
  buildSourceConfig,
  fetchAuthenticatedBlob,
  formatOrderPath,
	mediaAssetStatus,
	mediaIsPreviewableImage,
	mediaIsSearchPreviewable,
	mergeTopicChildrenPage,
	normalizeTopicChildrenState,
  normalizeFeatureInventory,
  responseFilename,
  revokeObjectURLs,
  topicRecord,
});

if (!globalThis.__DOCGRAPH_WEB_TEST__) {
  init();
}
