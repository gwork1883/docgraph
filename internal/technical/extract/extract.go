package extract

import (
	"regexp"
	"sort"
	"strings"
)

type EntityKind string

const (
	EntityAPIEndpoint        EntityKind = "api_endpoint"
	EntityPathLiteral        EntityKind = "path_literal"
	EntityOperationCandidate EntityKind = "operation_candidate"
)

type SourceKind string

const (
	SourceOpenAPI SourceKind = "openapi"
	SourceText    SourceKind = "text"
	SourceTable   SourceKind = "table"
	SourceHeading SourceKind = "heading"
	SourceCode    SourceKind = "code"
)

type EntityCandidate struct {
	Kind       EntityKind
	Raw        string
	Canonical  string
	Method     string
	Path       string
	Operation  string
	Source     SourceKind
	Confidence float64
	Evidence   string
	SectionID  string
	DocumentID string
	SpanStart  int
	SpanEnd    int
	Notes      []string
}

type SectionInput struct {
	DocumentID  string
	SectionID   string
	Title       string
	HeadingPath string
	Content     string
}

type OpenAPIEndpointInput struct {
	DocumentID string
	SectionID  string
	Method     string
	Path       string
	Operation  string
}

type pathCandidate struct {
	Raw       string
	Canonical string
	Start     int
	End       int
}

type methodCandidate struct {
	Raw    string
	Method string
	Start  int
	End    int
}

var operationPattern = regexp.MustCompile(`\b[A-Z][A-Za-z0-9]*[a-z][A-Za-z0-9]*[A-Z][A-Za-z0-9]*\b`)

func ExtractSection(input SectionInput) []EntityCandidate {
	collector := entityCollector{}
	collector.add(extractText(input, input.Title, SourceHeading, 0)...)
	collector.add(extractText(input, input.HeadingPath, SourceHeading, 0)...)
	collector.add(extractText(input, input.Content, SourceText, 0)...)
	collector.add(extractTableBlocks(input)...)
	return collector.results()
}

func FromOpenAPIEndpoint(input OpenAPIEndpointInput) EntityCandidate {
	method := CanonicalMethod(input.Method)
	path := CanonicalPath(input.Path)
	operation := CanonicalOperation(input.Operation)
	canonical := path
	if method != "" && path != "" {
		canonical = method + " " + path
	}
	return EntityCandidate{
		Kind:       EntityAPIEndpoint,
		Raw:        strings.TrimSpace(strings.Join(nonEmpty([]string{method, path}), " ")),
		Canonical:  canonical,
		Method:     method,
		Path:       path,
		Operation:  operation,
		Source:     SourceOpenAPI,
		Confidence: 1.0,
		Evidence:   canonical,
		SectionID:  input.SectionID,
		DocumentID: input.DocumentID,
	}
}

func PathLiterals(text string) []string {
	paths := scanPathCandidates(text)
	values := make([]string, 0, len(paths))
	for _, path := range paths {
		values = append(values, path.Canonical)
	}
	return uniqueStrings(values)
}

func extractText(input SectionInput, text string, source SourceKind, baseOffset int) []EntityCandidate {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	entities := make([]EntityCandidate, 0)
	offset := baseOffset
	for _, line := range splitLinesKeepingOffsets(text) {
		paths := scanPathCandidates(line.text)
		methods := scanMethodCandidates(line.text)
		for _, path := range paths {
			if method, ok := closestMethodBefore(methods, path.Start); ok {
				entities = append(entities, EntityCandidate{
					Kind:       EntityAPIEndpoint,
					Raw:        strings.TrimSpace(method.Raw + " " + path.Raw),
					Canonical:  method.Method + " " + path.Canonical,
					Method:     method.Method,
					Path:       path.Canonical,
					Source:     source,
					Confidence: 0.95,
					Evidence:   strings.TrimSpace(line.text),
					SectionID:  input.SectionID,
					DocumentID: input.DocumentID,
					SpanStart:  offset + method.Start,
					SpanEnd:    offset + path.End,
					Notes:      []string{"method_from_same_line"},
				})
				continue
			}
			entities = append(entities, EntityCandidate{
				Kind:       EntityPathLiteral,
				Raw:        path.Raw,
				Canonical:  path.Canonical,
				Path:       path.Canonical,
				Source:     source,
				Confidence: pathConfidence(source, line.text),
				Evidence:   strings.TrimSpace(line.text),
				SectionID:  input.SectionID,
				DocumentID: input.DocumentID,
				SpanStart:  offset + path.Start,
				SpanEnd:    offset + path.End,
			})
		}
		for _, op := range scanOperationCandidates(line.text) {
			entities = append(entities, EntityCandidate{
				Kind:       EntityOperationCandidate,
				Raw:        op.raw,
				Canonical:  op.operation,
				Operation:  op.operation,
				Source:     source,
				Confidence: 0.55,
				Evidence:   strings.TrimSpace(line.text),
				SectionID:  input.SectionID,
				DocumentID: input.DocumentID,
				SpanStart:  offset + op.start,
				SpanEnd:    offset + op.end,
			})
		}
		offset += len(line.text) + line.separatorLen
	}
	return entities
}

