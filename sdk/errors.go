package sdk

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error classification. Codes are part of
// the v0 compatibility surface: existing codes are never renamed or removed
// within a major version; new codes may be added.
type Code string

const (
	CodeUnknown           Code = "unknown"
	CodeInvalidArgument   Code = "invalid-argument"
	CodeNotImplemented    Code = "not-implemented"
	CodeUpgradeRequired   Code = "upgrade-required"
	CodeUnsupportedFormat Code = "unsupported-format"
	CodeNotRepository     Code = "not-repository"
	CodeNotFound          Code = "not-found"
	// CodeNotCached: the content is not on this machine. It is deliberately
	// distinct from CodeNotFound, which means the content could not be found
	// anywhere this repository can reach. A caller that gets this one has
	// something to offer the user — ObjectFetch — rather than a fault to
	// report.
	CodeNotCached  Code = "not-cached"
	CodeConflict   Code = "conflict"
	CodeBusy       Code = "busy"
	CodeIntegrity  Code = "integrity"
	CodePathEscape Code = "path-escape"
	CodeCancelled  Code = "cancelled"
	CodeGit        Code = "git"
	CodeStorage    Code = "storage"
	CodeIO         Code = "io"
	CodeValidation Code = "validation"
)

// Error is the only error type returned across the SDK boundary. Wrap with
// errors.As(err, &sdkErr) to inspect the code; errors.Is works with sentinel
// causes such as context.Canceled.
type Error struct {
	Code    Code
	Message string
	// Details holds typed, safe-to-display supplementary data. It never
	// contains secret values or credential material.
	Details any
	// Hint is an optional, copy-pastable next step (usually an exact
	// command) a presentation layer may show under the message.
	Hint  string
	cause error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the underlying cause for errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.cause }

// NewError constructs an SDK error.
func NewError(code Code, msg string) *Error { return &Error{Code: code, Message: msg} }

// WrapError attaches a cause.
func WrapError(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, cause: cause}
}

// WithHint sets the next-step hint and returns the error for chaining.
func (e *Error) WithHint(hint string) *Error {
	e.Hint = hint
	return e
}

// HintOf returns the Hint of an SDK error ("" for other errors).
func HintOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Hint
	}
	return ""
}

// CodeOf extracts the Code from any error (CodeUnknown if not an SDK error).
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, errCancelledSentinel) {
		return CodeCancelled
	}
	return CodeUnknown
}

var errCancelledSentinel = errors.New("cancelled")

// UpgradeRequired is the Details payload of a CodeUpgradeRequired error. A
// consumer that receives it must not attempt any other operation on that
// repository, worktree, or target with the current binary.
type UpgradeRequired struct {
	InstalledVersion string `json:"installedVersion"`
	RequiredVersion  string `json:"requiredVersion"`
	Repository       string `json:"repository,omitempty"`
	Reason           string `json:"reason"` // minimumFXVCSVersion | gitIntegrationVersion | apiVersion | pointerFormat
	UpgradeCommand   string `json:"upgradeCommand"`
}

// BusyOwner is the Details payload of CodeBusy: safe metadata about the
// operation currently holding a lock.
type BusyOwner struct {
	Operation string `json:"operation"`
	PID       int    `json:"pid,omitempty"`
	Host      string `json:"host,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}

// ValidationIssue is one entry of the Details payload of CodeValidation.
type ValidationIssue struct {
	Path    string `json:"path"` // JSON pointer or file path
	Message string `json:"message"`
}
