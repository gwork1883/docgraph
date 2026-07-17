# DocGraph XMind Connector Design

> Status: Proposed / Not implemented
>
> Updated: 2026-07-10
>
> Scope: local, unencrypted `.xmind` files

## 1. Purpose

This document defines the XMind connector as a source-faithful knowledge
ingestion path, not as a text conversion utility.

The connector must preserve and expose:

- workbook and Sheet identity;
- deep Topic hierarchy and sibling order;
- XMind-generated numbering and manually typed numbering;
- Notes, Labels, Markers, links, Tasks, and other textual attributes;
- Topic images and attachments as viewable source evidence;
- author-created Relationships and Topic links;
- Summary, Boundary, Callout, Floating Topic, and other XMind-specific
  structures;
- the original `.xmind` package for source-level traceability;
- a visible inventory of features that were imported, preserved only, rejected,
  or not understood.

The implementation is complete only when a search hit can be traced back to the
exact Topic, its ancestors, ordered children, media, authored relationships,
Sheet, and original XMind package.

## 2. Non-Negotiable Design Principles

### 2.1 XMind Is Not A Flat Text Format

`HeadingPath + Content` is a useful search projection, but it is not the source
model. Topic identity, parentage, sibling order, numbering, media references,
and authored relationships must be stored explicitly.

### 2.2 Preserve First, Interpret Second

Source data that DocGraph cannot yet interpret must still be preserved and made
visible to the user.

The connector must never silently discard an unknown element. Each element must
end in one of these states:

- `indexed`: represented structurally and searchable;
- `preserved`: retained and viewable, but not semantically interpreted;
- `rejected`: retained as a logical placeholder with an explicit safety or
  validation reason;
- `unsupported`: present in the source snapshot and reported in feature
  coverage diagnostics.

### 2.3 Source Evidence Must Survive Normalization

The currently indexed workbook revision must have a content-addressed snapshot.
Images and attachments must be copied into the central DocGraph asset store.
The indexed state must not depend on the original source path continuing to
exist or continuing to contain the same bytes.

### 2.4 Stable Identity Must Not Depend On Presentation

Topic identity must be derived from XMind component IDs. It must not include
title, content hash, ordinal, numbering, or parent identity.

Renaming, reordering, or moving a Topic within the workbook must not produce a
new Section or Node ID.

### 2.5 Completeness Before Silent Limits

Deep trees and large Sheets must not be silently truncated. Traversal should be
iterative rather than recursively relying on the Go stack. A safety limit may
fail the sync with a precise diagnostic; it must not import only the first N
levels or first N Topics.

### 2.6 Authored Facts Are Not Inferred Facts

An XMind Relationship is a source-authored graph fact. It must remain distinct
from rule-extracted edges and governance-approved `knowledge_relations`.

### 2.7 Media Understanding Is A Separate Capability

The XMind connector preserves and displays media. Media understanding is not a
connector responsibility.

## 3. Decision Log

### 3.1 Proposed First-Release Connector Scope

- One XMind source points to one server-accessible local `.xmind` file.
- XMind 8 XML, classic/Zen JSON, and V26.02+ are separate format adapters.
- Each Sheet becomes one DocGraph Document.
- Each Topic-like knowledge element becomes a Section and a DocSection Node.
- Topic hierarchy and authored links become graph edges.
- Original workbook bytes, embedded images, and attachments are preserved.
- Search results can open the exact Topic and display its media.
- Unknown features are reported and remain recoverable from the original
  workbook snapshot.

Directory scanning can be added later using the same workbook identity rules.
It is not part of the first implementation milestone.

### 3.2 Explicit OCR Decision

OCR is not part of the XMind connector or its first release.

The connector must not:

- add an OCR provider or dependency;
- add `ocr`, `include_ocr`, or similar XMind source configuration;
- run OCR during sync;
- generate OCR-derived text;
- claim that image pixels or text inside an image are searchable;
- add an XMind-specific OCR queue, schema, or lifecycle.

Without media understanding, an image can still be found through existing
source text such as:

- owning Topic title;
- ancestor path;
- Notes, Labels, and Markers;
- image filename;
- XMind-provided caption or alt text;
- attachment filename and media type.

Whenever the owning Topic is returned by search, the Web UI must display the
associated image or attachment even if the media has no searchable text.

If DocGraph later gains a generic media-derived-content capability, that
capability must sit after all connectors and operate on generic media assets.
Enabling or changing that processor requires an explicit forced source
re-import/reprocess that rebuilds derived search text and affected embeddings.
It must not be automatically hidden behind an unchanged XMind content hash.

The future generic contract should include:

