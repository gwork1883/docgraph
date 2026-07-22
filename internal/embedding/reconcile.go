package embedding

import (
	"sort"
	"strings"

	"github.com/docgraph/docgraph/internal/domain"
)

// EmbeddingPlan identifies the active chunk representation used to evaluate
// whether stored chunks are current. Dimensions are intentionally excluded:
// they are a model invariant rather than part of chunk identity.
type EmbeddingPlan struct {
	Model            string
	GeneratorVersion string
	Tokenizer        string
	ChunkStrategy    string
}

type SectionEmbeddingState string

const (
	SectionEmbeddingReady   SectionEmbeddingState = "ready"
	SectionEmbeddingPending SectionEmbeddingState = "pending"
	SectionEmbeddingStale   SectionEmbeddingState = "stale"
)

// ExpectedSectionEmbedding couples a current relational Section with the
// chunks expected from the active embedding plan.
type ExpectedSectionEmbedding struct {
	Section domain.EmbeddingSection
	Chunks  []EmbeddingChunk
}

// SectionEmbeddingEvaluation is the pure, per-Section currentness decision
// shared by Ensure and source-level reconciliation.
//
// CurrentExpectedChunks counts logical expected chunks. A deterministic pair
// of retry-split chunks therefore satisfies one expected chunk, matching the
// compatibility behavior already used by Ensure.
type SectionEmbeddingEvaluation struct {
	SectionID              string
	DocumentID             string
	State                  SectionEmbeddingState
	Reason                 string
	ExpectedChunks         int
	CurrentExpectedChunks  int
	MissingExpectedChunks  int
	ActivePlanChunks       int
	LegacyPlanChunks       int
	UnexpectedActiveChunks int
}

// EmbeddingReconciliation classifies a complete current Section snapshot
// against stored chunk metadata. Orphans are independent of plan: once their
// Section is absent from the relational snapshot, every stored plan is orphaned.
// LegacyPlanSectionIDs only contains current Sections that also retain chunks
// outside the active plan.
type EmbeddingReconciliation struct {
	Sections                    []SectionEmbeddingEvaluation
	ReadySectionIDs             []string
	PendingSectionIDs           []string
	StaleSectionIDs             []string
	OrphanSectionIDs            []string
	LegacyPlanSectionIDs        []string
	OrphanChunkCount            int
	ActivePlanOrphanChunkCount  int
	LegacyPlanOrphanChunkCount  int
	CurrentLegacyPlanChunkCount int
}

// EvaluateSectionEmbedding evaluates one current Section against stored chunk
// metadata. stored may contain both active and legacy plans; only active-plan
// chunks can satisfy expected chunks.
func EvaluateSectionEmbedding(section domain.EmbeddingSection, expected []EmbeddingChunk, stored []domain.EmbeddingChunkHash, plan EmbeddingPlan) SectionEmbeddingEvaluation {
	evaluation := SectionEmbeddingEvaluation{
		SectionID:      section.SectionID,
		DocumentID:     section.DocumentID,
		ExpectedChunks: len(expected),
	}
	active := make(map[string]domain.EmbeddingChunkHash, len(stored))
	for _, hash := range stored {
		if hash.SectionID != section.SectionID {
			continue
		}
		if embeddingPlanMatchesHash(plan, hash) {
			evaluation.ActivePlanChunks++
			active[hash.ChunkID] = hash
		} else {
			evaluation.LegacyPlanChunks++
		}
	}

	consumedActiveChunks := map[string]bool{}
	for _, chunk := range expected {
		hash := active[chunk.ChunkID]
		if embeddingChunkHashCurrent(hash, chunk, section, plan.Model, plan.GeneratorVersion) {
			consumedActiveChunks[chunk.ChunkID] = true
			evaluation.CurrentExpectedChunks++
			continue
		}
		if splitChunkIDs, ok := retrySplitChunkIDsCurrent(active, chunk, section, plan.Model, plan.GeneratorVersion); ok {
			for _, chunkID := range splitChunkIDs {
				consumedActiveChunks[chunkID] = true
			}
			evaluation.CurrentExpectedChunks++
			continue
		}
		evaluation.MissingExpectedChunks++
	}
	evaluation.UnexpectedActiveChunks = evaluation.ActivePlanChunks - len(consumedActiveChunks)

	switch {
	case len(expected) > 0 && evaluation.MissingExpectedChunks == 0 && evaluation.UnexpectedActiveChunks == 0:
		evaluation.State = SectionEmbeddingReady
	case evaluation.ActivePlanChunks == 0:
		evaluation.State = SectionEmbeddingPending
		evaluation.Reason = "active_plan_missing"
	default:
		evaluation.State = SectionEmbeddingStale
		if evaluation.UnexpectedActiveChunks > 0 && evaluation.MissingExpectedChunks == 0 {
			evaluation.Reason = "unexpected_active_chunks"
		} else {
			evaluation.Reason = "expected_chunks_incomplete"
		}
	}
	return evaluation
}

