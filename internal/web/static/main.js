let messages = {};
let currentLang = "en";
const syncingSourceIDs = new Set();
const syncedSourceIDs = new Set();
const sourcesByID = new Map();
const sourceArtifactPages = new Map();
const ARTIFACT_PAGE_SIZE = 50;

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

async function init() {
  await loadI18n(detectLanguage());
  bindEvents();
  syncRouteFromHash();
  updateSourceFormForKind();
  await loadStatus();
  await loadSources();
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
    const response = await fetch(`/i18n/${currentLang}.json`);
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
    await loadStatus();
    await loadSources();
  });
  document.querySelectorAll("[data-route]").forEach((button) => {
    button.addEventListener("click", () => navigate(button.dataset.route));
  });
  window.addEventListener("hashchange", syncRouteFromHash);

  document.querySelector("#source-add-button")?.addEventListener("click", () => openSourceDialog());
  document.querySelector("[data-close-source-dialog]")?.addEventListener("click", closeSourceDialog);
  document.querySelector('#source-form select[name="kind"]').addEventListener("change", updateSourceFormForKind);

  document.querySelector("#source-form").addEventListener("submit", onSourceSubmit);
  document.querySelector("#sources").addEventListener("click", onSourcesClick);
  document.querySelector("#search-form").addEventListener("submit", onSearchSubmit);
  document.querySelector("#results").addEventListener("click", onSearchFeedbackClick);
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
  const knownRoutes = new Set(["dashboard", "connectors", "search", "nodes"]);
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

function resetSourceForm(form) {
  form.reset();
  form.elements.source_id.value = "";
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

async function loadStatus() {
  const health = document.querySelector("#health");
  try {
    const response = await fetch("/api/status");
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
  } catch (error) {
    health.textContent = t("health.error");
    health.className = "status warn";
  }
}

async function request(path, options = {}) {
  const response = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    if (response.status === 409) {
      throw new Error(t("source.sync_in_progress"));
    }
    throw new Error(body.error?.message || t("error.request_failed", { status: response.status }));
  }
  return body;
}

