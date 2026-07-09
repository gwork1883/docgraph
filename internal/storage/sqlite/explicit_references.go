package sqlite

import (
	"context"
	"database/sql"
	"html"
	"net/url"
	pathpkg "path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/docgraph/docgraph/internal/domain"
)

type explicitReferenceSection struct {
	SectionID   string
	DocumentID  string
	DocumentURL string
	ExternalID  string
	Title       string
	HeadingPath string
	Content     string
	Ordinal     int
}

type explicitReferenceCatalog struct {
	Sections  []explicitReferenceSection
	ByDoc     map[string][]explicitReferenceSection
	DocByPath map[string]string
	DocByURL  map[string]string
}

type rawExplicitReference struct {
	text      string
	href      string
	extractor string
	refKind   string
	start     int
	end       int
	number    string
	title     string
}

var (
	markdownLinkRefRE = regexp.MustCompile(`\[([^\]\n]{1,160})\]\(([^)\s]+)\)`)
	htmlLinkRefRE     = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a>`)
	bareURLRefRE      = regexp.MustCompile(`(?i)\b(?:https?://|file://|/|\.\.?/)[^\s<>()"']*#[^\s<>()"']+`)
	sectionNumberRE   = regexp.MustCompile(`(?i)(?:§\s*|section\s+)([0-9]+(?:\.[0-9]+)*)(?:\s+([^。\n\r.;；,，]{1,80}))?`)
	bareNumberTitleRE = regexp.MustCompile(`(?m)(^|[\s(（])([0-9]+(?:\.[0-9]+)+)\s+([^。\n\r.;；,，]{2,80})`)
	plainTitleRefRE   = regexp.MustCompile(`(?i)(?:详见|参考|参见|见|details\s+see|for\s+details\s+see|see|refer\s+to)\s+([^。\n\r.;；]{2,80})`)
)

func (s *Store) attachExplicitReferences(ctx context.Context, sc *domain.SectionContent) error {
	catalog, err := s.loadExplicitReferenceCatalog(ctx)
	if err != nil {
		return err
	}
	source := explicitReferenceSection{
		SectionID:   sc.SectionID,
		DocumentID:  sc.DocumentID,
		DocumentURL: sc.DocumentURL,
		Title:       sc.Title,
		HeadingPath: sc.HeadingPath,
		Content:     sc.Content,
	}
	sc.ExplicitReferences = extractExplicitReferences(source, catalog)
	return nil
}

func (s *Store) enrichSearchResultExplicitReferences(ctx context.Context, result *domain.SearchResult) error {
	if len(result.Hits) == 0 {
		return nil
	}
	catalog, err := s.loadExplicitReferenceCatalog(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(result.Hits))
	for _, hit := range result.Hits {
		ids = append(ids, hit.SectionID)
	}
	sources, err := s.loadExplicitReferenceSections(ctx, ids)
	if err != nil {
		return err
	}
	byID := make(map[string]explicitReferenceSection, len(sources))
	for _, source := range sources {
		byID[source.SectionID] = source
	}
	for i := range result.Hits {
		source, ok := byID[result.Hits[i].SectionID]
		if !ok {
			continue
		}
		refs := extractExplicitReferences(source, catalog)
		result.Hits[i].HasExplicitReferences = len(refs) > 0
		result.Hits[i].ExplicitReferenceCount = len(refs)
		result.SuggestedReads.ExplicitReferences = append(result.SuggestedReads.ExplicitReferences, refs...)
	}
	if result.SuggestedReads.ImplicitSymbolLinks == nil {
		result.SuggestedReads.ImplicitSymbolLinks = []domain.SuggestedRead{}
	}
	if result.SuggestedReads.CuratedRelations == nil {
		result.SuggestedReads.CuratedRelations = []domain.SuggestedRead{}
	}
	if result.SuggestedReads.StructuralNeighbors == nil {
		result.SuggestedReads.StructuralNeighbors = []domain.SuggestedRead{}
	}
	return nil
}

func (s *Store) loadExplicitReferenceCatalog(ctx context.Context) (explicitReferenceCatalog, error) {
	rows, err := s.readDB().QueryContext(ctx, `
select s.id, s.document_id, d.url, d.external_id, s.title, s.heading_path, '', s.ordinal
from sections s
join documents d on d.id = s.document_id
order by s.document_id asc, s.ordinal asc, s.id asc
`)
	if err != nil {
		return explicitReferenceCatalog{}, err
	}
	defer rows.Close()

	catalog := explicitReferenceCatalog{
		Sections:  []explicitReferenceSection{},
		ByDoc:     map[string][]explicitReferenceSection{},
		DocByPath: map[string]string{},
		DocByURL:  map[string]string{},
	}
	for rows.Next() {
		var section explicitReferenceSection
		if err := rows.Scan(&section.SectionID, &section.DocumentID, &section.DocumentURL, &section.ExternalID, &section.Title, &section.HeadingPath, &section.Content, &section.Ordinal); err != nil {
			return explicitReferenceCatalog{}, err
		}
		catalog.Sections = append(catalog.Sections, section)
		catalog.ByDoc[section.DocumentID] = append(catalog.ByDoc[section.DocumentID], section)
		for _, key := range documentPathKeys(section.DocumentURL, section.ExternalID) {
			if _, exists := catalog.DocByPath[key]; !exists {
				catalog.DocByPath[key] = section.DocumentID
			}
		}
		if section.DocumentURL != "" {
			catalog.DocByURL[strings.ToLower(section.DocumentURL)] = section.DocumentID
		}
	}
	if err := rows.Err(); err != nil {
		return explicitReferenceCatalog{}, err
	}
	return catalog, nil
}

func (s *Store) loadExplicitReferenceSections(ctx context.Context, ids []string) ([]explicitReferenceSection, error) {
	seen := map[string]bool{}
	sections := make([]explicitReferenceSection, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		var section explicitReferenceSection
		err := s.readDB().QueryRowContext(ctx, `
select s.id, s.document_id, d.url, d.external_id, s.title, s.heading_path, s.content, s.ordinal
from sections s
join documents d on d.id = s.document_id
where s.id = ?
`, id).Scan(&section.SectionID, &section.DocumentID, &section.DocumentURL, &section.ExternalID, &section.Title, &section.HeadingPath, &section.Content, &section.Ordinal)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		sections = append(sections, section)
	}
	return sections, nil
}

func extractExplicitReferences(source explicitReferenceSection, catalog explicitReferenceCatalog) []domain.ExplicitReference {
	rawRefs := collectRawExplicitReferences(source.Content)
	refs := make([]domain.ExplicitReference, 0, len(rawRefs))
	seen := map[string]bool{}
	for _, raw := range rawRefs {
		ref := domain.ExplicitReference{
			SourceSectionID: source.SectionID,
			RefText:         strings.TrimSpace(raw.text),
			RefKind:         raw.refKind,
			Extractor:       raw.extractor,
			Confidence:      0.45,
		}
		resolveExplicitReference(&ref, raw, source, catalog)
		key := ref.Extractor + "\x00" + ref.RefText + "\x00" + ref.TargetSectionID + "\x00" + strings.Join(candidateIDs(ref.Candidates), ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ref)
	}
	return refs
}

func collectRawExplicitReferences(content string) []rawExplicitReference {
	refs := make([]rawExplicitReference, 0)
	occupied := make([][2]int, 0)
	for _, m := range markdownLinkRefRE.FindAllStringSubmatchIndex(content, -1) {
		refText := content[m[0]:m[1]]
		href := content[m[4]:m[5]]
		refs = append(refs, rawExplicitReference{
			text:      refText,
			href:      href,
			extractor: "markdown_link_extractor",
			refKind:   linkReferenceKind(href),
			start:     m[0],
			end:       m[1],
		})
		occupied = append(occupied, [2]int{m[0], m[1]})
	}
	for _, m := range htmlLinkRefRE.FindAllStringSubmatchIndex(content, -1) {
		if overlapsAny(m[0], m[1], occupied) {
			continue
		}
		href := html.UnescapeString(content[m[2]:m[3]])
		label := stripHTMLTags(content[m[4]:m[5]])
		refText := strings.TrimSpace(label)
		if refText == "" {
			refText = content[m[0]:m[1]]
		}
		refs = append(refs, rawExplicitReference{
			text:      refText,
			href:      href,
			extractor: "html_link_extractor",
			refKind:   linkReferenceKind(href),
			start:     m[0],
			end:       m[1],
		})
		occupied = append(occupied, [2]int{m[0], m[1]})
	}
	for _, m := range bareURLRefRE.FindAllStringSubmatchIndex(content, -1) {
		if overlapsAny(m[0], m[1], occupied) {
			continue
		}
		href := strings.TrimRight(content[m[0]:m[1]], ".,;。；)")
		refs = append(refs, rawExplicitReference{
			text:      href,
			href:      href,
			extractor: "url_fragment_extractor",
			refKind:   linkReferenceKind(href),
			start:     m[0],
			end:       m[1],
		})
		occupied = append(occupied, [2]int{m[0], m[1]})
	}
	for _, m := range sectionNumberRE.FindAllStringSubmatchIndex(content, -1) {
		if overlapsAny(m[0], m[1], occupied) {
			continue
		}
		title := ""
		if m[4] >= 0 {
			title = content[m[4]:m[5]]
		}
		refs = append(refs, rawExplicitReference{
			text:      strings.TrimSpace(content[m[0]:m[1]]),
			extractor: "heading_number_reference_extractor",
			refKind:   "heading_number",
			start:     m[0],
			end:       m[1],
			number:    content[m[2]:m[3]],
			title:     title,
		})
		occupied = append(occupied, [2]int{m[0], m[1]})
	}
	for _, m := range bareNumberTitleRE.FindAllStringSubmatchIndex(content, -1) {
		start := m[2]
		end := m[5]
		if overlapsAny(start, end, occupied) {
			continue
		}
		refs = append(refs, rawExplicitReference{
			text:      strings.TrimSpace(content[start:end]),
			extractor: "heading_number_reference_extractor",
			refKind:   "heading_number",
			start:     start,
			end:       end,
			number:    content[m[2]:m[3]],
			title:     content[m[4]:m[5]],
		})
		occupied = append(occupied, [2]int{start, end})
	}
	for _, m := range plainTitleRefRE.FindAllStringSubmatchIndex(content, -1) {
		if overlapsAny(m[0], m[1], occupied) {
			continue
		}
		title := strings.TrimSpace(content[m[2]:m[3]])
		if title == "" || looksLikeOnlyNumber(title) {
			continue
		}
		refs = append(refs, rawExplicitReference{
			text:      strings.TrimSpace(content[m[0]:m[1]]),
			extractor: "plain_title_reference_extractor",
			refKind:   "plain_title",
			start:     m[0],
			end:       m[1],
			title:     title,
		})
	}
	sort.SliceStable(refs, func(i, j int) bool {
		return refs[i].start < refs[j].start
	})
	return refs
}

func resolveExplicitReference(ref *domain.ExplicitReference, raw rawExplicitReference, source explicitReferenceSection, catalog explicitReferenceCatalog) {
	switch raw.extractor {
	case "markdown_link_extractor", "html_link_extractor", "url_fragment_extractor":
		resolveLinkReference(ref, raw.href, source, catalog)
	case "heading_number_reference_extractor":
		candidates := resolveHeadingNumber(raw.number, raw.title, source.DocumentID, catalog)
		fillResolvedReference(ref, candidates, 0.88)
	case "plain_title_reference_extractor":
		candidates := resolvePlainTitle(raw.title, source.DocumentID, catalog)
		fillResolvedReference(ref, candidates, 0.76)
	}
}

func resolveLinkReference(ref *domain.ExplicitReference, href string, source explicitReferenceSection, catalog explicitReferenceCatalog) {
	href = strings.TrimSpace(href)
	if href == "" {
		return
	}
	ref.TargetURL = href
	targetDocID := source.DocumentID
	fragment := ""

	if strings.HasPrefix(href, "#") {
		fragment = strings.TrimPrefix(href, "#")
	} else if u, err := url.Parse(href); err == nil {
		fragment = u.Fragment
		docID := resolveLinkedDocumentID(href, u, source, catalog)
		if docID != "" {
			targetDocID = docID
		}
	}
	if fragment == "" {
		if targetDocID != "" && targetDocID != source.DocumentID {
			ref.TargetDocumentID = targetDocID
			ref.Resolved = true
			ref.Confidence = 0.72
		}
		return
	}
	candidates := resolveFragment(fragment, targetDocID, catalog)
	fillResolvedReference(ref, candidates, 0.96)
	if ref.TargetURL == "" {
		ref.TargetURL = href
	}
}

func resolveLinkedDocumentID(href string, u *url.URL, source explicitReferenceSection, catalog explicitReferenceCatalog) string {
	if u.IsAbs() {
		key := strings.ToLower(stripURLFragment(href))
		if docID := catalog.DocByURL[key]; docID != "" {
			return docID
		}
		return ""
	}
	pathValue := u.Path
	if pathValue == "" {
		return source.DocumentID
	}
	keys := []string{normalizeDocPath(pathValue)}
	for _, base := range documentPathKeys(source.DocumentURL, source.ExternalID) {
		if strings.HasPrefix(pathValue, "/") {
			keys = append(keys, normalizeDocPath(pathValue))
			continue
		}
		keys = append(keys, normalizeDocPath(pathpkg.Join(pathpkg.Dir(base), pathValue)))
	}
	for _, key := range keys {
		if docID := catalog.DocByPath[key]; docID != "" {
			return docID
		}
	}
	return ""
}

func resolveFragment(fragment string, documentID string, catalog explicitReferenceCatalog) []domain.ExplicitReferenceCandidate {
	needle := normalizeAnchor(fragment)
	candidates := make([]domain.ExplicitReferenceCandidate, 0)
	for _, section := range catalog.ByDoc[documentID] {
		for _, anchor := range sectionAnchors(section) {
			if anchor == needle {
				candidates = append(candidates, candidateFromSection(section, 0.96))
				break
			}
		}
	}
	return candidates
}

func resolveHeadingNumber(number string, titleHint string, documentID string, catalog explicitReferenceCatalog) []domain.ExplicitReferenceCandidate {
	number = strings.TrimSpace(number)
	titleHint = normalizeText(titleHint)
	matches := make([]explicitReferenceSection, 0)
	for _, section := range catalog.ByDoc[documentID] {
		if !sectionHeadingStartsWithNumber(section, number) {
			continue
		}
		if titleHint != "" && !strings.Contains(normalizeText(sectionTitleText(section)), titleHint) && !strings.Contains(titleHint, normalizeText(section.Title)) {
			continue
		}
		matches = append(matches, section)
	}
	if len(matches) == 0 && titleHint != "" {
		return resolvePlainTitle(titleHint, documentID, catalog)
	}
	return candidatesFromSections(matches, 0.84)
}

func resolvePlainTitle(title string, documentID string, catalog explicitReferenceCatalog) []domain.ExplicitReferenceCandidate {
	needle := normalizeReferenceTitle(title)
	if needle == "" {
		return nil
	}
	matches := make([]explicitReferenceSection, 0)
	for _, section := range catalog.ByDoc[documentID] {
		heading := normalizeReferenceTitle(sectionTitleText(section))
		titleOnly := normalizeReferenceTitle(section.Title)
		if heading == needle || titleOnly == needle || strings.Contains(heading, needle) || strings.Contains(needle, titleOnly) {
			matches = append(matches, section)
		}
	}
	return candidatesFromSections(matches, 0.72)
}

func fillResolvedReference(ref *domain.ExplicitReference, candidates []domain.ExplicitReferenceCandidate, confidence float64) {
	if len(candidates) == 1 {
		c := candidates[0]
		ref.TargetSectionID = c.TargetSectionID
		ref.TargetDocumentID = c.TargetDocumentID
		ref.TargetURL = c.TargetURL
		ref.TargetHeadingPath = c.TargetHeadingPath
		ref.Resolved = true
		ref.Confidence = confidence
		return
	}
	ref.Resolved = false
	ref.Confidence = 0.42
	ref.Candidates = candidates
}

func candidatesFromSections(sections []explicitReferenceSection, confidence float64) []domain.ExplicitReferenceCandidate {
	candidates := make([]domain.ExplicitReferenceCandidate, 0, len(sections))
	for _, section := range sections {
		candidates = append(candidates, candidateFromSection(section, confidence))
	}
	return candidates
}

func candidateFromSection(section explicitReferenceSection, confidence float64) domain.ExplicitReferenceCandidate {
	return domain.ExplicitReferenceCandidate{
		TargetSectionID:   section.SectionID,
		TargetDocumentID:  section.DocumentID,
		TargetURL:         section.DocumentURL,
		TargetHeadingPath: section.HeadingPath,
		Confidence:        confidence,
	}
}

func candidateIDs(candidates []domain.ExplicitReferenceCandidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.TargetSectionID)
	}
	sort.Strings(ids)
	return ids
}

