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

func termsContain(terms []Term, want string) bool {
	for _, term := range terms {
		if term.Text == want {
			return true
		}
	}
	return false
}
