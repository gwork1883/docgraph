package xmind

import "io"

// ParseWorkbook fully validates and detects the package before dispatching to
// a structure-proven adapter. An unambiguously selected legacy Sheet-array
// JSON payload is admitted by its core structure; producer/version metadata is
// provenance and does not itself claim support for a current-format family.
// Structurally unknown JSON remains fail-closed. XMind 8 XML is admitted only
// after its content namespace, version, and core workbook structure validate.
func ParseWorkbook(reader io.ReaderAt, size int64, limits Limits) (Workbook, error) {
	detection, err := DetectPackage(reader, size, limits)
	if err != nil {
		return Workbook{}, err
	}
	if detection.Family == FormatXMind8XML || (detection.Family == FormatClassicZenJSON && !detection.FixtureRequired) {
		return parseDetectedWorkbook(reader, size, limits, detection)
	}
	detail := "real format fixture required before adapter implementation"
	switch detection.Family {
	case FormatJSONCandidate:
		detail = "detected a content.json workbook without an unambiguously selected legacy Sheet-array structure"
		if detection.FormatVersion != "" {
			detail += "; producer version " + detection.FormatVersion + " was recorded only as provenance"
		}
		detail += "; structurally different current or unknown JSON formats remain fail-closed"
	}
	return Workbook{}, &PackageError{Code: ErrorUnsupportedFormat, Operation: "parse xmind workbook", Detail: detail}
}