- a processor generation/version;
- a `force_reprocess` or equivalent explicit sync option;
- replacement of previous derived output for that generation;
- rebuilding of affected FTS and vector inputs;
- no automatic backfill in the XMind connector.

This future contract is documented for compatibility only. It is not a current
deliverable.

### 3.3 Deferred Scope

- pixel-level image search, OCR, or vision captions;
- faithful reproduction of the XMind canvas in DocGraph;
- editing and writing `.xmind` files;
- password-protected XMind files;
- automatic fetching of external HTTP or `file://` media references;
- historical retention of every successful workbook revision;
- source directory scanning;
- semantic classification of free-form Relationship labels into stronger edge
  types such as `depends_on`.

## 4. Current DocGraph Constraints

The implementation must account for these current behaviors:

- `internal/domain/types.go` `SectionInput` has no parent, depth, sibling order,
  source element ID, or metadata fields.
- `sections.metadata_json` exists in SQLite, but `ReplaceDocument` does not
  write it.
- `syncDocumentGraph` creates `Document -> every Section` `contains` edges,
  which flattens a Topic tree.
- `graphEdge` identifies an edge by `(src, dst, kind)`, which collapses multiple
  XMind Relationships between the same endpoints.
- document replacement deletes Sections but does not reconcile obsolete
  DocSection Nodes and authored edges.
- search relation expansion reads governance-approved, Document-level
  `knowledge_relations`; it does not read ordinary graph edges.
- `SourceArtifacts` currently means Documents, Sections, Entities, Nodes, and
  Edges. It is not a binary media model.
- search hits do not contain Node IDs or media summaries.
- Web search opens only the Document, not the matching Section.
- document detail loads a bounded flat Section list, and the current Web graph
  renders only a small flat subset rather than the real Topic topology.
- API token mode uses request headers. A protected media URL cannot be used as
  a plain `<img src>` without an authenticated Blob loader.

These are implementation constraints, not reasons to weaken the XMind model.

## 5. XMind Feature Compatibility Contract

| XMind feature | Canonical representation | Search behavior | Web behavior |
| --- | --- | --- | --- |
| Workbook | Source revision and raw snapshot | filename and source metadata | download current indexed snapshot |
| Sheet | Document | Sheet title and root context | Sheet document and ordered outline |
| Central/Main/Subtopic | Section + DocSection Node | title, path, Notes, Labels, Markers | exact Topic viewer |
| Deep hierarchy | section structure + `contains` edges | full ancestry participates in retrieval | lazy expandable tree |
| XMind Numbering | numbering config + derived display number | raw title searchable; display number is ordering/context, not an injected search term | display original numbering |
| Manually typed `1.xxx` | unchanged raw title | raw title searchable | unchanged title |
| Rich Notes | plain search projection + preserved rich source | note text searchable | readable rich/plain rendering |
| Labels | Section metadata and search text | label text searchable | label chips |
| Markers | raw IDs, resolved names, legend/custom metadata | resolved marker words searchable | marker icon/name/status |
| Topic image | media asset + Section reference | source text metadata only | authenticated preview/full image |
| Attachment | media asset + Section reference | filename and known metadata | preview when safe, otherwise download |
| Relationship | `related_to` authored edge | bounded authored-neighbor suggestion | label, endpoints, arrows, source provenance |
| Topic link | `links_to` authored edge | structural suggestion | navigate within/across Sheets |
| Floating Topic | independent Sheet root + position metadata | normal Topic search | separate root in outline/map context |
| Summary Topic | Section/Node + `summarizes` edges | summary text and target context | summary range and subtree |
| Boundary | Node + `groups` edges + visible text | boundary title associated with scope | boundary scope and title |
| Callout | Node/Section annotation + `annotates` edge | annotation text searchable | attached annotation |
| Task/To-Do | structured metadata + searchable display text | status/date/owner text when present | task metadata |
| Zone/V26 element | adapter-specific model when known | only known text indexed | structured display or preserved-only state |
| Unknown element | raw snapshot + feature inventory entry | no invented text | explicit unsupported diagnostic |

The compatibility matrix is part of acceptance. A parser that reads only
`children.attached` and Topic titles does not satisfy this design.

## 6. Format Adapter Architecture

Add a connector-specific intermediate representation under:

```text
internal/ingest/xmind
```

Recommended package structure:

```text
internal/ingest/xmind/
  detect.go
  model.go
  parse.go
  parse_json.go
  parse_xml.go
  normalize.go
  inventory.go
  limits.go
  testdata/
```

The parser first selects the package payload and then attempts the adapter whose
core structure matches. Producer and version metadata are provenance, not a
compatibility whitelist. JSON and XML still use separate adapters; a shared
permissive struct must not assume all versions have one stable shape.