func linkReferenceKind(href string) string {
	if strings.HasPrefix(strings.TrimSpace(href), "#") {
		return "section_anchor"
	}
	if u, err := url.Parse(href); err == nil && u.Fragment != "" {
		return "section_anchor"
	}
	return "link"
}

func sectionHeadingStartsWithNumber(section explicitReferenceSection, number string) bool {
	for _, text := range []string{section.Title, lastHeadingSegment(section.HeadingPath), section.HeadingPath} {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, number+" ") || strings.HasPrefix(text, number+". ") || strings.HasPrefix(text, number+"、") || strings.HasPrefix(text, number+" ") {
			return true
		}
		if strings.Contains(text, "> "+number+" ") || strings.Contains(text, "> "+number+". ") || strings.Contains(text, " > "+number+"、") {
			return true
		}
	}
	return false
}

func sectionAnchors(section explicitReferenceSection) []string {
	texts := []string{section.Title, lastHeadingSegment(section.HeadingPath), section.HeadingPath}
	anchors := make([]string, 0, len(texts))
	for _, text := range texts {
		anchor := normalizeAnchor(text)
		if anchor != "" {
			anchors = append(anchors, anchor)
		}
	}
	return anchors
}

func sectionTitleText(section explicitReferenceSection) string {
	if section.HeadingPath != "" {
		return lastHeadingSegment(section.HeadingPath)
	}
	return section.Title
}

