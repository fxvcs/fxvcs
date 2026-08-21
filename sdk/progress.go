package sdk

import "time"

// EventKind classifies progress events. Kinds are additive within v0.
type EventKind string

const (
	EventOperationStarted  EventKind = "operation-started"
	EventOperationFinished EventKind = "operation-finished"
	EventItemStarted       EventKind = "item-started"
	EventItemProgress      EventKind = "item-progress"
	EventItemFinished      EventKind = "item-finished"
	EventWarning           EventKind = "warning"
	EventInfo              EventKind = "info"
	// EventPlan is emitted once when the exact work list is known. Most
	// operations emit it before any item starts; an operation whose metadata
	// must itself be fetched (object fetch) can first report indeterminate
	// resolving progress, then announce the exact plan. Items lists every
	// planned unit so a UI can show pending/in-progress/done; Total is the
	// summed byte count.
	EventPlan EventKind = "plan"
	// EventItemFailed reports a per-item failure that did not abort the
	// operation (the item appears in the result's Failed list).
	EventItemFailed EventKind = "item-failed"
	// EventItemSkipped is a terminal per-item outcome for work that was
	// considered but did not need to run (already in the requested state) or
	// was refused without aborting the rest of the operation.
	EventItemSkipped EventKind = "item-skipped"
	// EventItemCancelled is a terminal per-item outcome for planned work that
	// did not run because the operation was cancelled.
	EventItemCancelled EventKind = "item-cancelled"
	// EventStage announces that the operation moved to a different kind of
	// work; the Stage field says which.
	EventStage EventKind = "stage"
)

// PlanItem is one unit of planned work announced by EventPlan.
type PlanItem struct {
	Path   string   `json:"path,omitempty"`   // primary repository path (first referrer)
	Paths  []string `json:"paths,omitempty"`  // every referring path
	Digest string   `json:"digest,omitempty"` // sha256:...
	Bytes  int64    `json:"bytes,omitempty"`
	Remote string   `json:"remote,omitempty"` // destination storage remote
}

// Event is a structured progress record. Consumers render it; the SDK never
// prints. Fields are optional except Time, Operation, and Kind.
type Event struct {
	Time      time.Time `json:"time"`
	Operation string    `json:"operation"` // e.g. "hydrate"
	Kind      EventKind `json:"kind"`
	Path      string    `json:"path,omitempty"`   // repository-relative path
	Digest    string    `json:"digest,omitempty"` // sha256:...
	Bytes     int64     `json:"bytes,omitempty"`
	Total     int64     `json:"total,omitempty"`
	Message   string    `json:"message,omitempty"`
	Code      Code      `json:"code,omitempty"` // for warnings
	// Remote names the storage remote an item event refers to (publish).
	Remote string `json:"remote,omitempty"`
	// Phase says what kind of work the item event reports. It exists so a
	// consumer can render "downloading 3.2 of 8.0 MiB" and "writing" as
	// different states of the same file — a desktop shows them as separate
	// bars, and a terminal shows the transfer bar and then a tick.
	//
	// An item generally passes through PhaseTransfer (its data crosses the
	// network, possibly long before the file is produced) and then
	// PhaseWrite. Operations with no transfer emit only PhaseWrite.
	Phase Phase `json:"phase,omitempty"`
	// Items is set on EventPlan.
	Items []PlanItem `json:"items,omitempty"`
	// Index/Count position an item event within the plan (1-based) so a UI
	// can show "3 of 12" without correlating digests.
	Index int `json:"index,omitempty"`
	Count int `json:"count,omitempty"`
	// Chunks and ChunksDone report the stored objects behind this item and how
	// many have arrived, for operations that transfer a file in pieces. A
	// whole-blob asset has one chunk; a large asset has many, and showing
	// "12 of 47" is often more legible than a byte figure.
	Chunks     int `json:"chunks,omitempty"`
	ChunksDone int `json:"chunksDone,omitempty"`
	// Stage is set on EventStage: the operation moved to a different kind of
	// work. Index/Count position it when the stage can count itself.
	Stage Stage `json:"stage,omitempty"`
	// ReusedItems and ReusedBytes accompany a plan event: how many of the
	// planned items need no transfer, and how much that saves, because their
	// content is already stored or is shared with another item in the same
	// operation.
	ReusedItems int   `json:"reusedItems,omitempty"`
	ReusedBytes int64 `json:"reusedBytes,omitempty"`
	// WireBytes is the cumulative number of bytes actually received or sent
	// for this item, including bytes later discarded and retried. Bytes is the
	// unique useful progress towards Total; keeping the two separate prevents
	// a retry from making completion exceed 100 percent.
	WireBytes int64 `json:"wireBytes,omitempty"`
	// Part identifies the sub-unit currently moving (for example an object or
	// chunk digest). PartBytes/PartTotal are cumulative for that part, while
	// PartIndex/PartCount place it within the item when those are known.
	PartID        string `json:"partId,omitempty"`
	PartBytes     int64  `json:"partBytes,omitempty"`
	PartTotal     int64  `json:"partTotal,omitempty"`
	PartWireBytes int64  `json:"partWireBytes,omitempty"`
	PartIndex     int    `json:"partIndex,omitempty"`
	PartCount     int    `json:"partCount,omitempty"`
}

// Stage names the part of an operation that is running. Operations are not
// uniform: a load resolves what each asset is made of, transfers what is
// missing, then writes files, and those have different bottlenecks, different
// units, and different reasons to be slow. A consumer that shows the stage
// tells the reader which of those it is waiting on.
type Stage string

const (
	// StageResolving is reading the index and the metadata that says what
	// each asset is made of.
	StageResolving Stage = "resolving"
	// StageTransferring is moving object bytes to or from a storage remote.
	StageTransferring Stage = "downloading"
	// StageWriting is producing files in the working tree.
	StageWriting Stage = "writing"
	// StageVerifying is reading and hashing local bytes before a destructive
	// operation such as unload is allowed to replace them.
	StageVerifying       Stage = "verifying"
	StagePreflight       Stage = "preflight"
	StageMaterialization Stage = "materialization"
	StageRendering       Stage = "rendering"
	StageActivation      Stage = "activation"
	StageLifecycle       Stage = "lifecycle"
	StageReadiness       Stage = "readiness"
	StageRollback        Stage = "rollback"
)

// Phase classifies item work.
type Phase string

const (
	// PhaseTransfer is bytes moving to or from a storage remote. Bytes/Total
	// are transfer figures for the item.
	PhaseTransfer Phase = "transfer"
	// PhaseWrite is producing the file in the working tree.
	PhaseWrite Phase = "write"
	// PhaseVerify is reading and hashing bytes without transferring them.
	PhaseVerify Phase = "verify"
)

// ProgressFunc receives events. Delivery is serialized (never two calls at
// once) but may come from worker goroutines of a concurrent operation. It
// must be fast and must not block; expensive rendering should be offloaded
// by the consumer.
type ProgressFunc func(Event)

// Discard ignores events.
func Discard(Event) {}
