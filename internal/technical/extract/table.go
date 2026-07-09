package extract

import (
	"strings"
)

type tableRow struct {
	evidence string
	paths    []pathCandidate
	methods  []methodCandidate
}

func extractTableBlocks(input SectionInput) []EntityCandidate {
	lines := splitLinesKeepingOffsets(input.Content)
	collector := entityCollector{}
	block := make([]tableRow, 0)
	flush := func() {
		if len(block) == 0 {
			return
		}
		collector.add(resolveTableBlock(input, block)...)
		block = block[:0]
	}
	for _, line := range lines {
		row, ok := parseTableRow(line.text)
		if !ok {
			if strings.TrimSpace(line.text) == "" {
				flush()
			}
			continue
		}
		block = append(block, row)
	}
	flush()
	return collector.results()
}

func resolveTableBlock(input SectionInput, block []tableRow) []EntityCandidate {
	entities := make([]EntityCandidate, 0)
	blockPaths := make([]pathCandidate, 0)
	blockMethods := make([]methodCandidate, 0)
	for _, row := range block {
		for _, path := range row.paths {
			entities = append(entities, EntityCandidate{
				Kind:       EntityPathLiteral,
				Raw:        path.Raw,
				Canonical:  path.Canonical,
				Path:       path.Canonical,
				Source:     SourceTable,
				Confidence: 0.65,
				Evidence:   row.evidence,
				SectionID:  input.SectionID,
				DocumentID: input.DocumentID,
			})
		}
		if len(row.paths) == 1 && len(row.methods) == 1 {
			method := row.methods[0]
			path := row.paths[0]
			entities = append(entities, EntityCandidate{
				Kind:       EntityAPIEndpoint,
				Raw:        method.Method + " " + path.Raw,
				Canonical:  method.Method + " " + path.Canonical,
				Method:     method.Method,
				Path:       path.Canonical,
				Source:     SourceTable,
				Confidence: 0.90,
				Evidence:   row.evidence,
				SectionID:  input.SectionID,
				DocumentID: input.DocumentID,
				Notes:      []string{"method_path_same_table_row"},
			})
		}
		blockPaths = append(blockPaths, row.paths...)
		blockMethods = append(blockMethods, row.methods...)
	}
	uniquePaths := uniquePathCandidates(blockPaths)
	uniqueMethods := uniqueMethodCandidates(blockMethods)
	if len(uniquePaths) == 1 && len(uniqueMethods) == 1 {
		method := uniqueMethods[0]
		path := uniquePaths[0]
		entities = append(entities, EntityCandidate{
			Kind:       EntityAPIEndpoint,
			Raw:        method.Method + " " + path.Raw,
			Canonical:  method.Method + " " + path.Canonical,
			Method:     method.Method,
			Path:       path.Canonical,
			Source:     SourceTable,
			Confidence: 0.85,
			Evidence:   tableEvidence(block),
			SectionID:  input.SectionID,
			DocumentID: input.DocumentID,
			Notes:      []string{"method_path_same_key_value_block"},
		})
	}
	return entities
}

func parseTableRow(line string) (tableRow, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return tableRow{}, false
	}
	cells, ok := splitTableCells(trimmed)
	if !ok {
		return tableRow{}, false
	}
	row := tableRow{evidence: trimmed}
	if len(cells) >= 2 {
		for i := 0; i+1 < len(cells); i += 2 {
			label := normalizeLabel(cells[i])
			value := cells[i+1]
			if isPathLabel(label) {
				row.paths = append(row.paths, scanPathCandidates(value)...)
			}
			if isMethodLabel(label) {
				row.methods = append(row.methods, scanMethodCandidates(value)...)
			}
		}
	}
	if len(row.paths) == 0 {
		row.paths = append(row.paths, scanPathCandidates(trimmed)...)
	}
	if len(row.methods) == 0 {
		row.methods = append(row.methods, scanMethodCandidates(trimmed)...)
	}
	return row, len(row.paths) > 0 || len(row.methods) > 0
}

func splitTableCells(line string) ([]string, bool) {
	if strings.Contains(line, "|") {
		parts := strings.Split(line, "|")
		cells := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				cells = append(cells, part)
			}
		}
		return cells, len(cells) >= 2
	}
	for _, sep := range []string{":", "："} {
		if idx := strings.Index(line, sep); idx >= 0 {
			left := strings.TrimSpace(line[:idx])
			right := strings.TrimSpace(line[idx+len(sep):])
			return []string{left, right}, left != "" && right != ""
		}
	}
	return nil, false
}

func normalizeLabel(label string) string {
	label = strings.TrimSpace(strings.ToLower(label))
	label = strings.ReplaceAll(label, " ", "_")
	label = strings.ReplaceAll(label, "-", "_")
	return label
}

func isPathLabel(label string) bool {
	switch label {
	case "interface", "path", "api", "url", "接口", "路径":
		return true
	default:
		return false
	}
}

func isMethodLabel(label string) bool {
	switch label {
	case "method", "http_method", "request_method", "verb", "请求方式", "方法":
		return true
	default:
		return false
	}
}

func uniquePathCandidates(paths []pathCandidate) []pathCandidate {
	seen := map[string]bool{}
	result := make([]pathCandidate, 0, len(paths))
	for _, path := range paths {
		key := strings.ToLower(path.Canonical)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, path)
	}
	return result
}

func uniqueMethodCandidates(methods []methodCandidate) []methodCandidate {
	seen := map[string]bool{}
	result := make([]methodCandidate, 0, len(methods))
	for _, method := range methods {
		if method.Method == "" || seen[method.Method] {
			continue
		}
		seen[method.Method] = true
		result = append(result, method)
	}
	return result
}

func tableEvidence(block []tableRow) string {
	lines := make([]string, 0, len(block))
	for _, row := range block {
		lines = append(lines, row.evidence)
	}
	return strings.Join(lines, "\n")
}
