package profile

import (
	"encoding/json"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
)

func TestBuildJSONDeterministicChineseAndAPIProfile(t *testing.T) {
	doc := domain.DocumentInput{
		ID:          "doc-auth",
		ExternalID:  "auth.md",
		Title:       "权限接口",
		URL:         "https://example.test/auth.md",
		ContentHash: "hash-auth",
	}
	sections := []domain.SectionInput{
		{
			ID:          "section-errors",
			HeadingPath: "权限接口 > 错误响应",
			Title:       "错误响应",
			Content:     "权限接口的几种错误响应包括 401 响应、403 响应和 error_code。GET /auth/permissions 返回权限列表。",
			ContentHash: "hash-section-errors",
		},
	}

	first, err := BuildJSON(doc, sections)
	if err != nil {
		t.Fatalf("BuildJSON returned error: %v", err)
	}
	second, err := BuildJSON(doc, sections)
	if err != nil {
		t.Fatalf("second BuildJSON returned error: %v", err)
	}
	if first != second {
		t.Fatalf("BuildJSON is not deterministic:\nfirst:  %s\nsecond: %s", first, second)
	}

	var got RetrievalProfile
	if err := json.Unmarshal([]byte(first), &got); err != nil {
		t.Fatalf("unmarshal generated profile: %v", err)
	}
	for _, want := range []string{"权限", "接口", "错误", "响应", "错误响应"} {
		if !profileHasTerm(got, want) {
			t.Fatalf("generated profile missing term %q: %+v", want, got.TopTerms)
		}
	}
	for _, want := range []string{"GET /auth/permissions", "401", "403", "error_code"} {
		if !stringSliceContains(got.APIRefs, want) {
			t.Fatalf("generated API refs = %+v, want %q", got.APIRefs, want)
		}
	}
	if got.Stats.SectionCount != 1 || got.Stats.TokenCount == 0 || got.Stats.UniqueTermCount == 0 {
		t.Fatalf("profile stats = %+v, want populated stats", got.Stats)
	}
}

func profileHasTerm(profile RetrievalProfile, term string) bool {
	for _, got := range profile.TopTerms {
		if got.Term == term {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
