package searchtoken

import "testing"

func TestGSETokenizerCutsSearchQueries(t *testing.T) {
	tokenizer, err := NewGSETokenizer(Options{
		DictionaryEntries: []DictionaryEntry{
			{Text: "短词", Freq: 1000, Pos: "n"},
			{Text: "数据模型", Freq: 1000, Pos: "n"},
		},
	})
	if err != nil {
		t.Fatalf("NewGSETokenizer returned error: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "mixed latin and custom short chinese word",
			query: "sample中文短词",
			want:  []string{"sample", "中文", "短词"},
		},
		{
			name:  "mixed latin default chinese phrase and technical suffix",
			query: "sample默认数据模型json",
			want:  []string{"sample", "默认", "数据模型", "json"},
		},
		{
			name:  "api path with operation suffix is preserved as technical symbol",
			query: "GET /entity/v1/entities:filter-filter-count",
			want:  []string{"/entity/v1/entities:filter-filter-count", "entity", "v1"},
		},
		{
			name:  "camel case api path is preserved as technical symbol",
			query: "GET /EntityV1/BatchGetEntityMeta",
			want:  []string{"/EntityV1/BatchGetEntityMeta", "Entity", "Batch"},
		},
		{
			name:  "api path with templated field is preserved as technical symbol",
			query: "GET /access/v1/access/{ object.id }/meta",
			want:  []string{"/access/v1/access/{object.id}/meta", "object.id", "meta"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			terms := tokenizer.QueryTerms(tt.query)
			for _, want := range tt.want {
				if !termsContain(terms, want) {
					t.Fatalf("QueryTerms(%q) = %+v, want term %q", tt.query, terms, want)
				}
			}
		})
	}
}

func TestSymbolTermsUsesTechnicalExtractorForPathLiterals(t *testing.T) {
	terms := Default().SymbolTerms(`interface | /storage/任意片段/meta
/`)
	if !stringTermsContain(terms, "/storage/任意片段/meta") {
		t.Fatalf("SymbolTerms missing path literal: %+v", terms)
	}
	if stringTermsContain(terms, "/") {
		t.Fatalf("SymbolTerms includes bare slash: %+v", terms)
	}
}

func termsContain(terms []Term, want string) bool {
	for _, term := range terms {
		if term.Text == want {
			return true
		}
	}
	return false
}

func stringTermsContain(terms []string, want string) bool {
	for _, term := range terms {
		if term == want {
			return true
		}
	}
	return false
}