func lastHeadingSegment(headingPath string) string {
	parts := strings.Split(headingPath, ">")
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if part != "" {
			return part
		}
	}
	return strings.TrimSpace(headingPath)
}

func normalizeAnchor(text string) string {
	text = strings.ToLower(html.UnescapeString(strings.TrimSpace(text)))
	var b strings.Builder
	lastDash := false
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func normalizeText(text string) string {
	text = strings.ToLower(html.UnescapeString(strings.TrimSpace(text)))
	var b strings.Builder
	lastSpace := false
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func normalizeReferenceTitle(text string) string {
	text = normalizeText(text)
	text = strings.TrimSpace(sectionNumberRE.ReplaceAllString(text, "$2"))
	text = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*\s+`).ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func stripHTMLTags(text string) string {
	tagRE := regexp.MustCompile(`(?s)<[^>]+>`)
	return strings.TrimSpace(html.UnescapeString(tagRE.ReplaceAllString(text, "")))
}

func overlapsAny(start, end int, spans [][2]int) bool {
	for _, span := range spans {
		if start < span[1] && end > span[0] {
			return true
		}
	}
	return false
}

func looksLikeOnlyNumber(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	for _, r := range text {
		if !(unicode.IsDigit(r) || r == '.' || unicode.IsSpace(r)) {
			return false
		}
	}
	return true
}

func stripURLFragment(raw string) string {
	if idx := strings.Index(raw, "#"); idx >= 0 {
		return raw[:idx]
	}
	return raw
}

func documentPathKeys(documentURL string, externalID string) []string {
	keys := make([]string, 0, 4)
	if externalID != "" {
		keys = append(keys, normalizeDocPath(externalID))
	}
	if documentURL != "" {
		if u, err := url.Parse(documentURL); err == nil {
			if u.Path != "" {
				keys = append(keys, normalizeDocPath(u.Path))
			}
		}
		keys = append(keys, normalizeDocPath(documentURL))
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, key)
	}
	return result
}

func normalizeDocPath(value string) string {
	value = strings.TrimSpace(value)
	value = stripURLFragment(value)
	if value == "" {
		return ""
	}
	if u, err := url.Parse(value); err == nil && u.Path != "" {
		value = u.Path
	}
	value = strings.ReplaceAll(value, "\\", "/")
	value = pathpkg.Clean(value)
	value = strings.TrimPrefix(value, "/")
	if value == "." {
		return ""
	}
	return strings.ToLower(value)
}
