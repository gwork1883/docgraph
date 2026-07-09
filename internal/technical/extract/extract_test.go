package extract

import "testing"

func TestExtractSectionTechnicalEntities(t *testing.T) {
	tests := []struct {
		name         string
		input        SectionInput
		want         EntityCandidate
		absentKind   EntityKind
		absentPath   string
		absentMethod string
	}{
		{
			name:  "same line get endpoint",
			input: SectionInput{Content: "GET /entity/v1/entities"},
			want:  EntityCandidate{Kind: EntityAPIEndpoint, Method: "GET", Path: "/entity/v1/entities", Canonical: "GET /entity/v1/entities"},
		},
		{
			name:  "same line post endpoint with operation suffix",
			input: SectionInput{Content: "POST /entity/v1/entities:filter-filter-count"},
			want:  EntityCandidate{Kind: EntityAPIEndpoint, Method: "POST", Path: "/entity/v1/entities:filter-filter-count", Canonical: "POST /entity/v1/entities:filter-filter-count"},
		},
		{
			name:  "path only camel case operation",
			input: SectionInput{Content: "/EntityV1/BatchGetEntityMeta"},
			want:  EntityCandidate{Kind: EntityPathLiteral, Path: "/EntityV1/BatchGetEntityMeta", Canonical: "/EntityV1/BatchGetEntityMeta"},
		},
		{
			name:  "placeholder whitespace canonicalization",
			input: SectionInput{Content: "DELETE /access/v1/access/{ object.id }/meta"},
			want:  EntityCandidate{Kind: EntityAPIEndpoint, Method: "DELETE", Path: "/access/v1/access/{object.id}/meta", Canonical: "DELETE /access/v1/access/{object.id}/meta"},
		},
		{
			name:  "non english path segment",
			input: SectionInput{Content: "/storage/任意片段/meta"},
			want:  EntityCandidate{Kind: EntityPathLiteral, Path: "/storage/任意片段/meta", Canonical: "/storage/任意片段/meta"},
		},
		{
			name:       "bare slash ignored",
			input:      SectionInput{Content: "/"},
			absentKind: EntityPathLiteral,
			absentPath: "/",
		},
		{
			name:         "path only does not default method",
			input:        SectionInput{Content: "/entity/v1/entities"},
			want:         EntityCandidate{Kind: EntityPathLiteral, Path: "/entity/v1/entities", Canonical: "/entity/v1/entities"},
			absentKind:   EntityAPIEndpoint,
			absentPath:   "/entity/v1/entities",
			absentMethod: "GET",
		},
		{
			name:         "method substring inside prose is not paired with path",
			input:        SectionInput{Content: "forget /entity/v1/entities"},
			want:         EntityCandidate{Kind: EntityPathLiteral, Path: "/entity/v1/entities", Canonical: "/entity/v1/entities"},
			absentKind:   EntityAPIEndpoint,
			absentPath:   "/entity/v1/entities",
			absentMethod: "GET",
		},
		{
			name:  "table split english method path",
			input: SectionInput{Content: "interface | /entity/v1/entities:filter-filter-count\nmethod | POST"},
			want:  EntityCandidate{Kind: EntityAPIEndpoint, Method: "POST", Path: "/entity/v1/entities:filter-filter-count", Canonical: "POST /entity/v1/entities:filter-filter-count"},
		},
		{
			name:  "table split chinese method path",
			input: SectionInput{Content: "接口 | /entity/v1/entities\n请求方式 | POST"},
			want:  EntityCandidate{Kind: EntityAPIEndpoint, Method: "POST", Path: "/entity/v1/entities", Canonical: "POST /entity/v1/entities"},
		},
		{
			name:         "ambiguous table does not pair",
			input:        SectionInput{Content: "path | /entity/v1/entities\npath | /entity/v1/entities:filter-filter-count\nmethod | GET\nmethod | POST"},
			want:         EntityCandidate{Kind: EntityPathLiteral, Path: "/entity/v1/entities", Canonical: "/entity/v1/entities"},
			absentKind:   EntityAPIEndpoint,
			absentPath:   "/entity/v1/entities",
			absentMethod: "GET",
		},
		{
			name:  "operation candidate",
			input: SectionInput{Content: "Call BatchGetEntityMeta after filtering."},
			want:  EntityCandidate{Kind: EntityOperationCandidate, Operation: "BatchGetEntityMeta", Canonical: "BatchGetEntityMeta"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractSection(tt.input)
			if tt.want.Kind != "" && !containsCandidate(got, tt.want) {
				t.Fatalf("ExtractSection() = %+v, want candidate %+v", got, tt.want)
			}
			if tt.absentKind != "" && containsCandidate(got, EntityCandidate{Kind: tt.absentKind, Method: tt.absentMethod, Path: tt.absentPath}) {
				t.Fatalf("ExtractSection() = %+v, did not want %s %s %s", got, tt.absentKind, tt.absentMethod, tt.absentPath)
			}
		})
	}
}

func TestFromOpenAPIEndpoint(t *testing.T) {
	got := FromOpenAPIEndpoint(OpenAPIEndpointInput{
		DocumentID: "doc-1",
		SectionID:  "section-1",
		Method:     "post",
		Path:       "/entity/v1/entities:filter-filter-count",
		Operation:  "FilterFilterCount",
	})
	if got.Kind != EntityAPIEndpoint || got.Source != SourceOpenAPI || got.Confidence != 1.0 {
		t.Fatalf("FromOpenAPIEndpoint() = %+v, want authoritative API endpoint", got)
	}
	if got.Method != "POST" || got.Path != "/entity/v1/entities:filter-filter-count" || got.Canonical != "POST /entity/v1/entities:filter-filter-count" {
		t.Fatalf("FromOpenAPIEndpoint() = %+v, want canonical endpoint", got)
	}
}

func TestPathLiteralsUsesSharedScanner(t *testing.T) {
	got := PathLiterals(`"/access/v1/access/{ object.id }/meta", /`)
	want := "/access/v1/access/{object.id}/meta"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("PathLiterals() = %+v, want [%q]", got, want)
	}
}

func containsCandidate(candidates []EntityCandidate, want EntityCandidate) bool {
	for _, candidate := range candidates {
		if want.Kind != "" && candidate.Kind != want.Kind {
			continue
		}
		if want.Method != "" && candidate.Method != want.Method {
			continue
		}
		if want.Path != "" && candidate.Path != want.Path {
			continue
		}
		if want.Operation != "" && candidate.Operation != want.Operation {
			continue
		}
		if want.Canonical != "" && candidate.Canonical != want.Canonical {
			continue
		}
		return true
	}
	return false
}
