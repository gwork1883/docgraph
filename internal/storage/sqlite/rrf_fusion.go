package sqlite

import (
	"sort"

	"github.com/docgraph/docgraph/internal/domain"
)

// RankedHit represents a search hit with its rank position and original score.
type RankedHit struct {
	SectionID     string
	Hit           domain.SearchHit
	RawScore      float64  // text lane: ScoreBreakdown.Total; vector lane: similarity
	RawSimilarity float64  // vector lane: original cosine similarity (0 if text lane)
	Source        []string // evidence sources: ["text_unicode", "text_trigram", "vector", "entity_exact", ...]
}

// assignRanks sorts hits by their score (descending) and assigns consecutive natural-number ranks (1, 2, 3...).
// scoreFn extracts the score used for ranking.
// sourceFn extracts evidence source labels.
func assignRanks(hits []domain.SearchHit, scoreFn func(domain.SearchHit) float64, sourceFn func(domain.SearchHit) []string) []RankedHit {
	type scored struct {
		hit   domain.SearchHit
		score float64
	}
	sorted := make([]scored, len(hits))
	for i, h := range hits {
		sorted[i] = scored{hit: h, score: scoreFn(h)}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].score > sorted[j].score
	})

	ranked := make([]RankedHit, len(sorted))
	for i, s := range sorted {
		var rawSim float64
		if s.hit.ScoreBreakdown != nil {
			rawSim = s.hit.ScoreBreakdown.VectorBoost // stores raw similarity in RRF mode
		}
		ranked[i] = RankedHit{
			SectionID:     s.hit.SectionID,
			Hit:           s.hit,
			RawScore:      s.score,
			RawSimilarity: rawSim,
			Source:        sourceFn(s.hit),
		}
	}
	return ranked
}

// weightedRRFFusion implements the Weighted Reciprocal Rank Fusion algorithm.
// It takes text-lane and vector-lane ranked hits, applies dynamic weights,
// and returns a map of SectionID → RRFContribution with transparent scoring metadata.
//
// Formula: Score(d) = w_text * 1/(k + r_text(d)) + w_vector * 1/(k + r_vector(d))
func weightedRRFFusion(textRanked, vectorRanked []RankedHit, wText, wVector, k float64) map[string]*domain.RRFContribution {
	result := map[string]*domain.RRFContribution{}

	// Text lane: accumulate w_text * 1/(k + r_text)
	for i, rh := range textRanked {
		rText := i + 1
		textRRF := wText / (k + float64(rText))
		if existing, ok := result[rh.SectionID]; ok {
			existing.TextRank = rText
			existing.TextRRF = textRRF
			existing.RRFScore += textRRF
			if rh.RawScore > existing.RawBM25Score {
				existing.RawBM25Score = rh.RawScore
			}
			existing.SourceEvidence = uniqueStrings(append(existing.SourceEvidence, rh.Source...))
		} else {
			result[rh.SectionID] = &domain.RRFContribution{
				TextRank:       rText,
				TextRRF:        textRRF,
				RRFScore:       textRRF,
				RawBM25Score:   rh.RawScore,
				SourceEvidence: rh.Source,
			}
		}
	}

	// Vector lane: accumulate w_vector * 1/(k + r_vector)
	for i, rh := range vectorRanked {
		rVector := i + 1
		vectorRRF := wVector / (k + float64(rVector))
		if existing, ok := result[rh.SectionID]; ok {
			existing.VectorRank = rVector
			existing.VectorRRF = vectorRRF
			existing.RRFScore += vectorRRF
			if rh.RawSimilarity > existing.RawSimilarity {
				existing.RawSimilarity = rh.RawSimilarity
			}
			existing.SourceEvidence = uniqueStrings(append(existing.SourceEvidence, "vector"))
		} else {
			sources := []string{"vector"}
			if len(rh.Source) > 0 {
				sources = rh.Source
			}
			result[rh.SectionID] = &domain.RRFContribution{
				VectorRank:     rVector,
				VectorRRF:      vectorRRF,
				RRFScore:       vectorRRF,
				RawSimilarity:  rh.RawSimilarity,
				SourceEvidence: sources,
			}
		}
	}

	return result
}

// applyMultiplierCalibration applies business multiplier adjustments after RRF fusion.
// Canonical documents get ×1.2 boost; stale embeddings get ×0.85 penalty.
func applyMultiplierCalibration(contribs map[string]*domain.RRFContribution, hitsBySection map[string]*domain.SearchHit) {
	for sectionID, contrib := range contribs {
		multiplier := 1.0
		hit := hitsBySection[sectionID]
		if hit != nil {
			if hit.Canonical {
				multiplier *= 1.05
			}
			// Stale embedding penalty: vector trace invalid
			if hit.Trace != nil && !hit.Trace.VectorTraceValid {
				multiplier *= 0.85
			}
		}
		contrib.Multiplier = multiplier
		contrib.FinalScore = contrib.RRFScore * multiplier
	}
}

