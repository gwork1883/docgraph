package xmind

// Limits bounds archive inspection and the later normalized parser. Phase 0
// enforces the archive fields; the model fields form the contract for adapters.
// A zero field is replaced by the corresponding default.
type Limits struct {
	MaxArchiveBytes       uint64
	MaxEntries            uint64
	MaxEntryBytes         uint64
	MaxTotalBytes         uint64
	MaxCompressionRatio   uint64
	MaxNormalizedElements uint64
	MaxDepth              uint64
	MaxRichTextBytes      uint64
}

func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes:       256 << 20,
		MaxEntries:            4096,
		MaxEntryBytes:         64 << 20,
		MaxTotalBytes:         512 << 20,
		MaxCompressionRatio:   200,
		MaxNormalizedElements: 250_000,
		MaxDepth:              512,
		MaxRichTextBytes:      8 << 20,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxArchiveBytes == 0 {
		l.MaxArchiveBytes = d.MaxArchiveBytes
	}
	if l.MaxEntries == 0 {
		l.MaxEntries = d.MaxEntries
	}
	if l.MaxEntryBytes == 0 {
		l.MaxEntryBytes = d.MaxEntryBytes
	}
	if l.MaxTotalBytes == 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxCompressionRatio == 0 {
		l.MaxCompressionRatio = d.MaxCompressionRatio
	}
	if l.MaxNormalizedElements == 0 {
		l.MaxNormalizedElements = d.MaxNormalizedElements
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxRichTextBytes == 0 {
		l.MaxRichTextBytes = d.MaxRichTextBytes
	}
	return l
}
