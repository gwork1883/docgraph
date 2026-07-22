import assert from "node:assert/strict";
import test from "node:test";

globalThis.__DOCGRAPH_WEB_TEST__ = true;
globalThis.window = globalThis;
globalThis.window.alert = () => {};
globalThis.document = {
  currentScript: null,
  baseURI: "http://localhost/",
};
const sessionValues = new Map();
globalThis.sessionStorage = {
  getItem: (key) => sessionValues.get(key) || null,
  setItem: (key, value) => sessionValues.set(key, String(value)),
  removeItem: (key) => sessionValues.delete(key),
};

await import("./main.js");

const model = globalThis.DocGraphWebModel;

test("embedding status prefers reconciled ready and expected counts with legacy fallbacks", () => {
  assert.equal(model.embeddingReadySections({ ready_sections: 3, embedded_sections: 8 }), 3);
  assert.equal(model.embeddingReadySections({ ready_sections: 0, embedded_sections: 8 }), 0);
  assert.equal(model.embeddingReadySections({ embedded_sections: 8 }), 8);
  assert.equal(model.embeddingExpectedChunks({ expected_chunks: 5, total_chunks: 9 }), 5);
  assert.equal(model.embeddingExpectedChunks({ total_chunks: 9 }), 9);
  assert.equal(model.embeddingReadyChunks({ ready_chunks: 4, embedded_chunks: 7 }), 4);
  assert.equal(model.embeddingReadyChunks({ embedded_chunks: 7 }), 7);
});

test("embedding status surfaces orphan cleanup instead of reporting ready", () => {
  const status = {
    enabled: true,
    status: "ready",
    total_sections: 3,
    ready_sections: 3,
    embedded_sections: 5,
    orphan_sections: 2,
    orphan_chunks: 4,
  };
  assert.equal(model.embeddingCleanupRequired(status), true);
  assert.equal(model.embeddingStateLabel("source-a", status), "source.vector_cleanup_required");
  assert.match(model.formatEmbeddingStatus("source-a", status), /3\/3/);
  assert.match(model.formatEmbeddingStatus("source-a", status), /2 source\.vector_orphan/);
});

test("XMind source config remains empty", () => {
  const form = { get: () => null };
  assert.deepEqual(model.buildSourceConfig(form, "xmind"), {});
});

test("feature inventory accepts storage coverage fields", () => {
  assert.deepEqual(model.normalizeFeatureInventory([{
    feature_key: "zone",
    coverage_status: "preserved",
    element_path: "content.json/sheets/0/zones/0",
    count: 3,
  }]), [{
    feature: "zone",
    state: "preserved",
    detail: "content.json/sheets/0/zones/0",
    count: 3,
  }]);
});

test("feature inventory exposes aggregated reason and bounded samples", () => {
  const [entry] = model.normalizeFeatureInventory([{
    feature_key: "relationship_geometry",
    coverage_status: "unsupported",
    element_path: "/0/relationships/2/controlPoints/0",
    count: 25,
    metadata_json: JSON.stringify({
      reason_code: "invalid_control_point_number",
      sample_paths: ["/0/relationships/2/controlPoints/0", "/0/relationships/9/controlPoints/1"],
      details_truncated: true,
    }),
  }]);
  assert.equal(entry.count, 25);
  assert.equal(entry.state, "unsupported");
  assert.match(entry.detail, /invalid_control_point_number/);
  assert.match(entry.detail, /relationships\/9/);
});

test("only safe raster media is previewed", () => {
  assert.equal(model.mediaIsPreviewableImage({ media_type: "image/png" }), true);
  assert.equal(model.mediaIsPreviewableImage({ media_type: "image/jpeg; charset=binary" }), true);
  assert.equal(model.mediaIsPreviewableImage({ media_type: "image/svg+xml" }), false);
  assert.equal(model.mediaIsPreviewableImage({ media_type: "text/html" }), false);
});