async function loadSources() {
  const container = document.querySelector("#sources");
  const body = await request("/api/sources");
  container.innerHTML = "";
  sourcesByID.clear();
  if (!body.sources?.length) {
    container.innerHTML = `<p class="muted">${escapeHTML(t("sources.empty"))}</p>`;
    return;
  }
  const sources = [...body.sources].sort((a, b) => String(b.created_at || "").localeCompare(String(a.created_at || "")) || String(b.id || "").localeCompare(String(a.id || "")));
  sources.forEach((source) => {
    sourcesByID.set(source.id, source);
    const syncing = syncingSourceIDs.has(source.id);
    const synced = syncedSourceIDs.has(source.id);
    const item = document.createElement("div");
    item.className = "item";
    item.innerHTML = `
      <strong>${escapeHTML(source.name)}</strong>
      <small>${escapeHTML(source.kind)} · ${escapeHTML(source.id)}</small>
      ${source.created_at ? `<small>${escapeHTML(formatJobTime(source.created_at))}</small>` : ""}
      <small>${escapeHTML(source.dsn)}</small>
      <div class="source-actions">
        <button data-edit="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.edit"))}</button>
        <button data-sync="${escapeAttr(source.id)}" type="button" ${syncing || synced ? "disabled" : ""}>${escapeHTML(t(syncing ? "source.syncing" : synced ? "source.synced" : "source.sync"))}</button>
        <button data-jobs="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.jobs"))}</button>
        <button data-artifacts="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.artifacts"))}</button>
        <button data-delete="${escapeAttr(source.id)}" type="button">${escapeHTML(t("source.delete"))}</button>
      </div>
      <div class="source-jobs" data-jobs-list="${escapeAttr(source.id)}"></div>
      <div class="source-artifacts" data-artifacts-list="${escapeAttr(source.id)}"></div>
    `;
    container.appendChild(item);
  });
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

function buildSourcePayload(form) {
  const kind = String(form.get("kind") || "local").trim();
  return {
    kind,
    name: String(form.get("name") || "").trim(),
    dsn: String(form.get("dsn") || "").trim(),
    product_hint: String(form.get("product_hint") || "").trim(),
    module_hint: String(form.get("module_hint") || "").trim(),
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

function setFormValue(form, name, value) {
  if (!form.elements[name]) return;
  form.elements[name].value = value == null ? "" : String(value);
}

async function onSourcesClick(event) {
  const button = event.target.closest("button[data-edit], button[data-sync], button[data-jobs], button[data-delete-job], button[data-artifacts], button[data-artifact-page], button[data-delete], button[data-open-node], button[data-save-desc]");
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

  if (button.dataset.jobs) {
    button.disabled = true;
    try {
      await loadSourceJobs(button.dataset.jobs);
    } catch (error) {
      alert(error.message);
    } finally {
      button.disabled = false;
    }
    return;
  }

  if (button.dataset.deleteJob) {
    button.disabled = true;
    const sourceID = button.dataset.sourceId || "";
    try {
      await request(`/api/sources/${encodeURIComponent(sourceID)}/jobs/${encodeURIComponent(button.dataset.deleteJob)}`, { method: "DELETE" });
      await loadSourceJobs(sourceID);
      await loadStatus();
    } catch (error) {
      alert(error.message);
    } finally {
      button.disabled = false;
    }
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
  if (!sourceID || syncingSourceIDs.has(sourceID) || syncedSourceIDs.has(sourceID)) {
    alert(t("source.sync_in_progress"));
    return;
  }
  syncingSourceIDs.add(sourceID);
  button.disabled = true;
  button.textContent = t("source.syncing");
  try {
    await request(`/api/sources/${sourceID}/sync`, { method: "POST", body: "{}" });
    syncedSourceIDs.add(sourceID);
    button.textContent = t("source.synced");
    await loadStatus();
  } catch (error) {
    syncingSourceIDs.delete(sourceID);
    button.disabled = false;
    button.textContent = t("source.sync");
    alert(error.message);
  }
}

async function loadSourceJobs(sourceID) {
  const body = await request(`/api/sources/${encodeURIComponent(sourceID)}/jobs?limit=10`);
  const container = document.querySelector(`[data-jobs-list="${CSS.escape(sourceID)}"]`);
  if (!container) return;
  container.innerHTML = renderSourceJobs(sourceID, body.jobs || []);
}

function renderSourceJobs(sourceID, jobs) {
  if (!jobs.length) {
    return `<p class="muted">${escapeHTML(t("jobs.empty"))}</p>`;
  }
  return `
    <div class="job-list">
      ${jobs.map((job) => renderSourceJob(sourceID, job)).join("")}
    </div>
  `;
}

function renderSourceJob(sourceID, job) {
  const payload = parsePayload(job.payload_json);
  const brokenLinks = Array.isArray(payload.broken_links) ? payload.broken_links : [];
  const error = String(job.last_error || "").trim();
  return `
    <div class="job-row">
      <div class="job-main">
        <strong>${escapeHTML(job.status || "")}</strong>
        <small>${escapeHTML(job.id || "")} · ${escapeHTML(formatJobTime(job.updated_at || job.created_at))}</small>
      </div>
      <div class="job-meta">
        <span>${escapeHTML(t("jobs.documents"))}: ${escapeHTML(payload.documents ?? 0)}</span>
        <span>${escapeHTML(t("jobs.broken_links"))}: ${escapeHTML(brokenLinks.length)}</span>
      </div>
      ${error ? `<p class="job-error">${escapeHTML(error)}</p>` : ""}
      ${brokenLinks.length ? renderBrokenLinks(brokenLinks) : ""}
      <button type="button" data-source-id="${escapeAttr(sourceID)}" data-delete-job="${escapeAttr(job.id || "")}">${escapeHTML(t("jobs.delete"))}</button>
    </div>
  `;
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
      item.innerHTML = `
        <a class="result-title" href="${escapeAttr(hit.document_url || "#")}" ${hit.document_url ? 'target="_blank" rel="noreferrer"' : ""}>${escapeHTML(title)}</a>
        <div class="result-url">${escapeHTML(url)}</div>
        <div class="result-path">${escapeHTML(path || hit.document_id)}</div>
        ${hit.desc ? `<p class="result-desc">${escapeHTML(hit.desc)}</p>` : ""}
        ${tags.length ? `<div class="result-tags">${tags.slice(0, 6).map((tag) => `<span>${escapeHTML(tag)}</span>`).join("")}</div>` : ""}
        ${matchedFields.length ? `<small class="result-match">${escapeHTML(t("search.matched_fields"))}: ${escapeHTML(matchedFields.join(", "))}</small>` : ""}
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