Core Sheet/root Topic structure and stable source IDs are fatal boundaries.
Unknown or malformed local features are preserved in the snapshot and feature
inventory, then skipped or partially indexed with warnings when this does not
discard the Topic tree. A structurally compatible file may therefore parse even
when its producer/version has not been seen before; this is not a claim that all
features of that version have been verified.

Required fixture families for compatibility claims and feature expansion:

- XMind 8 `content.xml`;
- classic/Zen `content.json`;
- V26.02+ workbook containing Zone and current feature encodings.

Each family needs real, anonymized fixtures covering numbering, images,
attachments, Relationships, Topic links, Summary, Boundary, Callout, Floating
Topics, rich Notes, and multiple Sheets.

A normalized model should preserve both known semantics and unknown raw fields:

```go
type Workbook struct {
    FormatFamily string
    FormatVersion string
    Sheets []Sheet
    Assets []Asset
    FeatureInventory FeatureInventory
}

type Sheet struct {
    ID string
    Title string
    Roots []Topic
    Relationships []Relationship
    Elements []Element
}

type Topic struct {
    ID string
    Kind string
    RawTitle string
    Notes RichText
    Labels []string
    Markers []Marker
    Numbering Numbering
    Children []Topic
    Assets []AssetRef
    Links []Link
    Position Position
    RawFields map[string]json.RawMessage
}
```

The exact Go representation may change during fixture work, but the semantic
contract must not.

### 6.1 Unknown Fields

JSON parsing must not use strict decoding that discards extensions before they
are inventoried. XML parsing must preserve unhandled element names and enough
location information to report them.

The original package snapshot remains the authoritative lossless copy. Raw
extension fields in the intermediate model exist for diagnostics and adapter
evolution; they do not replace the source snapshot.

## 7. Canonical Document And Section Mapping

### 7.1 Source And Document Identity

For the first release:

```text
Source.Kind = "xmind"
Source.DSN  = local path or file:// path to one .xmind file
Document   = one Sheet
```

Recommended IDs:

```text
workbook_key = canonical source-local workbook identity
document_id  = stable("doc", source_id, workbook_key, sheet_id)
section_id   = stable("sec", source_id, workbook_key, topic_id)
node_id      = stable("node", "section", section_id)
```

Topic IDs are expected to be workbook-scoped. The parser must validate this.
If a legacy or malformed workbook repeats an ID across Sheets, fall back to a
Sheet-scoped composite ID and report the reduced cross-Sheet move stability.

Using workbook-scoped Topic IDs allows a Topic moved between Sheets to retain
its Section and Node identity. This requires workbook-level atomic reconcile;
it cannot be implemented safely as independent per-Sheet replacement.

### 7.2 Section Content

Canonical `content` contains user-visible source text only:

- Topic title is stored separately as title;
- Notes plain-text projection;
- visible Labels and resolved Marker names;
- visible task/status fields;
- visible hyperlink labels and targets;
- Callout/Summary/Boundary text when associated with the Topic.

Do not include:

- Base64 media bytes;
- entire descendant subtree text;
- opaque style JSON;
- OCR or other media-derived text;
- generated explanations that are not present in the source.

Add a separate `search_text` projection. In the first release it is built only
from source-authored text and known media metadata such as filename, caption,
and alt text. `content` remains the readable source projection.

### 7.3 Structure Storage

Add a generic `section_structures` table rather than hiding query-critical
fields in JSON:

```sql
create table section_structures (
  section_id text primary key references sections(id) on delete cascade,
  document_id text not null references documents(id) on delete cascade,
  parent_section_id text references sections(id) on delete cascade,
  source_element_id text not null,
  element_kind text not null,
  depth integer not null,
  sibling_ordinal integer not null,
  order_path_json text not null default '[]',
  display_number text not null default '',
  presentation_json text not null default '{}'
);
```

`order_path_json` is a structural path such as `[0,2,1]`; it is not derived
from titles. `presentation_json` holds XMind numbering configuration, position,
and other display metadata required for faithful context.

`SectionInput`, Store interfaces, SQLite replacement, Section summaries, and
detail APIs must carry this structure explicitly.

### 7.4 Numbering

XMind-generated numbering is independent of Topic text and follows Topic order.
Store:

- raw Topic title;
- child array order;
- numbering format, prefix, suffix, tiered behavior, and restart behavior;
- the derived display number for the indexed revision.

Search should index both raw and display forms. Manually typed numbering remains
part of the raw title and must never be stripped or rewritten.

## 8. Graph Mapping

### 8.1 Hierarchy

Use:

```text
Document --contains--> each Sheet root Topic
Parent Topic --contains--> Child Topic
```

Do not also create `Document --contains--> every Topic`; that produces a flat
second parent for every node.

Hierarchy queries must use `section_structures` for ordered children. Generic
`RelatedNodes` ordering is not a substitute for sibling order.

### 8.2 XMind Relationships

Use the repository-consistent edge kind:

```text
Topic A --related_to--> Topic B
```

Use `provenance = "source_authored"`. Do not copy these edges into governance
`knowledge_relations`.

The edge ID must use the original Relationship ID:

```text
edge_id = stable("edge", source_id, workbook_key, relationship_id)
```

Preserve in `metadata_json`:

```json
{
  "origin": "xmind_relationship",
  "xmind_relationship_id": "...",
  "label": "depends on",
  "source_topic_id": "...",
  "target_topic_id": "...",
  "semantic_direction": "undirected",
  "start_arrow": "...",
  "end_arrow": "...",
  "control_points": [],
  "style": {}
}
```

Endpoint order and visual arrows must be preserved. Direction must not be
interpreted as causality unless the source contains an explicit semantic signal.
The Topic viewer should normally query these edges with `direction=both`.

### 8.3 Other Authored Edges

| Source feature | Edge kind | Notes |
| --- | --- | --- |
| Topic Link | `links_to` | supports same-Sheet and cross-Sheet target |
| Summary | `summarizes` | one edge to every Topic in the source range |
| Boundary | `groups` | preserves grouped sibling range |
| Callout | `annotates` | callout points to owning Topic |

All authored edge IDs must use source element IDs where available.

### 8.4 Graph Reconcile

Before XMind graph ingestion, add document/workbook-scoped graph ownership and
reconcile support.

The replacement must:

- delete obsolete XMind-owned FTS node rows;
- delete obsolete XMind-owned Nodes and Edges;
- retain shared Product, Module, API, manually curated, and unrelated nodes;
- replace Relationships by original ID;
- remove deleted Topic links, Summary edges, Boundary edges, and Callout edges;
- leave no edge with missing or stale evidence after a successful sync.

An Upsert-only graph path is not acceptable for XMind.

## 9. Media And Source Snapshot Storage

### 9.1 Storage Layers

Do not store large media bytes in SQLite. Add a central content-addressed Blob
store rooted under the configured DocGraph data directory:

```text
<data_dir>/assets/blobs/sha256/ab/cd/<digest>
```

This is centralized DocGraph state, not a hidden checkout or cache next to each
source.

Recommended metadata tables:

```sql
media_blobs (
  sha256 text primary key,
  size_bytes integer not null,
  sniffed_media_type text not null,
  storage_key text not null,
  created_at text not null
);

source_snapshots (
  id text primary key,
  source_id text not null references sources(id) on delete cascade,
  source_hash text not null,
  blob_sha256 text not null references media_blobs(sha256),
  format_family text not null,
  format_version text not null default '',
  active integer not null default 0,
  created_at text not null
);

media_assets (
  id text primary key,
  source_id text not null references sources(id) on delete cascade,
  document_id text not null references documents(id) on delete cascade,
  external_id text not null,
  blob_sha256 text references media_blobs(sha256),
  kind text not null,
  original_name text not null default '',
  media_type text not null default '',
  size_bytes integer not null default 0,
  status text not null,
  metadata_json text not null default '{}',
  unique(document_id, external_id)
);

section_media_refs (
  section_id text not null references sections(id) on delete cascade,
  asset_id text not null references media_assets(id) on delete cascade,
  role text not null,
  ordinal integer not null,
  metadata_json text not null default '{}',
  primary key(section_id, asset_id, role, ordinal)
);
```

Media references preserve XMind resource ID, ZIP member path, placement, width,
height, crop, alignment, and original URI in metadata.

No OCR or media-derived-text table is part of this connector milestone.

### 9.2 Blob Lifecycle

Add an `internal/blobstore` abstraction with content-addressed `Put`, `Open`,
and garbage-collection support.

Required write behavior:

1. stream from ZIP while hashing and enforcing limits;
2. write into a job-scoped staging directory;
3. fsync and atomically rename to the digest path;
4. replace workbook metadata and references in one SQL transaction;
5. tolerate an unreferenced staged Blob after SQL failure;
6. garbage-collect only unreferenced Blobs older than a safety window;
7. never maintain a manually incremented reference count.

Default snapshot retention keeps at least the currently indexed successful
workbook revision. Historical retention policy is deferred. Sync job results
should record the active snapshot ID and source hash.

### 9.3 Media Safety