test("search previews reject unknown or large original images without requiring derived thumbnails", () => {
	assert.equal(model.mediaIsSearchPreviewable({ media_type: "image/png", size_bytes: 1024 }), true);
	assert.equal(model.mediaIsSearchPreviewable({ media_type: "image/png", size_bytes: 0 }), false);
	assert.equal(model.mediaIsSearchPreviewable({ media_type: "image/png", size_bytes: 3 * 1024 * 1024 }), false);
	assert.equal(model.mediaIsSearchPreviewable({ media_type: "image/svg+xml", size_bytes: 1024 }), false);
});

test("section context and order path normalize for Topic routing", () => {
  assert.equal(model.topicRecord({ section_id: "sec-deep", title: "Deep" }).id, "sec-deep");
  assert.equal(model.formatOrderPath([0, 2, 1]), "[0, 2, 1]");
});

test("Topic child pages preserve has_more and merge without omission or duplicates", () => {
	const first = model.normalizeTopicChildrenState({
		document: { id: "sheet-large" },
		children_page: {
			document_id: "sheet-large",
			parent_section_id: "topic-root",
			sections: [{ id: "child-001" }, { id: "child-002" }],
			limit: 2,
			offset: 0,
			has_more: true,
		},
	}, "topic-root");
	assert.equal(first.has_more, true);
	assert.deepEqual(first.sections.map((item) => item.id), ["child-001", "child-002"]);
	const complete = model.mergeTopicChildrenPage(first, {
		document_id: "sheet-large",
		parent_section_id: "topic-root",
		sections: [{ id: "child-002" }, { id: "child-003" }],
		limit: 2,
		offset: 2,
		has_more: false,
	});
	assert.equal(complete.has_more, false);
	assert.deepEqual(complete.sections.map((item) => item.id), ["child-001", "child-002", "child-003"]);
});

test("Topic child pages from a stale parent cannot enter the active Topic", () => {
	const active = model.normalizeTopicChildrenState({
		document: { id: "sheet-large" },
		children: [{ id: "active-child" }],
	}, "active-topic");
	const merged = model.mergeTopicChildrenPage(active, {
		document_id: "sheet-large",
		parent_section_id: "stale-topic",
		sections: [{ id: "stale-child" }],
		has_more: false,
	});
	assert.equal(merged, active);
	assert.deepEqual(merged.sections.map((item) => item.id), ["active-child"]);
});

test("authenticated download filename supports UTF-8 disposition", () => {
  assert.equal(model.responseFilename("attachment; filename*=UTF-8''map%20book.xmind"), "map book.xmind");
  assert.equal(model.responseFilename('attachment; filename="diagram.png"'), "diagram.png");
});

test("object URL owner cleanup revokes every URL", () => {
  const revoked = [];
  const original = URL.revokeObjectURL;
  URL.revokeObjectURL = (value) => revoked.push(value);
  try {
    const urls = new Set(["blob:first", "blob:second"]);
    model.revokeObjectURLs(urls);
    assert.deepEqual(revoked, ["blob:first", "blob:second"]);
    assert.equal(urls.size, 0);
  } finally {
    URL.revokeObjectURL = original;
  }
});

test("authenticated media fetch uses header token without query credentials", async () => {
  sessionStorage.setItem("docgraph.auth.token", "fixture-token");
  const originalFetch = globalThis.fetch;
  let captured;
  globalThis.fetch = async (path, options) => {
    captured = { path, options };
    return new Response(new Blob(["image-bytes"], { type: "image/png" }), {
      status: 200,
      headers: { "Content-Disposition": 'inline; filename="evidence.png"' },
    });
  };
  try {
    const result = await model.fetchAuthenticatedBlob("/api/media-assets/opaque-id/content");
    assert.equal(captured.path, "/api/media-assets/opaque-id/content");
    assert.equal(new Headers(captured.options.headers).get("X-DocGraph-Token"), "fixture-token");
    assert.equal(String(captured.path).includes("fixture-token"), false);
    assert.equal(result.filename, "evidence.png");
    assert.equal(await result.blob.text(), "image-bytes");
  } finally {
    globalThis.fetch = originalFetch;
    sessionStorage.removeItem("docgraph.auth.token");
  }
});
