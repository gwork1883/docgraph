# Real XMind fixtures

## `烘焙知识体系.xmind`

- SHA-256: `d4ab251a7ecd528210720693119381464c9a786b0b60326626036624c38874f0`
- Size: 185,594 bytes
- Producer metadata: `Vana` version `12.0.2.202204260729`
- Data structure version: `3`
- Layout engine version: `5`
- Provenance: created by the repository user as an XMind connector test. No
  post-save package editing was reported. The producer platform and creation
  date were not recorded and therefore remain unknown.
- Payload evidence: `manifest.json` declares `content.json`; the unlisted root
  `content.xml` is retained as an inactive compatibility payload.

This fixture proves the manifest-selected legacy JSON v3 package signature,
one Sheet, 84 stable Topic IDs, attached hierarchy through depth 3, and manual
number-like title text. It does not prove XMind 8 XML, V26.02+, dynamic
numbering, Relationships, links, Topic media/attachments, multiple Sheets,
Floating Topics, Summary, Boundary, Callout, or Zone instances. The package
thumbnail is not Topic media.

The fixture bytes are an immutable compatibility contract. Tests verify the
hash before using it; update this record and the explicit compatibility review
if the file is intentionally replaced.

## `classic-rich-v1.xmind`

- SHA-256: `4df1a27ee86803aa9972953c176970472ef5ce44fa622f56e9664c3e99a56701`
- Size: 297,289 bytes
- Producer metadata: `Vana` version `26.01.03145`
- Data structure version: `2`
- Layout engine version: `5`
- Provenance: created by the repository user as a rich connector fixture. No
  post-save package editing was reported. The producer platform and exact
  creation workflow were not recorded and therefore remain unknown.
- Payload evidence: `manifest.json` declares `content.json`; the unlisted root
  `content.xml` is retained as an inactive compatibility payload.

This fixture proves a pre-26.02 legacy JSON v2 signature, 3 Sheets, 55 Topics,
depth 12, one Floating Topic, Summary range, Callout, Relationship, external
Topic link, Label, Markers, and 5 image references to 4 embedded resources. It
also proves repeated resource references and content sniffing where a `.png`
path contains JPEG bytes.

It does not contain dynamic Numbering, Notes, Boundary, structured Task fields,
attachments, internal/cross-Sheet Topic links, multiple same-endpoint
Relationships, authored arrow/style data, Zones, or 500+ Topics. Those
capabilities remain fixture-gated. The package bytes are immutable and their
hash is asserted by parser and production-sync tests.