// buildFinalHits combines RRF contributions with full SearchHit data, sorts by FinalScore,
// and applies per-document section caps.
func buildFinalHits(
	contribs map[string]*domain.RRFContribution,
	textHitsBySection map[string]*domain.SearchHit,
	vectorHitsBySection map[string]*domain.SearchHit,
	limit int,
	maxSectionsPerDocument int,
) []domain.SearchHit {
	type final struct {
		sectionID string
		contrib   *domain.RRFContribution
	}
	all := make([]final, 0, len(contribs))
	for sectionID, contrib := range contribs {
		all = append(all, final{sectionID: sectionID, contrib: contrib})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].contrib.FinalScore == all[j].contrib.FinalScore {
			// Tie-break: prefer hits with more evidence sources
			if len(all[i].contrib.SourceEvidence) != len(all[j].contrib.SourceEvidence) {
				return len(all[i].contrib.SourceEvidence) > len(all[j].contrib.SourceEvidence)
			}
			// Final tie-break: canonical first
			hitI := mergeHitForSection(all[i].sectionID, textHitsBySection, vectorHitsBySection)
			hitJ := mergeHitForSection(all[j].sectionID, textHitsBySection, vectorHitsBySection)
			if hitI.Canonical != hitJ.Canonical {
				return hitI.Canonical
			}
			return hitI.DocumentTitle < hitJ.DocumentTitle
		}
		return all[i].contrib.FinalScore > all[j].contrib.FinalScore
	})

	result := make([]domain.SearchHit, 0, minInt(limit, len(all)))
	perDoc := map[string]int{}
	for _, entry := range all {
		if len(result) >= limit {
			break
		}
		if perDoc[entry.contrib.DocumentID] >= maxSectionsPerDocument {
			continue
		}
		hit := mergeHitForSection(entry.sectionID, textHitsBySection, vectorHitsBySection)
		hit.Rank = entry.contrib.FinalScore
		hit.RRFContribution = entry.contrib

		// Set evidence level based on RRF contribution
		if hit.EvidenceLevel == "" {
			hit.EvidenceLevel = evidenceLevelForRRFContribution(*entry.contrib)
		}

		perDoc[entry.contrib.DocumentID]++
		result = append(result, hit)
	}
	return result
}

// mergeHitForSection creates a complete SearchHit by combining text and vector lane data.
// Text lane provides the base hit; vector lane enriches with trace, snippet, and similarity.
func mergeHitForSection(sectionID string, textHitsBySection, vectorHitsBySection map[string]*domain.SearchHit) domain.SearchHit {
	// Prefer text lane hit as base (it has all the text data)
	textHit := textHitsBySection[sectionID]
	vectorHit := vectorHitsBySection[sectionID]

	if textHit != nil && vectorHit != nil {
		// Merge: text hit is base, vector hit enriches
		hit := *textHit
		// Keep vector trace if available (more detailed than text trace)
		if vectorHit.Trace != nil {
			hit.Trace = vectorHit.Trace
		}
		// Keep vector snippet if it's better (shows chunk context)
		if vectorHit.Snippet != "" && (hit.Snippet == "" || len(vectorHit.Snippet) > len(hit.Snippet)) {
			hit.Snippet = vectorHit.Snippet
		}
		// Merge matched entities
		hit.MatchedEntities = mergeMatchedEntities(hit.MatchedEntities, vectorHit.MatchedEntities)
		// Merge ScoreBreakdown: text lane breakdown explains text evidence, vector explains semantic
		if hit.ScoreBreakdown != nil && vectorHit.ScoreBreakdown != nil {
			// Keep text breakdown as primary; add VectorBoost from vector
			hit.ScoreBreakdown.VectorBoost = vectorHit.ScoreBreakdown.VectorBoost // raw similarity
		}
		// Merge QueryMatch
		if hit.QueryMatch != nil && vectorHit.QueryMatch != nil {
			mergeQueryMatch(hit.QueryMatch, vectorHit.QueryMatch, "vector")
		} else if hit.QueryMatch == nil && vectorHit.QueryMatch != nil {
			hit.QueryMatch = vectorHit.QueryMatch
		}
		return hit
	}

	if textHit != nil {
		return *textHit
	}
	if vectorHit != nil {
		return *vectorHit
	}

	// Should not happen - at least one lane must have the hit
	return domain.SearchHit{SectionID: sectionID}
}

// evidenceLevelForRRFContribution determines evidence level from RRF contribution data.
func evidenceLevelForRRFContribution(contrib domain.RRFContribution) string {
	hasText := contrib.TextRank > 0
	hasVector := contrib.VectorRank > 0

	if hasText && hasVector {
		return "strong_hybrid"
	}
	if hasVector && !hasText {
		return "weak_vector_only"
	}
	if hasText {
		// Text-only: check if it was an exact/entity match from source evidence
		for _, src := range contrib.SourceEvidence {
			if src == "entity_exact" || src == "entity_normalized" {
				return "strong_exact"
			}
		}
		return "medium_semantic"
	}
	return "medium_semantic"
}

// minInt is declared in sqlite.go; do not redeclare here.
