package profile

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/docgraph/docgraph/internal/domain"
)

type RetrievalProfile struct {
	TopTerms            []domain.ProfileTerm  `json:"top_terms"`
	TopTags             []string              `json:"top_tags"`
	Keyphrases          []string              `json:"keyphrases"`
	Aliases             []string              `json:"aliases"`
	APIRefs             []string              `json:"api_refs"`
	SectionDistribution []SectionDistribution `json:"section_distribution"`
	Stats               RetrievalProfileStats `json:"stats"`
}

type SectionDistribution struct {
	SectionID string   `json:"section_id"`
	Title     string   `json:"title"`
	Terms     []string `json:"terms"`
	TermCount int      `json:"term_count"`
}

type RetrievalProfileStats struct {
	TokenCount      int `json:"token_count"`
	SectionCount    int `json:"section_count"`
	UniqueTermCount int `json:"unique_term_count"`
}

type termStats struct {
	tf          int
	sections    map[string]bool
	headingHits int
	titleHits   int
	score       float64
}

func BuildJSON(doc domain.DocumentInput, sections []domain.SectionInput) (string, error) {
	profile := Build(doc, sections)
	data, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func Build(doc domain.DocumentInput, sections []domain.SectionInput) RetrievalProfile {
	stats := map[string]*termStats{}
	apiRefs := map[string]bool{}
	tokenCount := 0

	titleTerms := termSet(termsFromText(doc.Title, true))
	for _, section := range sections {
		sectionText := strings.Join([]string{section.Title, section.HeadingPath, section.Content}, "\n")
		terms := termsFromText(sectionText, true)
		tokenCount += len(terms)
		sectionSeen := map[string]bool{}
		for _, term := range terms {
			if term == "" {
				continue
			}
			stat := ensureTerm(stats, term)
			stat.tf++
			stat.sections[section.ID] = true
			sectionSeen[term] = true
		}
		for _, term := range termsFromText(section.Title+" "+section.HeadingPath, true) {
			ensureTerm(stats, term).headingHits++
		}
		for _, ref := range apiRefsFromText(sectionText) {
			apiRefs[ref] = true
			stat := ensureTerm(stats, strings.ToLower(ref))
			stat.tf++
			stat.sections[section.ID] = true
			sectionSeen[strings.ToLower(ref)] = true
		}
		_ = sectionSeen
	}
	for term := range titleTerms {
		ensureTerm(stats, term).titleHits++
	}
	for term, stat := range stats {
		stat.score = float64(stat.tf) + float64(len(stat.sections))*0.8 + float64(stat.headingHits)*2.2 + float64(stat.titleHits)*3.5
		if looksImportant(term) {
			stat.score += 1.5
		}
	}

	topTerms := sortedTerms(stats, 40)
	topTags := topTags(topTerms, 12)
	keyphrases := keyphrases(topTerms, 20)
	apiRefList := sortedStringKeys(apiRefs, 40)
	distribution := sectionDistribution(sections, stats)

	return RetrievalProfile{
		TopTerms:            topTerms,
		TopTags:             topTags,
		Keyphrases:          keyphrases,
		Aliases:             aliases(doc, apiRefList),
		APIRefs:             apiRefList,
		SectionDistribution: distribution,
		Stats: RetrievalProfileStats{
			TokenCount:      tokenCount,
			SectionCount:    len(sections),
			UniqueTermCount: len(stats),
		},
	}
}

func ensureTerm(stats map[string]*termStats, term string) *termStats {
	term = strings.TrimSpace(term)
	stat, ok := stats[term]
	if !ok {
		stat = &termStats{sections: map[string]bool{}}
		stats[term] = stat
	}
	return stat
}

func termsFromText(text string, includeChineseSingles bool) []string {
	text = strings.ToLower(text)
	terms := make([]string, 0)
	terms = append(terms, asciiAndAPITokens(text)...)
	if includeChineseSingles {
		terms = append(terms, chineseNgrams(text, 1, 4)...)
	} else {
		terms = append(terms, chineseNgrams(text, 2, 4)...)
	}
	return terms
}

func asciiAndAPITokens(text string) []string {
	terms := make([]string, 0)
	var token strings.Builder
	flush := func() {
		if token.Len() == 0 {
			return
		}
		value := strings.Trim(token.String(), `"'.,;:!?()[]{}<>`)
		token.Reset()
		if len(value) < 2 && !strings.Contains(value, "/") {
			return
		}
		terms = append(terms, value)
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/_.-", r) {
			token.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return terms
}

func chineseNgrams(text string, minN int, maxN int) []string {
	terms := make([]string, 0)
	for _, run := range hanRuns(text) {
		for n := minN; n <= maxN; n++ {
			if len(run) < n {
				continue
			}
			for i := 0; i+n <= len(run); i++ {
				terms = append(terms, string(run[i:i+n]))
			}
		}
	}
	return terms
}

func hanRuns(text string) [][]rune {
	var runs [][]rune
	var current []rune
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			current = append(current, r)
			continue
		}
		if len(current) > 0 {
			runs = append(runs, current)
			current = nil
		}
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	return runs
}

func apiRefsFromText(text string) []string {
	raw := asciiAndAPITokens(strings.ToLower(text))
	refs := make([]string, 0)
	for i, token := range raw {
		upper := strings.ToUpper(token)
		if isHTTPMethod(upper) && i+1 < len(raw) && strings.HasPrefix(raw[i+1], "/") {
			refs = append(refs, upper+" "+raw[i+1])
			continue
		}
		if strings.HasPrefix(token, "/") || isStatusCode(token) || strings.Contains(token, "error_code") {
			refs = append(refs, token)
		}
	}
	return unique(refs)
}

func isHTTPMethod(value string) bool {
	switch value {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	default:
		return false
	}
}

func isStatusCode(value string) bool {
	if len(value) != 3 {
		return false
	}
	if value[0] < '1' || value[0] > '5' {
		return false
	}
	for _, r := range value {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func sortedTerms(stats map[string]*termStats, limit int) []domain.ProfileTerm {
	terms := make([]domain.ProfileTerm, 0, len(stats))
	for term, stat := range stats {
		if shouldSkipTerm(term) {
			continue
		}
		terms = append(terms, domain.ProfileTerm{
			Term:        term,
			TF:          stat.tf,
			Sections:    len(stat.sections),
			HeadingHits: stat.headingHits,
			TitleHits:   stat.titleHits,
			Score:       math.Round(stat.score*10) / 10,
		})
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].Score == terms[j].Score {
			return terms[i].Term < terms[j].Term
		}
		return terms[i].Score > terms[j].Score
	})
	if len(terms) > limit {
		return terms[:limit]
	}
	return terms
}

func topTags(terms []domain.ProfileTerm, limit int) []string {
	tags := make([]string, 0, limit)
	for _, term := range terms {
		if len(tags) >= limit {
			break
		}
		runes := []rune(term.Term)
		if len(runes) > 12 || strings.Contains(term.Term, "/") {
			continue
		}
		tags = append(tags, term.Term)
	}
	return unique(tags)
}

func keyphrases(terms []domain.ProfileTerm, limit int) []string {
	values := make([]string, 0, limit)
	for _, term := range terms {
		if len(values) >= limit {
			break
		}
		if len([]rune(term.Term)) >= 2 {
			values = append(values, term.Term)
		}
	}
	return unique(values)
}

func aliases(doc domain.DocumentInput, apiRefs []string) []string {
	values := []string{strings.TrimSpace(doc.Title), strings.TrimSpace(doc.ExternalID), strings.TrimSpace(doc.URL)}
	values = append(values, apiRefs...)
	return unique(nonEmpty(values))
}

func sectionDistribution(sections []domain.SectionInput, stats map[string]*termStats) []SectionDistribution {
	distribution := make([]SectionDistribution, 0, len(sections))
	for _, section := range sections {
		terms := termsFromText(section.Title+" "+section.HeadingPath+" "+section.Content, false)
		counts := map[string]int{}
		for _, term := range terms {
			if shouldSkipTerm(term) {
				continue
			}
			counts[term]++
		}
		top := make([]domain.ProfileTerm, 0, len(counts))
		for term, count := range counts {
			stat := stats[term]
			score := float64(count)
			if stat != nil {
				score += stat.score / 10
			}
			top = append(top, domain.ProfileTerm{Term: term, TF: count, Score: score})
		}
		sort.Slice(top, func(i, j int) bool {
			if top[i].Score == top[j].Score {
				return top[i].Term < top[j].Term
			}
			return top[i].Score > top[j].Score
		})
		sectionTerms := make([]string, 0, 8)
		for _, term := range top {
			if len(sectionTerms) >= 8 {
				break
			}
			sectionTerms = append(sectionTerms, term.Term)
		}
		distribution = append(distribution, SectionDistribution{
			SectionID: section.ID,
			Title:     section.Title,
			Terms:     sectionTerms,
			TermCount: len(terms),
		})
	}
	return distribution
}

func shouldSkipTerm(term string) bool {
	term = strings.TrimSpace(term)
	if term == "" {
		return true
	}
	if len([]rune(term)) == 1 && !hasHan(term) {
		return true
	}
	return false
}

func looksImportant(term string) bool {
	lower := strings.ToLower(term)
	keywords := []string{"error", "错误", "响应", "权限", "auth", "token", "401", "403", "500", "api", "接口", "code"}
	for _, keyword := range keywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return strings.Contains(lower, "/")
}

func termSet(terms []string) map[string]bool {
	set := map[string]bool{}
	for _, term := range terms {
		if !shouldSkipTerm(term) {
			set[term] = true
		}
	}
	return set
}

func sortedStringKeys(values map[string]bool, limit int) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) > limit {
		return result[:limit]
	}
	return result
}

func hasHan(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func unique(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func nonEmpty(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