- MIME must be detected from bytes, not trusted from filename.
- ZIP entries must never be extracted using their paths.
- Enforce per-entry, total uncompressed size, file count, and compression-ratio
  limits.
- Embedded SVG, HTML, and unknown active formats are download-only by default.
- Do not fetch external HTTP, UNC, or `file://` media automatically.
- Preserve an `external` or `rejected` logical asset and diagnostic instead.
- A missing or rejected asset must remain visible in the Topic viewer.

## 10. Atomic Sync And Incremental Behavior

### 10.1 Sync Phases

```text
detect package
  -> parse entire workbook
  -> validate IDs, references, and feature inventory
  -> normalize all Sheets, Topics, edges, and media references
  -> stage workbook snapshot and media Blobs
  -> compute hashes and replacement plan
  -> atomically replace workbook-owned relational state
  -> activate source snapshot
  -> enqueue normal embedding ensure work
  -> garbage-collect unreferenced Blobs asynchronously
```

Parsing and reference validation must finish before deleting or replacing any
currently indexed data. A damaged, encrypted, unknown, or partially understood
package must not return an empty workbook and trigger stale-document deletion.

### 10.2 Replacement Unit

Use a workbook-level aggregate operation such as:

```go
ReplaceWorkbookBundle(ctx, bundle WorkbookBundle) error
```

It replaces:

- Sheet Documents;
- Topic Sections;
- section structures;
- Section FTS projections;
- source-owned graph Nodes and Edges;
- media assets and references;
- active source snapshot metadata.

Independent per-Sheet replacement is insufficient because Topics and links may
move across Sheets.

### 10.3 Hashes

Keep distinct hashes:

- `snapshot_hash`: exact `.xmind` package bytes;
- `semantic_hash`: normalized source-authored text, hierarchy, numbering,
  authored relationships, and visible metadata;
- `media_manifest_hash`: asset identities, references, statuses, placement, and
  Blob digests.

Style-only or thumbnail-only changes should not rebuild text embeddings.
Numbering, hierarchy, relationship labels, media metadata, or visible source
text changes must update the relevant search/display projections.

An image byte change updates the media manifest and Web evidence. It does not
by itself invent new text or require OCR.

## 11. Search And Context Behavior

### 11.1 Search Input

For an XMind Topic, build first-release `search_text` from:

- raw title and display-numbered title;
- complete ancestor path;
- Notes;
- Labels;
- resolved Marker names;
- task/status fields;
- link labels and targets;
- Relationship labels associated with the Topic;
- image captions, alt text, and filenames already present in XMind;
- attachment filenames and media types.

Do not append descendant subtree text or media bytes.

### 11.2 Search Result Contract

Extend `SearchHit` with:

- `node_id`;
- `element_kind`;
- `display_number`;
- structured ancestry or enough data to fetch it;
- bounded `media_assets` summaries;
- evidence field identifying title, Note, Label, Marker, Relationship label, or
  media metadata matches.

Hydrate media summaries in one batch after final search hits are chosen. Avoid
one query per hit.

### 11.3 Authored Relationship Suggestions

Current relation expansion is for approved Document relations. Add a separate,
bounded Topic-level source-authored expansion:

- start from the matched Section Node;
- expand one hop over `related_to` and `links_to`;
- return at most 3-5 authored neighbors per hit;
- label the evidence as an XMind author-created relation;
- do not promote a dense neighbor graph wholesale into primary hits;
- optionally add a small score only when the neighbor also matches the query.

Add a distinct response field such as:

```text
suggested_reads.authored_relations
```

Do not put these entries in `curated_relations`.

### 11.4 Agent Context

`doc_get_section` and context results must expose media summaries and authored
relations. A text-only agent must still be told that a Topic contains an image
or attachment and receive stable asset IDs and source provenance.

## 12. REST API Design

Add:

```text
GET /api/sections/{section_id}
GET /api/sections/{section_id}/context
GET /api/documents/{document_id}/outline
GET /api/documents/{document_id}/media-assets
GET /api/media-assets/{asset_id}
GET /api/media-assets/{asset_id}/content
GET /api/sources/{source_id}/snapshot
```

`/context` returns:

- exact Section/Topic;
- ordered ancestors;
- ordered direct children;
- bounded subtree on request;
- authored incoming/outgoing relationships;
- media assets;
- Sheet and source snapshot identifiers.

`/outline` must support lazy parent-based loading and pagination. It must not
depend on the current 200-Section document-detail limit.

Media content responses require:

- database lookup by opaque asset ID;
- normal API authorization;
- strong SHA-256 ETag;
- Range support where practical;
- `X-Content-Type-Options: nosniff`;
- `Cross-Origin-Resource-Policy: same-origin`;
- inline disposition only for explicitly safe raster formats;
- attachment disposition for HTML, SVG, PDF, unknown, or active content.

