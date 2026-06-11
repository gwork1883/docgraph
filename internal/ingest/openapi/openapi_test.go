package openapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMinimalOpenAPIJSON(t *testing.T) {
	doc, err := Parse("membership.json", []byte(`{
  "openapi": "3.0.3",
  "info": {
    "title": "Membership API",
    "version": "1.0.0"
  },
  "paths": {
    "/member/benefits": {
      "get": {
        "operationId": "getMemberBenefits",
        "summary": "List member benefits",
        "description": "Returns available benefits for a member account."
      }
    },
    "/member/benefits/redeem": {
      "post": {
        "operationId": "redeemMemberBenefit",
        "summary": "Redeem a member benefit",
        "description": "Creates a redemption for the selected benefit."
      }
    }
  }
}`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if doc.Title != "Membership API" {
		t.Fatalf("Title = %q, want Membership API", doc.Title)
	}
	if doc.Path != "membership.json" {
		t.Fatalf("Path = %q, want membership.json", doc.Path)
	}
	if doc.Hash == "" {
		t.Fatal("Hash is empty")
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("Sections len = %d, want 2", len(doc.Sections))
	}

	first := doc.Sections[0]
	if first.Title != "GET /member/benefits" {
		t.Fatalf("first Title = %q, want GET /member/benefits", first.Title)
	}
	if got := strings.Join(first.HeadingPath, " > "); got != "Membership API > API > GET /member/benefits" {
		t.Fatalf("first HeadingPath = %q, want Membership API > API > GET /member/benefits", got)
	}
	for _, want := range []string{
		"GET /member/benefits",
		"operationId: getMemberBenefits",
		"summary: List member benefits",
		"description: Returns available benefits for a member account.",
	} {
		if !strings.Contains(first.Content, want) {
			t.Fatalf("first Content = %q, want substring %q", first.Content, want)
		}
	}
	if first.Ordinal != 0 || first.Hash == "" {
		t.Fatalf("first section ordinal/hash = %d/%q, want 0/non-empty", first.Ordinal, first.Hash)
	}

	second := doc.Sections[1]
	if second.Title != "POST /member/benefits/redeem" {
		t.Fatalf("second Title = %q, want POST /member/benefits/redeem", second.Title)
	}
	if !strings.Contains(second.Content, "operationId: redeemMemberBenefit") {
		t.Fatalf("second Content = %q, want operationId", second.Content)
	}
	if second.Ordinal != 1 {
		t.Fatalf("second Ordinal = %d, want 1", second.Ordinal)
	}
}

func TestParseUsesFileNameWhenInfoTitleMissing(t *testing.T) {
	doc, err := Parse("openapi.json", []byte(`{
  "openapi": "3.0.3",
  "paths": {
    "/health": {
      "get": {
        "summary": "Health check"
      }
    }
  }
}`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if doc.Title != "openapi.json" {
		t.Fatalf("Title = %q, want openapi.json", doc.Title)
	}
	if len(doc.Sections) != 1 {
		t.Fatalf("Sections len = %d, want 1", len(doc.Sections))
	}
	if !strings.Contains(doc.Sections[0].Content, "summary: Health check") {
		t.Fatalf("section Content = %q, want summary", doc.Sections[0].Content)
	}
}

func TestParseOpenAPIYAMLMatchesJSONEndpointSections(t *testing.T) {
	jsonDoc, err := Parse("membership.openapi.json", []byte(openAPIFixtureJSON))
	if err != nil {
		t.Fatalf("Parse JSON returned error: %v", err)
	}
	yamlDoc, err := Parse("membership.openapi.yaml", []byte(openAPIFixtureYAML))
	if err != nil {
		t.Fatalf("Parse YAML returned error: %v", err)
	}

	if yamlDoc.Title != jsonDoc.Title {
		t.Fatalf("YAML title = %q, want JSON title %q", yamlDoc.Title, jsonDoc.Title)
	}
	if len(yamlDoc.Sections) != len(jsonDoc.Sections) {
		t.Fatalf("YAML sections len = %d, want JSON sections len %d", len(yamlDoc.Sections), len(jsonDoc.Sections))
	}
	for i := range jsonDoc.Sections {
		got := yamlDoc.Sections[i]
		want := jsonDoc.Sections[i]
		if got.Title != want.Title || strings.Join(got.HeadingPath, " > ") != strings.Join(want.HeadingPath, " > ") {
			t.Fatalf("YAML section[%d] identity = %+v, want %+v", i, got, want)
		}
		if got.Content != want.Content {
			t.Fatalf("YAML section[%d] content mismatch\n got: %q\nwant: %q", i, got.Content, want.Content)
		}
		if got.Ordinal != want.Ordinal || got.Hash != want.Hash {
			t.Fatalf("YAML section[%d] ordinal/hash = %d/%q, want %d/%q", i, got.Ordinal, got.Hash, want.Ordinal, want.Hash)
		}
	}
}

func TestLoadOpenAPIYAMLAndYMLFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"membership.openapi.yaml", "membership.openapi.yml"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(openAPIFixtureYAML), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}

		doc, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%s) returned error: %v", name, err)
		}
		if doc.Path != filepath.ToSlash(path) {
			t.Fatalf("Load(%s) Path = %q, want %q", name, doc.Path, filepath.ToSlash(path))
		}
		if doc.Title != "Membership API" || len(doc.Sections) != 2 {
			t.Fatalf("Load(%s) doc = %+v, want Membership API with 2 sections", name, doc)
		}
	}
}

