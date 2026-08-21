package manifest

import (
	"errors"
	"fmt"
)

// LimitKind names the resource limit that an evaluation exceeded.
type LimitKind string

const (
	LimitInstructions LimitKind = "instructions" // VM step budget
	LimitMemory       LimitKind = "memory"       // allocation budget
	LimitRecursion    LimitKind = "recursion"    // call-stack / register depth
	LimitWallClock    LimitKind = "wall-clock"   // deadline or caller cancellation
	LimitOutput       LimitKind = "output"       // recorded entries
	LimitStringLength LimitKind = "string-length"
	LimitSourceSize   LimitKind = "source-size"
)

// ErrLimitExceeded is returned when the manifest exceeded a sandbox limit.
// It always wraps ErrLimit so callers can test with errors.Is.
type ErrLimitExceeded struct {
	Kind   LimitKind
	Detail string
}

func (e *ErrLimitExceeded) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("manifest: %s limit exceeded", e.Kind)
	}
	return fmt.Sprintf("manifest: %s limit exceeded: %s", e.Kind, e.Detail)
}

func (e *ErrLimitExceeded) Is(target error) bool { return target == ErrLimit }

// Sentinel errors. Every error returned by Evaluate wraps exactly one of them.
var (
	// ErrLimit is the class of every *ErrLimitExceeded.
	ErrLimit = errors.New("manifest: sandbox limit exceeded")
	// ErrSyntax is a Lua parse/compile failure.
	ErrSyntax = errors.New("manifest: syntax error")
	// ErrForbidden is an attempt to reach a capability the sandbox does not
	// expose (io, os, require, load, ...). Reading an unknown global is also
	// forbidden so that misspelled or dynamic directives cannot vanish silently.
	ErrForbidden = errors.New("manifest: forbidden operation")
	// ErrUnknownDirective is a call to a top-level name that is neither a
	// recorded directive nor a tolerated cosmetic directive.
	ErrUnknownDirective = errors.New("manifest: unknown directive")
	// ErrBadArgument is a directive called with an unsupported value shape.
	ErrBadArgument = errors.New("manifest: invalid directive argument")
	// ErrRuntime is any other Lua runtime error raised by the manifest.
	ErrRuntime = errors.New("manifest: runtime error")
	// ErrInvalid is a manifest that evaluated cleanly but violates the
	// FiveM manifest rules (for example fxmanifest.lua without fx_version).
	ErrInvalid = errors.New("manifest: invalid manifest")
)
