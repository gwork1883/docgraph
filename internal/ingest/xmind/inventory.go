package xmind

import (
	"encoding/json"
	"fmt"
	"sort"
)

// InventoryStatus is the exhaustive disposition of a source encounter.
type InventoryStatus string

const (
	InventoryIndexed     InventoryStatus = "indexed"
	InventoryPreserved   InventoryStatus = "preserved"
	InventoryRejected    InventoryStatus = "rejected"
	InventoryUnsupported InventoryStatus = "unsupported"
)

func (s InventoryStatus) valid() bool {
	switch s {
	case InventoryIndexed, InventoryPreserved, InventoryRejected, InventoryUnsupported:
		return true
	default:
		return false
	}
}

// FeatureEncounter records a structural source element or an unknown field.
// Location is a JSON Pointer or namespace-aware XML path. Raw is bounded by
// Limits.MaxRichTextBytes; the package snapshot remains the lossless authority.
type FeatureEncounter struct {
	Feature         string          `json:"feature"`
	SourceName      string          `json:"source_name,omitempty"`
	SourceElementID string          `json:"source_element_id,omitempty"`
	Location        string          `json:"location"`
	Status          InventoryStatus `json:"status"`
	Count           uint64          `json:"count"`
	ReasonCode      string          `json:"reason_code,omitempty"`
	Detail          string          `json:"detail,omitempty"`
	Raw             json.RawMessage `json:"raw,omitempty"`
}

// PackageEntry is content evidence from the ZIP central directory and a full
// streaming read. Digest is SHA-256 of uncompressed bytes. No member is written
// to a filesystem path during inspection.
type PackageEntry struct {
	Path              string `json:"path"`
	Directory         bool   `json:"directory"`
	Method            uint16 `json:"method"`
	Flags             uint16 `json:"flags"`
	CRC32             uint32 `json:"crc32"`
	CompressedBytes   uint64 `json:"compressed_bytes"`
	UncompressedBytes uint64 `json:"uncompressed_bytes"`
	SHA256            string `json:"sha256,omitempty"`
}

// ResourceEncounter records every ZIP entry, including unreferenced entries.
// Parsers later add owning topic IDs, roles, MIME evidence, and reference state.
type ResourceEncounter struct {
	PackageEntry
	Status         InventoryStatus `json:"status"`
	ReasonCode     string          `json:"reason_code,omitempty"`
	Referenced     bool            `json:"referenced"`
	ReferenceCount uint64          `json:"reference_count"`
	OwningTopicIDs []string        `json:"owning_topic_ids,omitempty"`
	Roles          []string        `json:"roles,omitempty"`
	DetectedMIME   string          `json:"detected_mime,omitempty"`
}

type FeatureInventory struct {
	Features  []FeatureEncounter  `json:"features"`
	Resources []ResourceEncounter `json:"resources"`
}

type FeatureCoverage struct {
	Feature string          `json:"feature"`
	Status  InventoryStatus `json:"status"`
	Count   uint64          `json:"count"`
}

func (i FeatureInventory) Validate() error {
	for n, item := range i.Features {
		if item.Feature == "" {
			return fmt.Errorf("feature inventory item %d has no feature", n)
		}
		if item.Location == "" {
			return fmt.Errorf("feature inventory item %d has no location", n)
		}
		if !item.Status.valid() {
			return fmt.Errorf("feature inventory item %d has invalid status %q", n, item.Status)
		}
		if item.Count == 0 {
			return fmt.Errorf("feature inventory item %d has zero count", n)
		}
		if (item.Status == InventoryRejected || item.Status == InventoryUnsupported) && item.ReasonCode == "" {
			return fmt.Errorf("feature inventory item %d with status %q has no reason code", n, item.Status)
		}
	}
	for n, resource := range i.Resources {
		if resource.Path == "" {
			return fmt.Errorf("resource inventory item %d has no path", n)
		}
		if !resource.Status.valid() {
			return fmt.Errorf("resource inventory item %d has invalid status %q", n, resource.Status)
		}
		if (resource.Status == InventoryRejected || resource.Status == InventoryUnsupported) && resource.ReasonCode == "" {
			return fmt.Errorf("resource inventory item %d with status %q has no reason code", n, resource.Status)
		}
	}
	return nil
}

// Coverage returns deterministic aggregate counts for health and Web views.
func (i FeatureInventory) Coverage() []FeatureCoverage {
	type key struct {
		feature string
		status  InventoryStatus
	}
	counts := make(map[key]uint64)
	for _, item := range i.Features {
		counts[key{feature: item.Feature, status: item.Status}] += item.Count
	}
	out := make([]FeatureCoverage, 0, len(counts))
	for k, count := range counts {
		out = append(out, FeatureCoverage{Feature: k.feature, Status: k.status, Count: count})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Feature != out[b].Feature {
			return out[a].Feature < out[b].Feature
		}
		return out[a].Status < out[b].Status
	})
	return out
}