func TestParseOpenAPIYAMLIncludesOperationDetailsAndLocalRefs(t *testing.T) {
	doc, err := Parse("membership.openapi.yaml", []byte(openAPIFixtureYAML))
	if err != nil {
		t.Fatalf("Parse YAML returned error: %v", err)
	}

	getSection := findSection(t, doc, "GET /member/benefits")
	assertContentContains(t, getSection.Content,
		"tags: membership, benefits",
		"operationId: getMemberBenefits",
		"summary: List member benefits",
		"description: Returns available benefits for a member account.",
		"memberId",
		"query",
		"Member identifier from shared components.",
		"200",
		"Successful benefit list response.",
	)

	postSection := findSection(t, doc, "POST /member/benefits/redeem")
	assertContentContains(t, postSection.Content,
		"tags: membership, redemption",
		"operationId: redeemMemberBenefit",
		"Redemption payload from shared components.",
		"application/json",
		"201",
		"Redemption created.",
	)
}

func findSection(t *testing.T, doc Document, title string) Section {
	t.Helper()

	for _, section := range doc.Sections {
		if section.Title == title {
			return section
		}
	}
	t.Fatalf("section %q not found in %+v", title, doc.Sections)
	return Section{}
}

func assertContentContains(t *testing.T, content string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(content, want) {
			t.Fatalf("content missing %q\ncontent:\n%s", want, content)
		}
	}
}

const openAPIFixtureJSON = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Membership API",
    "version": "1.0.0"
  },
  "paths": {
    "/member/benefits": {
      "get": {
        "operationId": "getMemberBenefits",
        "tags": ["membership", "benefits"],
        "summary": "List member benefits",
        "description": "Returns available benefits for a member account.",
        "parameters": [
          {
            "$ref": "#/components/parameters/MemberID"
          }
        ],
        "responses": {
          "200": {
            "$ref": "#/components/responses/BenefitListResponse"
          }
        }
      }
    },
    "/member/benefits/redeem": {
      "post": {
        "operationId": "redeemMemberBenefit",
        "tags": ["membership", "redemption"],
        "summary": "Redeem a member benefit",
        "requestBody": {
          "$ref": "#/components/requestBodies/RedeemBenefitRequest"
        },
        "responses": {
          "201": {
            "description": "Redemption created."
          }
        }
      }
    }
  },
  "components": {
    "parameters": {
      "MemberID": {
        "name": "memberId",
        "in": "query",
        "description": "Member identifier from shared components."
      }
    },
    "requestBodies": {
      "RedeemBenefitRequest": {
        "description": "Redemption payload from shared components.",
        "content": {
          "application/json": {
            "schema": {
              "$ref": "#/components/schemas/RedeemBenefitCommand"
            }
          }
        }
      }
    },
    "responses": {
      "BenefitListResponse": {
        "description": "Successful benefit list response.",
        "content": {
          "application/json": {
            "schema": {
              "type": "object"
            }
          }
        }
      }
    },
    "schemas": {
      "RedeemBenefitCommand": {
        "type": "object"
      }
    }
  }
}`

const openAPIFixtureYAML = `openapi: 3.0.3
info:
  title: Membership API
  version: 1.0.0
paths:
  /member/benefits:
    get:
      operationId: getMemberBenefits
      tags:
        - membership
        - benefits
      summary: List member benefits
      description: Returns available benefits for a member account.
      parameters:
        - $ref: '#/components/parameters/MemberID'
      responses:
        '200':
          $ref: '#/components/responses/BenefitListResponse'
  /member/benefits/redeem:
    post:
      operationId: redeemMemberBenefit
      tags:
        - membership
        - redemption
      summary: Redeem a member benefit
      requestBody:
        $ref: '#/components/requestBodies/RedeemBenefitRequest'
      responses:
        '201':
          description: Redemption created.
components:
  parameters:
    MemberID:
      name: memberId
      in: query
      description: Member identifier from shared components.
  requestBodies:
    RedeemBenefitRequest:
      description: Redemption payload from shared components.
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/RedeemBenefitCommand'
  responses:
    BenefitListResponse:
      description: Successful benefit list response.
      content:
        application/json:
          schema:
            type: object
  schemas:
    RedeemBenefitCommand:
      type: object
`