func pathConfidence(source SourceKind, line string) float64 {
	if source == SourceHeading {
		return 0.65
	}
	lower := strings.ToLower(line)
	if strings.Contains(lower, "interface") || strings.Contains(lower, "path") || strings.Contains(line, "接口") {
		return 0.65
	}
	return 0.40
}

func closestMethodBefore(methods []methodCandidate, pathStart int) (methodCandidate, bool) {
	var best methodCandidate
	found := false
	for _, method := range methods {
		if method.End > pathStart {
			continue
		}
		if !found || method.End > best.End {
			best = method
			found = true
		}
	}
	return best, found
}

func scanMethodCandidates(text string) []methodCandidate {
	methods := make([]methodCandidate, 0)
	for start := 0; start < len(text); {
		for start < len(text) && !isASCIILetter(text[start]) {
			start++
		}
		end := start
		for end < len(text) && isASCIILetter(text[end]) {
			end++
		}
		if start == end {
			continue
		}
		raw := text[start:end]
		if method := CanonicalMethod(raw); method != "" {
			methods = append(methods, methodCandidate{Raw: raw, Method: method, Start: start, End: end})
		}
		start = end
	}
	return methods
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

type operationCandidate struct {
	raw       string
	operation string
	start     int
	end       int
}

func scanOperationCandidates(text string) []operationCandidate {
	matches := operationPattern.FindAllStringIndex(text, -1)
	ops := make([]operationCandidate, 0, len(matches))
	for _, match := range matches {
		raw := text[match[0]:match[1]]
		if CanonicalMethod(raw) != "" {
			continue
		}
		ops = append(ops, operationCandidate{
			raw:       raw,
			operation: CanonicalOperation(raw),
			start:     match[0],
			end:       match[1],
		})
	}
	return ops
}

type lineWithOffset struct {
	text         string
	separatorLen int
}

func splitLinesKeepingOffsets(text string) []lineWithOffset {
	if text == "" {
		return nil
	}
	lines := make([]lineWithOffset, 0)
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] != '\n' {
			continue
		}
		separatorLen := 1
		lineText := text[start:i]
		if len(lineText) > 0 && lineText[len(lineText)-1] == '\r' {
			lineText = lineText[:len(lineText)-1]
		}
		lines = append(lines, lineWithOffset{text: lineText, separatorLen: separatorLen})
		start = i + 1
	}
	lines = append(lines, lineWithOffset{text: text[start:], separatorLen: 0})
	return lines
}

type entityCollector struct {
	entities []EntityCandidate
	seen     map[string]bool
}

func (c *entityCollector) add(entities ...EntityCandidate) {
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	for _, entity := range entities {
		if entity.Kind == "" || entity.Canonical == "" {
			continue
		}
		key := strings.Join([]string{
			string(entity.Kind),
			entity.Method,
			entity.Path,
			entity.Operation,
			string(entity.Source),
			entity.Evidence,
		}, "\x00")
		if c.seen[key] {
			continue
		}
		c.seen[key] = true
		c.entities = append(c.entities, entity)
	}
}

func (c *entityCollector) results() []EntityCandidate {
	sort.SliceStable(c.entities, func(i, j int) bool {
		if c.entities[i].SpanStart == c.entities[j].SpanStart {
			return c.entities[i].Canonical < c.entities[j].Canonical
		}
		return c.entities[i].SpanStart < c.entities[j].SpanStart
	})
	return c.entities
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

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, value)
	}
	return result
}
