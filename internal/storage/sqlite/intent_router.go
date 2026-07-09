package sqlite

import (
	"strings"

	"github.com/docgraph/docgraph/internal/vectorstore"
)

// IntentRouteResult is the output of the lightweight intent router.
type IntentRouteResult struct {
	Route   string  // "entity", "conceptual", "general"
	WText   float64 // text lane RRF weight
	WVector float64 // vector lane RRF weight
}

// RouteIntent classifies the query intent using pure heuristic rules (regex + token analysis)
// and returns the corresponding RRF weights. Execution time < 1ms.
// When vector search is disabled, callers should set WVector = 0 after calling this function.
func RouteIntent(query string, semanticIntents []string, weights vectorstore.IntentRouteWeights) IntentRouteResult {
	// Apply defaults if weights are not configured (zero-value struct)
	if weights.EntityText == 0 && weights.EntityVector == 0 &&
		weights.ConceptualText == 0 && weights.ConceptualVector == 0 &&
		weights.GeneralText == 0 && weights.GeneralVector == 0 {
		weights = vectorstore.IntentRouteWeights{
			EntityText:       0.8,
			EntityVector:     0.2,
			ConceptualText:   0.3,
			ConceptualVector: 0.7,
			GeneralText:      0.5,
			GeneralVector:    0.5,
			RRFK:             60,
		}
	}

	q := strings.TrimSpace(query)
	if q == "" {
		return IntentRouteResult{Route: "general", WText: weights.GeneralText, WVector: weights.GeneralVector}
	}

	// Rule A: entity/error-code queries (heavy text)
	if isEntityQuery(q) {
		return IntentRouteResult{Route: "entity", WText: weights.EntityText, WVector: weights.EntityVector}
	}

	// Rule B: conceptual/question queries (heavy vector)
	if isConceptualQuery(q) || len(semanticIntents) > 0 {
		return IntentRouteResult{Route: "conceptual", WText: weights.ConceptualText, WVector: weights.ConceptualVector}
	}

	// Rule C: general (balanced)
	return IntentRouteResult{Route: "general", WText: weights.GeneralText, WVector: weights.GeneralVector}
}

// isEntityQuery detects queries that are likely entity/error-code lookups:
// - short queries (<5 chars)
// - contains special symbols like _, -, /, ., (), backslash
// - pure uppercase English words
// - contains known dev keywords (GET, POST, PUT, DELETE, PATCH, NullPointerException, ERR_, etc.)
// - entity extraction finds typed entities
func isEntityQuery(q string) bool {
	// Very short queries are likely entity lookups
	if len(q) < 5 {
		return true
	}

	// Pure uppercase English (API names, error codes, config keys)
	upperOnly := true
	hasLetter := false
	for _, ch := range q {
		if ch >= 'a' && ch <= 'z' {
			upperOnly = false
			break
		}
		if ch >= 'A' && ch <= 'Z' {
			hasLetter = true
		}
	}
	if upperOnly && hasLetter {
		return true
	}

	// Dense special symbols: _, -, /, ., (), \, :, #
	symbolCount := 0
	for _, ch := range q {
		if ch == '_' || ch == '-' || ch == '/' || ch == '.' || ch == '(' || ch == ')' || ch == '\\' || ch == ':' || ch == '#' || ch == '$' || ch == '@' {
			symbolCount++
		}
	}
	// If symbols are dense relative to length, likely an entity query
	if symbolCount >= 2 && float64(symbolCount)/float64(len(q)) > 0.15 {
		return true
	}

	// Known dev keywords
	devKeywords := []string{
		"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS",
		"NULL", "NullPointerException", "ERR_", "ERROR_",
		"HTTP", "HTTPS", "API", "URL", "URI",
		"NULL", "TRUE", "FALSE",
	}
	for _, kw := range devKeywords {
		if strings.Contains(q, kw) {
			return true
		}
	}

	// Has entities extracted (API paths, methods, error codes)
	entities := extractQueryEntities(q)
	if len(entities) > 0 {
		return true
	}

	return false
}

// isConceptualQuery detects queries that are likely conceptual/question lookups:
// - contains question words (怎么/如何/为什么/what/how/why/explain/describe)
// - longer queries (>12 chars) tend to be descriptive
// - Chinese question patterns
func isConceptualQuery(q string) bool {
	// Chinese question words
	chineseQuestions := []string{"怎么", "如何", "为什么", "为什么", "怎样", "啥是", "什么是", "哪个", "哪样"}
	for _, w := range chineseQuestions {
		if strings.Contains(q, w) {
			return true
		}
	}

	// English question words (must be standalone or at start)
	qLower := strings.ToLower(q)
	englishQuestions := []string{"how", "why", "what", "when", "where", "which", "explain", "describe", "compare", "difference"}
	for _, w := range englishQuestions {
		if strings.HasPrefix(qLower, w+" ") || strings.Contains(qLower, " "+w+" ") || strings.HasPrefix(qLower, w) {
			return true
		}
	}

	// Longer queries (>12 chars) tend to be conceptual/descriptive
	if len(q) > 12 {
		return true
	}

	return false
}
