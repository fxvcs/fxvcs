package sdk

// Operation contracts are split by area:
//   ops_lifecycle.go  init / clone / setup / upgrade / track / untrack / scan / migrate
//   ops_checkout.go   hydrate / dehydrate / status / verify / doctor / repair
//   ops_storage.go    storage / objects / cache
//   ops_fivem.go      resources sync / set / validate
// Each operation is gated by the compatibility preflight (Client.openRepo).
// Request/Result shapes are part of the v0 surface and may only gain optional
// fields.

// PathSelector names literal repository-relative paths, or glob patterns when
// Glob is true. Literal is the default; brackets are never glob syntax unless
// Glob is set.
type PathSelector struct {
	Paths []string `json:"paths"`
	Glob  bool     `json:"glob,omitempty"`
}

// ItemOutcome reports one path's result inside a multi-path operation.
type ItemOutcome struct {
	Path   string `json:"path"`
	Digest string `json:"digest,omitempty"`
	// Bytes is the asset-sized quantity the item accounted for: written for a
	// load, reclaimed from the working tree for an unload, uploaded for a
	// publication. It is never the size of the placeholder an unload leaves
	// behind, so a consumer can sum it or put it in a ratio without checking
	// which operation produced it.
	Bytes   int64  `json:"bytes,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Paths lists the repository paths an uploaded object belongs to. A
	// chunked asset is stored as many objects, so publication outcomes are
	// per object while users think in files; this maps one back to the other.
	Paths []string `json:"paths,omitempty"`
	// Reuse says the item's content was not transferred: ReuseCached when it
	// was already stored locally, ReuseShared when another item in the same
	// operation has identical content and one transfer served both. Empty
	// means it was transferred.
	Reuse Reuse `json:"reuse,omitempty"`
}

// Reuse classifies content that did not have to be transferred. Content
// addressing is what makes it possible — one stored copy of a font vendored
// into forty resources — and a consumer that reports it is reporting the
// thing the storage model buys.
type Reuse string

const (
	// ReuseCached: the content was already in the local object store.
	ReuseCached Reuse = "cached"
	// ReuseShared: another item in the same operation has identical content.
	ReuseShared Reuse = "shared"
)