// EvaluateEmbeddingReconciliation performs a deterministic, side-effect-free
// mark pass. Callers are responsible for supplying a complete authoritative
// current snapshot and the desired stored inventory scope before acting on the
// result.
func EvaluateEmbeddingReconciliation(current []ExpectedSectionEmbedding, stored []domain.EmbeddingChunkHash, plan EmbeddingPlan) EmbeddingReconciliation {
	currentByID := make(map[string]ExpectedSectionEmbedding, len(current))
	storedBySection := make(map[string][]domain.EmbeddingChunkHash)
	for _, item := range current {
		currentByID[item.Section.SectionID] = item
	}
	for _, hash := range stored {
		storedBySection[hash.SectionID] = append(storedBySection[hash.SectionID], hash)
	}

	result := EmbeddingReconciliation{}
	currentIDs := sortedExpectationIDs(currentByID)
	for _, sectionID := range currentIDs {
		item := currentByID[sectionID]
		evaluation := EvaluateSectionEmbedding(item.Section, item.Chunks, storedBySection[sectionID], plan)
		result.Sections = append(result.Sections, evaluation)
		switch evaluation.State {
		case SectionEmbeddingReady:
			result.ReadySectionIDs = append(result.ReadySectionIDs, sectionID)
		case SectionEmbeddingPending:
			result.PendingSectionIDs = append(result.PendingSectionIDs, sectionID)
		case SectionEmbeddingStale:
			result.StaleSectionIDs = append(result.StaleSectionIDs, sectionID)
		}
		if evaluation.LegacyPlanChunks > 0 {
			result.LegacyPlanSectionIDs = append(result.LegacyPlanSectionIDs, sectionID)
			result.CurrentLegacyPlanChunkCount += evaluation.LegacyPlanChunks
		}
	}

	orphanSections := map[string]bool{}
	for _, hash := range stored {
		if _, exists := currentByID[hash.SectionID]; exists {
			continue
		}
		orphanSections[hash.SectionID] = true
		result.OrphanChunkCount++
		if embeddingPlanMatchesHash(plan, hash) {
			result.ActivePlanOrphanChunkCount++
		} else {
			result.LegacyPlanOrphanChunkCount++
		}
	}
	result.OrphanSectionIDs = sortedStringSet(orphanSections)
	return result
}

func embeddingPlanMatchesHash(plan EmbeddingPlan, hash domain.EmbeddingChunkHash) bool {
	return hash.Model == strings.TrimSpace(plan.Model) &&
		hash.GeneratorVersion == strings.TrimSpace(plan.GeneratorVersion) &&
		hash.Tokenizer == strings.TrimSpace(plan.Tokenizer) &&
		hash.ChunkStrategy == strings.TrimSpace(plan.ChunkStrategy)
}

func sortedExpectationIDs(values map[string]ExpectedSectionEmbedding) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedStringSet(values map[string]bool) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func groupEmbeddingChunkHashesBySection(existing map[string]domain.EmbeddingChunkHash) map[string][]domain.EmbeddingChunkHash {
	grouped := make(map[string][]domain.EmbeddingChunkHash)
	for _, hash := range existing {
		grouped[hash.SectionID] = append(grouped[hash.SectionID], hash)
	}
	return grouped
}