Never accept an arbitrary filesystem path in a media content request.

Extend source artifact counts with media asset count, missing/rejected count,
snapshot size, and total referenced media bytes. Keep the terminology clear:
`SourceArtifacts` is the sync product view; `media_assets` are binary-backed
source elements.

## 13. Web UX

### 13.1 Search

A search result for an XMind Topic must show:

- display-numbered title;
- collapsible full ancestor path;
- Sheet and source name;
- matching snippet and evidence kind;
- up to three media thumbnails;
- remaining media/attachment count;
- authored Relationship indicators.

Clicking the result opens the exact Topic, not just its Sheet Document.

### 13.2 Topic Viewer

The Topic viewer must provide:

- raw and display title when they differ;
- complete breadcrumb;
- ordered children;
- Notes, Labels, Markers, and task metadata;
- image gallery and attachments;
- incoming/outgoing `related_to`, `links_to`, `summarizes`, `groups`, and
  `annotates` relations;
- relation label and source provenance;
- Topic ID, Sheet ID, source snapshot ID, and original workbook download;
- explicit placeholders for missing, rejected, or preserved-only elements.

Large trees use lazy loading and a virtualized/collapsible outline. The existing
flat SVG document graph is not the XMind viewer.

### 13.3 Authenticated Media Loading

In token mode, Web media must use the existing authenticated request path to
fetch a Blob and create an object URL. Plain `<img src="/api/...">` does not send
the DocGraph token header. Object URLs must be revoked when the view is replaced.

### 13.4 Source Health And Coverage

The connector detail and health views must show a feature inventory, for
example:

```text
Topics: 1260 indexed
Images: 83 preserved and viewable, 2 rejected
Attachments: 14 preserved and downloadable
Relationships: 41 indexed
Boundaries: 12 indexed
Zones: 3 preserved-only
Unknown elements: 2 unsupported
```

Do not use wording that implies OCR readiness or OCR processing.

## 14. Configuration And Connector UI

First-release connector configuration:

```json
{
  "kind": "xmind",
  "name": "Architecture Maps",
  "dsn": "/path/to/architecture.xmind"
}
```

No XMind-specific `config_json` fields are required initially.

The UI help text must say that DSN is a path accessible to the DocGraph server
process. It is not a browser file upload.

If connector-specific fields are added later, update both `buildSourceConfig`
and `fillSourceForm`. The current Web edit flow rebuilds `config_json` from
visible form fields, so an unmodeled field would otherwise be silently lost.

The REST API and CLI should share source-kind validation. Currently REST has a
kind allowlist while CLI can persist an arbitrary kind.

## 15. Diagnostics And Failure Semantics

Every successful job result should include:

- detected format family and version;
- workbook snapshot ID and hash;
- Sheet, Topic, and authored-edge counts;
- media asset counts and total bytes;
- feature inventory grouped by indexed/preserved/rejected/unsupported;
- duplicate or missing component ID warnings;
- dangling relationship/link endpoints;
- reduced-identity-stability warnings;
- unknown element paths/types.

Fatal before-write errors include:

- damaged ZIP or invalid package;
- encrypted workbook;
- no recognized workbook content;
- duplicate critical IDs that cannot be disambiguated;
- unresolved required Sheet/root Topic hierarchy references;
- safety limit exceeded;
- a V26.02+ or unknown core layout that does not match an installed adapter.

Fatal parse errors must leave the previous successful Documents, Sections,
graph, media references, and snapshot active.

## 16. Security Limits

Implement global ingest limits rather than XMind-specific truncation behavior:

- archive byte size;
- ZIP entry count;
- per-entry uncompressed size;
- total uncompressed bytes;
- compression ratio;
- maximum normalized element count;
- maximum depth that fails explicitly rather than truncating;
- maximum rich-text size per element;
- allowed inline media types.

Do not write ZIP member paths to disk. Do not trust package filenames. Do not
follow external references without a separate explicit policy.

## 17. Implementation Phases

### Phase 0: Fixture And Parser Contract

Files:

```text
internal/ingest/xmind/model.go
internal/ingest/xmind/detect.go
internal/ingest/xmind/inventory.go
internal/ingest/xmind/testdata/*
internal/ingest/xmind/xmind_test.go
```

Deliverables:

- anonymized XMind 8, classic JSON, and V26.02+ fixtures;
- package detection matrix;
- feature inventory output;
- normalized model contract;
- explicit fixture coverage for all features in Section 5.

Acceptance:

