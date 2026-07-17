package sqlite

import (
	"context"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
)

func TestRebuildGraphMembershipsBackfillsLegacyOwnershipAndEvidence(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-a")
	createTestSource(t, ctx, store, "source-b")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID: "doc-a", SourceID: "source-a", ExternalID: "a.md", Title: "A", ContentHash: "doc-a-hash",
	}, []domain.SectionInput{{ID: "section-a", Title: "A", Content: "a", ContentHash: "section-a-hash"}}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`insert into nodes (id, kind, name, canonical_name, metadata_json) values ('node-a', 'Document', 'A', 'a', '{"source_id":"source-a","document_id":"doc-a"}')`,
		`insert into nodes (id, kind, name, canonical_name, metadata_json) values ('node-b', 'Document', 'B', 'b', '{"source_id":"source-b"}')`,
		`insert into nodes (id, kind, name, canonical_name, owner_source_id) values ('node-owner', 'Product', 'Owner', 'owner', 'source-a')`,
		`insert into nodes (id, kind, name, canonical_name) values ('manual-1', 'Product', 'Manual 1', 'manual-1')`,
		`insert into nodes (id, kind, name, canonical_name) values ('manual-2', 'Product', 'Manual 2', 'manual-2')`,
		`insert into edges (id, src_id, dst_id, kind) values ('edge-cross', 'node-a', 'node-b', 'related_to')`,
		`insert into edges (id, src_id, dst_id, kind, evidence_section_id) values ('edge-evidence', 'manual-1', 'manual-2', 'describes', 'section-a')`,
		`insert into edges (id, src_id, dst_id, kind, owner_source_id) values ('edge-owner', 'manual-1', 'manual-2', 'contains', 'source-b')`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec legacy fixture %q: %v", statement, err)
		}
	}

	for run := 0; run < 2; run++ {
		if err := store.rebuildGraphMemberships(ctx); err != nil {
			t.Fatalf("rebuild run %d: %v", run, err)
		}
	}

	assertCount(t, ctx, store, "source_nodes", "source_id = 'source-a'", 2)
	assertCount(t, ctx, store, "source_nodes", "source_id = 'source-b'", 1)
	assertCount(t, ctx, store, "document_nodes", "document_id = 'doc-a' and node_id = 'node-a'", 1)
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-a'", 2)
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-b'", 2)

	aEdges, err := store.ListSourceEdges(ctx, "source-a", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !edgeSummariesContain(aEdges, "edge-cross") || !edgeSummariesContain(aEdges, "edge-evidence") {
		t.Fatalf("source-a edges = %+v", aEdges)
	}
	bEdges, err := store.ListSourceEdges(ctx, "source-b", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !edgeSummariesContain(bEdges, "edge-cross") || !edgeSummariesContain(bEdges, "edge-owner") {
		t.Fatalf("source-b edges = %+v", bEdges)
	}
}

func TestMigrateV6BackfillsGraphMembershipsOnlyOnce(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-v6")

	if _, err := store.db.ExecContext(ctx, `
insert into nodes (id, kind, name, canonical_name, metadata_json)
values ('legacy-before-v6', 'Document', 'Before V6', 'before-v6', '{"source_id":"source-v6"}')
`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `delete from schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `insert into schema_migrations(version) values (5)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v5 to v6 returned error: %v", err)
	}
	assertCount(t, ctx, store, "source_nodes", "source_id = 'source-v6' and node_id = 'legacy-before-v6'", 1)

	// A second migrate at v6 must not rescan graph JSON. Production writes use
	// the structured maintenance helpers instead of relying on migration.
	if _, err := store.db.ExecContext(ctx, `
insert into nodes (id, kind, name, canonical_name, metadata_json)
values ('legacy-after-v6', 'Document', 'After V6', 'after-v6', '{"source_id":"source-v6"}')
`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate at v6 returned error: %v", err)
	}
	assertCount(t, ctx, store, "source_nodes", "source_id = 'source-v6' and node_id = 'legacy-after-v6'", 0)
}

func TestGraphMembershipWritesMoveIncidentEdgesBetweenSources(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-a")
	createTestSource(t, ctx, store, "source-b")

	if err := store.UpsertNode(ctx, domain.NodeInput{ID: "moving", Kind: "API", Name: "Moving", MetadataJSON: `{"source_id":"source-a"}`}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertNode(ctx, domain.NodeInput{ID: "manual", Kind: "API", Name: "Manual"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{ID: "moving-edge", SrcID: "moving", DstID: "manual", Kind: "related_to"}); err != nil {
		t.Fatal(err)
	}
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-a' and edge_id = 'moving-edge'", 1)
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-b' and edge_id = 'moving-edge'", 0)

	if err := store.UpsertNode(ctx, domain.NodeInput{ID: "moving", Kind: "API", Name: "Moving", MetadataJSON: `{"source_id":"source-b"}`}); err != nil {
		t.Fatal(err)
	}
	assertCount(t, ctx, store, "source_nodes", "source_id = 'source-a' and node_id = 'moving'", 0)
	assertCount(t, ctx, store, "source_nodes", "source_id = 'source-b' and node_id = 'moving'", 1)
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-a' and edge_id = 'moving-edge'", 0)
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-b' and edge_id = 'moving-edge'", 1)
}

func TestDeleteDocumentsNotInSourceRemovesDocumentNodesAndEvidenceOnlyMembership(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-a")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID: "doc-a", SourceID: "source-a", ExternalID: "a.md", Title: "A", ContentHash: "doc-a-hash",
	}, []domain.SectionInput{{ID: "section-a", Title: "A", Content: "a", ContentHash: "section-a-hash"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID: "doc-node", Kind: "Document", Name: "A", MetadataJSON: `{"source_id":"source-a","document_id":"doc-a"}`,
	}); err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"manual-1", "manual-2"} {
		if err := store.UpsertNode(ctx, domain.NodeInput{ID: nodeID, Kind: "Product", Name: nodeID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{
		ID: "replaced-evidence", SrcID: "manual-1", DstID: "manual-2", Kind: "describes", EvidenceSectionID: "section-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID: "doc-a", SourceID: "source-a", ExternalID: "a.md", Title: "A", ContentHash: "doc-a-hash-2",
	}, []domain.SectionInput{{ID: "section-b", Title: "B", Content: "b", ContentHash: "section-b-hash"}}); err != nil {
		t.Fatal(err)
	}
	assertCount(t, ctx, store, "edges", "id = 'replaced-evidence' and evidence_section_id is null", 1)
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-a' and edge_id = 'replaced-evidence'", 0)

	if err := store.UpsertEdge(ctx, domain.EdgeInput{
		ID: "evidence-only", SrcID: "manual-1", DstID: "manual-2", Kind: "describes", EvidenceSectionID: "section-b",
	}); err != nil {
		t.Fatal(err)
	}
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-a' and edge_id = 'evidence-only'", 1)

	if err := store.DeleteDocumentsNotInSource(ctx, "source-a", nil); err != nil {
		t.Fatal(err)
	}
	assertCount(t, ctx, store, "nodes", "id = 'doc-node'", 0)
	assertCount(t, ctx, store, "fts_nodes", "node_id = 'doc-node'", 0)
	assertCount(t, ctx, store, "fts_nodes_shortgrams", "node_id = 'doc-node'", 0)
	assertCount(t, ctx, store, "edges", "id = 'evidence-only' and evidence_section_id is null", 1)
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-a' and edge_id = 'evidence-only'", 0)
}

func TestDeleteSourceUsesMaterializedMembershipAndPreservesUnownedGraph(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-a")
	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID: "source-node", Kind: "Product", Name: "Source", MetadataJSON: `{"source_id":"source-a"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertNode(ctx, domain.NodeInput{ID: "manual-node", Kind: "Product", Name: "Manual"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{ID: "source-edge", SrcID: "source-node", DstID: "manual-node", Kind: "related_to"}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSource(ctx, "source-a"); err != nil {
		t.Fatal(err)
	}
	assertCount(t, ctx, store, "nodes", "id = 'source-node'", 0)
	assertCount(t, ctx, store, "fts_nodes", "node_id = 'source-node'", 0)
	assertCount(t, ctx, store, "edges", "id = 'source-edge'", 0)
	assertCount(t, ctx, store, "nodes", "id = 'manual-node'", 1)
	assertCount(t, ctx, store, "source_nodes", "source_id = 'source-a'", 0)
	assertCount(t, ctx, store, "source_edges", "source_id = 'source-a'", 0)
}

func TestDeleteSourceRespectsEffectiveNodeOwnership(t *testing.T) {
	t.Run("metadata membership does not override another source owner", func(t *testing.T) {
		ctx := context.Background()
		store := openMigratedTempStore(t, ctx)
		createTestSource(t, ctx, store, "source-a")
		createTestSource(t, ctx, store, "source-b")

		if err := store.UpsertNode(ctx, domain.NodeInput{
			ID: "owned-a-visible-b", Kind: "Product", Name: "Owned A Visible B", MetadataJSON: `{"source_id":"source-b"}`,
		}); err != nil {
			t.Fatal(err)
		}
		setNodeOwnerForTest(t, ctx, store, "owned-a-visible-b", "source-a")
		if err := store.UpsertNode(ctx, domain.NodeInput{ID: "peer-a", Kind: "Product", Name: "Peer A"}); err != nil {
			t.Fatal(err)
		}
		setNodeOwnerForTest(t, ctx, store, "peer-a", "source-a")
		if err := store.UpsertEdge(ctx, domain.EdgeInput{ID: "edge-a", SrcID: "owned-a-visible-b", DstID: "peer-a", Kind: "related_to"}); err != nil {
			t.Fatal(err)
		}

		assertCount(t, ctx, store, "source_nodes", "source_id = 'source-a' and node_id = 'owned-a-visible-b' and (ref_mask & 2) != 0", 1)
		assertCount(t, ctx, store, "source_nodes", "source_id = 'source-b' and node_id = 'owned-a-visible-b' and (ref_mask & 1) != 0", 1)
		if err := store.DeleteSource(ctx, "source-b"); err != nil {
			t.Fatal(err)
		}

		assertCount(t, ctx, store, "nodes", "id = 'owned-a-visible-b' and owner_source_id = 'source-a'", 1)
		assertCount(t, ctx, store, "fts_nodes", "node_id = 'owned-a-visible-b'", 1)
		assertCount(t, ctx, store, "fts_nodes_trigram", "node_id = 'owned-a-visible-b'", 1)
		assertCount(t, ctx, store, "edges", "id = 'edge-a'", 1)
		assertCount(t, ctx, store, "source_nodes", "source_id = 'source-b'", 0)
	})

	t.Run("explicit owner is deleted despite another metadata membership", func(t *testing.T) {
		ctx := context.Background()
		store := openMigratedTempStore(t, ctx)
		createTestSource(t, ctx, store, "source-a")
		createTestSource(t, ctx, store, "source-b")

		if err := store.UpsertNode(ctx, domain.NodeInput{
			ID: "owned-b-visible-a", Kind: "Product", Name: "Owned B Visible A", MetadataJSON: `{"source_id":"source-a"}`,
		}); err != nil {
			t.Fatal(err)
		}
		setNodeOwnerForTest(t, ctx, store, "owned-b-visible-a", "source-b")
		if err := store.UpsertNode(ctx, domain.NodeInput{ID: "manual-peer", Kind: "Product", Name: "Manual Peer"}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertEdge(ctx, domain.EdgeInput{ID: "edge-owned-b", SrcID: "owned-b-visible-a", DstID: "manual-peer", Kind: "related_to"}); err != nil {
			t.Fatal(err)
		}

		if err := store.DeleteSource(ctx, "source-b"); err != nil {
			t.Fatal(err)
		}
		assertCount(t, ctx, store, "nodes", "id = 'owned-b-visible-a'", 0)
		assertCount(t, ctx, store, "fts_nodes", "node_id = 'owned-b-visible-a'", 0)
		assertCount(t, ctx, store, "fts_nodes_trigram", "node_id = 'owned-b-visible-a'", 0)
		assertCount(t, ctx, store, "edges", "id = 'edge-owned-b'", 0)
		assertCount(t, ctx, store, "nodes", "id = 'manual-peer'", 1)
	})

	t.Run("legacy metadata-only node keeps historical lifecycle", func(t *testing.T) {
		ctx := context.Background()
		store := openMigratedTempStore(t, ctx)
		createTestSource(t, ctx, store, "source-b")

		if err := store.UpsertNode(ctx, domain.NodeInput{
			ID: "legacy-b", Kind: "Product", Name: "Legacy B", MetadataJSON: `{"source_id":"source-b"}`,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertNode(ctx, domain.NodeInput{ID: "manual-peer", Kind: "Product", Name: "Manual Peer"}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertEdge(ctx, domain.EdgeInput{ID: "edge-legacy-b", SrcID: "legacy-b", DstID: "manual-peer", Kind: "related_to"}); err != nil {
			t.Fatal(err)
		}

		if err := store.DeleteSource(ctx, "source-b"); err != nil {
			t.Fatal(err)
		}
		assertCount(t, ctx, store, "nodes", "id = 'legacy-b'", 0)
		assertCount(t, ctx, store, "fts_nodes", "node_id = 'legacy-b'", 0)
		assertCount(t, ctx, store, "fts_nodes_trigram", "node_id = 'legacy-b'", 0)
		assertCount(t, ctx, store, "edges", "id = 'edge-legacy-b'", 0)
		assertCount(t, ctx, store, "nodes", "id = 'manual-peer'", 1)
	})
}

func TestDeleteDocumentsNotInSourcePreservesNodeOwnedByAnotherSource(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-a")
	createTestSource(t, ctx, store, "source-b")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID: "doc-b", SourceID: "source-b", ExternalID: "b.md", Title: "B", ContentHash: "doc-b-hash",
	}, []domain.SectionInput{{ID: "section-b", Title: "B", Content: "b", ContentHash: "section-b-hash"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID: "owned-a-doc-b", Kind: "Document", Name: "Owned A Doc B",
		MetadataJSON: `{"source_id":"source-a","document_id":"doc-b"}`,
	}); err != nil {
		t.Fatal(err)
	}
	setNodeOwnerForTest(t, ctx, store, "owned-a-doc-b", "source-a")
	if err := store.UpsertNode(ctx, domain.NodeInput{ID: "peer-a", Kind: "Product", Name: "Peer A"}); err != nil {
		t.Fatal(err)
	}
	setNodeOwnerForTest(t, ctx, store, "peer-a", "source-a")
	if err := store.UpsertEdge(ctx, domain.EdgeInput{ID: "edge-doc-b", SrcID: "owned-a-doc-b", DstID: "peer-a", Kind: "related_to"}); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteDocumentsNotInSource(ctx, "source-b", nil); err != nil {
		t.Fatal(err)
	}
	assertCount(t, ctx, store, "documents", "id = 'doc-b'", 0)
	assertCount(t, ctx, store, "document_nodes", "document_id = 'doc-b'", 0)
	assertCount(t, ctx, store, "nodes", "id = 'owned-a-doc-b' and owner_source_id = 'source-a'", 1)
	assertCount(t, ctx, store, "fts_nodes", "node_id = 'owned-a-doc-b'", 1)
	assertCount(t, ctx, store, "fts_nodes_trigram", "node_id = 'owned-a-doc-b'", 1)
	assertCount(t, ctx, store, "edges", "id = 'edge-doc-b'", 1)
}

func setNodeOwnerForTest(t *testing.T, ctx context.Context, store *Store, nodeID, sourceID string) {
	t.Helper()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `update nodes set owner_source_id = ? where id = ?`, sourceID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("set owner for node %s affected %d rows, err=%v", nodeID, affected, err)
	}
	if err := replaceNodeMembershipsTx(ctx, tx, nodeID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func edgeSummariesContain(edges []domain.EdgeSummary, id string) bool {
	for _, edge := range edges {
		if edge.ID == id {
			return true
		}
	}
	return false
}
