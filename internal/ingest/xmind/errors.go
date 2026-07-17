package xmind

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable, machine-readable failure category. Callers should
// branch on the code (or errors.Is with the sentinels below), not error text.
type ErrorCode string

const (
	ErrorInvalidArgument   ErrorCode = "invalid_argument"
	ErrorInvalidArchive    ErrorCode = "invalid_archive"
	ErrorEncryptedArchive  ErrorCode = "encrypted_archive"
	ErrorArchiveLimit      ErrorCode = "archive_limit_exceeded"
	ErrorUnsafeArchivePath ErrorCode = "unsafe_archive_path"
	ErrorDuplicateEntry    ErrorCode = "duplicate_archive_entry"
	ErrorUnknownFormat     ErrorCode = "unknown_format"
	ErrorAmbiguousFormat   ErrorCode = "ambiguous_format"
	ErrorUnsupportedFormat ErrorCode = "unsupported_format"
	ErrorInvalidPayload    ErrorCode = "invalid_workbook_payload"
	ErrorValidation        ErrorCode = "workbook_validation_failed"
)

// PackageError describes a failure that must stop ingestion before a Workbook
// can be returned. Entry and limit fields are populated when relevant.
type PackageError struct {
	Code      ErrorCode
	Operation string
	Entry     string
	Limit     string
	Actual    uint64
	Maximum   uint64
	Detail    string
	Err       error
}

func (e *PackageError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := string(e.Code)
	if e.Operation != "" {
		msg = e.Operation + ": " + msg
	}
	if e.Entry != "" {
		msg += fmt.Sprintf(" (entry %q)", e.Entry)
	}
	if e.Limit != "" {
		msg += fmt.Sprintf(" (%s: actual=%d maximum=%d)", e.Limit, e.Actual, e.Maximum)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *PackageError) Unwrap() error { return e.Err }

func (e *PackageError) Is(target error) bool {
	var other *PackageError
	return errors.As(target, &other) && other.Code != "" && e.Code == other.Code
}

var (
	ErrInvalidArgument   = &PackageError{Code: ErrorInvalidArgument}
	ErrInvalidArchive    = &PackageError{Code: ErrorInvalidArchive}
	ErrEncryptedArchive  = &PackageError{Code: ErrorEncryptedArchive}
	ErrArchiveLimit      = &PackageError{Code: ErrorArchiveLimit}
	ErrUnsafeArchivePath = &PackageError{Code: ErrorUnsafeArchivePath}
	ErrDuplicateEntry    = &PackageError{Code: ErrorDuplicateEntry}
	ErrUnknownFormat     = &PackageError{Code: ErrorUnknownFormat}
	ErrAmbiguousFormat   = &PackageError{Code: ErrorAmbiguousFormat}
	ErrUnsupportedFormat = &PackageError{Code: ErrorUnsupportedFormat}
	ErrInvalidPayload    = &PackageError{Code: ErrorInvalidPayload}
	ErrValidation        = &PackageError{Code: ErrorValidation}
)