- unknown elements are inventoried, not silently discarded;
- damaged/unknown packages fail before returning an empty workbook;
- image and attachment references resolve to package resources without writing
  them to arbitrary paths.

### Phase 1: Generic Structure, Snapshot, Media, And Reconcile Primitives

Files:

```text
internal/domain/types.go
internal/storage/storage.go
internal/storage/sqlschema/schema.go
internal/storage/sqlite/sqlite.go
internal/storage/sqlite/sqlite_test.go
internal/blobstore/*
internal/config/config.go
internal/app/app.go
```

Deliverables:

- section structure schema and APIs;
- `search_text` projection support;
- central Blob store;
- source snapshot and media tables;
- workbook aggregate replacement;
- source-owned graph reconcile;
- unreferenced Blob garbage collection.

Acceptance:

- aggregate replacement is transactional for relational state;
- deleting a Topic, Relationship, image, or attachment leaves no ghost rows;
- shared/manual graph data is retained;
- source deletion removes logical references and later GC removes unreferenced
  Blobs.

### Phase 2: XMind Adapters And Normalization

Files:

```text
internal/ingest/xmind/parse.go
internal/ingest/xmind/parse_json.go
internal/ingest/xmind/parse_xml.go
internal/ingest/xmind/normalize.go
internal/ingest/xmind/limits.go
internal/ingest/xmind/xmind_test.go
```

Deliverables:

- three format adapters proven against fixtures;
- Topic hierarchy and numbering normalization;
- media and attachment resolution;
- authored Relationship and special-element normalization;
- semantic, media-manifest, and snapshot hashes.

Acceptance:

- all fixtures produce deterministic normalized output;
- no OCR code, dependency, configuration, or derived text exists;
- V26-specific unknown data is preserved and reported.

### Phase 3: Sync Integration

Files:

```text
internal/sync/sync.go
internal/sync/sync_test.go
internal/jobs/jobs.go
```

Deliverables:

- `xmind` source dispatch;
- workbook-level atomic replacement;
- snapshot activation and media staging;
- embedding ensure integration for source-authored text only;
- feature coverage in job results.

Acceptance:

- parse failures do not erase previous indexed state;
- unchanged semantic content avoids unnecessary text/profile/embedding rebuild;
- media-only changes update evidence without inventing text;
- duplicate sync protection continues to work.

### Phase 4: Search, Context, REST, And MCP

Files:

```text
internal/domain/types.go
internal/storage/sqlite/sqlite.go
internal/query/context.go
internal/server/server.go
internal/server/server_test.go
internal/mcp/server.go
internal/mcp/server_test.go
```

Deliverables:

- Topic context and outline APIs;
- media and snapshot APIs;
- `SearchHit` Node/media/element fields;
- bounded source-authored relation suggestions;
- media summaries in section/context responses.

Acceptance:

- search hit to exact Topic is a direct navigation path;
- authored relations are distinguishable from curated relations;
- a text-only MCP client can see that media evidence exists;
- no N+1 media hydration in search.

### Phase 5: Web Topic And Media Experience

Files:

```text
internal/web/static/index.html
internal/web/static/main.js
internal/web/static/styles.css
internal/web/static/i18n/en.json
internal/web/static/i18n/zh-CN.json
```

Deliverables:

- XMind connector option and help;
- lazy outline and Topic viewer;
- authenticated image gallery and attachment download;
- authored relation display;
- source feature coverage and media diagnostics;
- exact Topic navigation from search.

Acceptance:

- token and no-auth modes both display media;
- large/deep Sheets remain usable;
- preserved-only and rejected assets are visible;
- Web does not imply OCR or pixel-level search.

### Phase 6: Documentation And Release Validation

Files:

```text
README.md
README.zh-CN.md
docs/docgraph-architecture.md
docs/docgraph-acceptance.md
```

Deliverables:

- connector setup and DSN examples;
- supported feature matrix;
- explicit unsupported/encrypted/OCR behavior;
- migration, storage, backup, and asset GC notes.

## 18. Required Test Matrix

### 18.1 Parser

- XMind 8 XML, classic JSON, and V26.02+ detection;
- multiple Sheets;
- attached and Floating Topics;
- at least 10 levels of depth;
- XMind numbering, tiered numbering, restart, and manual `1.xxx` titles;
- plain/rich Notes, Labels, Markers, links, Tasks;
- images, repeated image references, attachments, missing assets;
- multiple Relationships between the same endpoints;
- single-arrow, double-arrow, and visually undirected Relationships;
- Summary, Boundary, Callout, Zone, and unknown elements;
- corrupted, encrypted, oversized, high-compression-ratio packages.

### 18.2 Identity And Reconcile

