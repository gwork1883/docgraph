# XMind fixture status

The repository has two real fixtures under `docs/tests/`. Their provenance and
exact hashes are recorded in `docs/tests/README.md`. Together they prove
manifest-selected pre-26.02 legacy Sheet-array JSON with
`dataStructureVersion=2` and `dataStructureVersion=3`, plus their explicitly
recorded feature coverage. The following format families remain unproven:

- XMind 8 XML (`content.xml`);
- XMind V26.02+ and its current Zone/resource/numbering/endpoint encodings.

Do not add a hand-built ZIP and describe it as a real format fixture. ZIPs made
in memory by `xmind_test.go` exercise only detector, failure, inventory, and
archive-security contracts. They are not compatibility evidence and must not be
used to mark an adapter supported.

`ParseCandidateForValidation` and `ParseWorkbook` use the same structural
adapters. Producer/version metadata is recorded as provenance rather than used
as a whitelist. A manifest-selected JSON payload, or the unique root
`content.json` when no manifest exists, is admitted when its Sheet-array,
rootTopic, and required IDs validate. `xmap-content` XML is admitted through the
legacy XML adapter when its Sheet/root Topic structure validates; unknown local
elements and unverified namespace/version values are inventoried. Structurally
different V26.02+ or unknown JSON remains fail-closed because no current schema
or real fixture is available, and no V26.02+ discriminator is guessed.

Each future real, anonymized fixture must include provenance recording:

- XMind product name, exact version/build, and platform;
- creation/export date and method;
- fixture SHA-256;
- observed root ZIP entries and expected format family;
- the feature coverage it provides;
- the anonymization method;
- whether package bytes were changed after XMind saved the file.

Prefer anonymizing source content inside XMind and saving it again. If a package
is edited after export, retain that fact in provenance and treat the derivative
as a parser-mechanics fixture, not sole evidence of the producer's format.

The real fixture set must collectively cover multiple Sheets, attached and
Floating Topics, 10+ levels, dynamic/tiered/restarted numbering, a manually
typed `1.xxx` title, Notes, Labels, Markers, tasks, images, repeated and missing
media references, attachments, same-endpoint Relationships, arrow/control/style
metadata, cross-Sheet Topic links, Summary, Boundary, Callout, Zone, and at
least one unknown/current element. A representative workbook must contain 500+
Topics.