- rename Topic: stable Section/Node ID;
- reorder siblings: stable ID, changed sibling order/display number;
- move Topic to another parent: stable ID, updated path and edges;
- move Topic across Sheets: stable ID and updated Document ownership;
- duplicate titles: distinct IDs and paths;
- delete Topic: no Section, Node, edge, media ref, or FTS ghost;
- delete one of two same-endpoint Relationships: the other remains;
- change Relationship label: stable edge ID and updated metadata;
- delete Sheet: stale Document and owned state removed;
- failed sync: previous active snapshot and index remain intact.

### 18.3 Search And Web

- 500+ Topics and at least 10 levels deep;
- a Topic after the current flat-document limits is searchable and directly
  openable;
- raw titles remain searchable; XMind-generated numbering is displayed and available as ordered context without being injected as a search term;
- Topic hit displays associated image even when the image has no readable text;
- image filename/caption search returns the owning Topic and image;
- no test expects pixel text or OCR output;
- authored Relationship appears as a bounded suggestion and in Topic context;
- cross-Sheet Topic link navigates to the target;
- token-authenticated media loads without query-string credentials;
- original indexed workbook snapshot is downloadable.

### 18.4 Future Generic Media Reprocessing

This is not a first-release test suite. When a generic media processor exists,
it must add tests proving that an explicit forced re-import/reprocess bypasses
unchanged-source short-circuiting, replaces prior processor output, and rebuilds
affected FTS/vector inputs. Those tests belong to the generic media capability,
not `internal/ingest/xmind`.

## 19. Acceptance Criteria

The XMind connector is acceptable when all of the following are true:

- A deep Topic can be searched and opened directly.
- Full ancestry and sibling order match the source workbook.
- XMind-generated numbering and manually typed numbering remain distinguishable.
- Renames, moves, and reorders do not churn Topic identity.
- Images and attachments are preserved, visible, attributable, and downloadable.
- A Topic search hit shows its media even without OCR or image understanding.
- The product does not claim pixel-content search.
- Multiple same-endpoint Relationships remain distinct.
- XMind Relationships appear as source-authored `related_to` edges with their
  original label and visual direction metadata.
- Topic links, Summary, Boundary, Callout, and Floating Topics are not silently
  flattened away.
- Deleted source elements leave no ghost data.
- Unknown/V26 elements appear in feature coverage and remain recoverable from
  the original snapshot.
- A failed import never replaces the previous successful index with an empty or
  partial result.
- OCR remains absent from connector code, configuration, dependencies, and UI
  promises.

## 20. Open Questions

These questions should be answered by fixture evidence before their phase is
implemented:

- Exact V26.02+ resource, Zone, numbering, and endpoint encodings.
- Whether Topic component IDs are always workbook-unique in every supported
  format family.
- Which current marker legends/custom marker names are embedded in the package.
- Which attachment formats can be safely previewed inline rather than downloaded.
- Default global archive/media limits based on representative internal files.
- Whether the Web UI later needs faithful XMind canvas rendering in addition to
  the required outline/Topic viewer.
- Historical source snapshot retention beyond the current successful revision.

Open questions must not be resolved with one fixture-specific hardcoded rule.

## 21. Recommended Starting Step

Start with Phase 0, not with the source-kind switch or Web connector option.

The first implementation task is:

1. collect and anonymize one XMind 8, one classic JSON, and one V26.02+ workbook;
2. ensure the fixtures collectively include numbering, images, attachments,
   Relationships, Topic links, Summary, Boundary, Callout, Floating Topics,
   multiple Sheets, and at least one unknown/current element;
3. implement package detection and a feature-inventory test that lists every
   encountered element and resource without writing to storage;
4. review the inventory before defining final adapter structs.

This prevents the connector from being shaped around a guessed or obsolete
`content.json` example and gives every subsequent phase an executable contract.

## References

- [XMind V26.02 file format compatibility notice](https://xmind.com/help/why-cant-open-files-created-v26-02-older-versions)
- [XMind Numbering](https://xmind.com/user-guide/numbering-new)
- [XMind Relationship](https://xmind.com/user-guide/relationship-new)
- [XMind Picture](https://xmind.com/user-guide/pic-new)
- [XMind Attachment](https://xmind.com/user-guide/attachment-new)
- [XMind Summary](https://xmind.com/user-guide/summary-new)
- [XMind Boundary](https://xmind.com/user-guide/boundary-new)
- [XMind Callout](https://xmind.com/user-guide/callout-new)
- [Official XMind JS SDK](https://github.com/xmindltd/xmind-sdk-js)
- [Official legacy XMind model schemas](https://github.com/xmindltd/xmind-model)
