package manifest

import (
	"context"
	"runtime/metrics"
	"sync"
	"time"
)

// budgetContext is a context.Context whose Done method is called by the
// gopher-lua main loop before every VM instruction. That call is used as the
// instruction hook: it counts steps, samples process allocation every
// memCheckInterval steps, and closes its own channel when a limit trips. The
// VM then raises a Lua error on the very next instruction (including inside
// pcall handlers), so a tripped budget cannot be swallowed by the script.
type budgetContext struct {
	parent context.Context

	steps        uint64
	maxSteps     uint64
	allocStart   uint64
	maxAllocs    uint64
	sample       []metrics.Sample
	deadline     time.Time
	haveDeadline bool

	once    sync.Once
	done    chan struct{}
	tripped *ErrLimitExceeded
}

const memCheckInterval = 8

var allocSampleName = "/gc/heap/allocs:bytes"

func readAllocs(s []metrics.Sample) uint64 {
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

func newBudgetContext(parent context.Context, maxSteps, maxAllocs uint64, wall time.Duration) *budgetContext {
	b := &budgetContext{
		parent:    parent,
		maxSteps:  maxSteps,
		maxAllocs: maxAllocs,
		sample:    []metrics.Sample{{Name: allocSampleName}},
		done:      make(chan struct{}),
	}
	b.allocStart = readAllocs(b.sample)
	if wall > 0 {
		b.deadline = time.Now().Add(wall)
		b.haveDeadline = true
	}
	if pd, ok := parent.Deadline(); ok && (!b.haveDeadline || pd.Before(b.deadline)) {
		b.deadline = pd
		b.haveDeadline = true
	}
	return b
}

func (b *budgetContext) trip(kind LimitKind, detail string) {
	b.once.Do(func() {
		b.tripped = &ErrLimitExceeded{Kind: kind, Detail: detail}
		close(b.done)
	})
}

// check performs one accounting step. It is cheap on the hot path: a counter
// increment, and every memCheckInterval steps one runtime/metrics read plus a
// time read.
func (b *budgetContext) check() {
	b.steps++
	if b.steps > b.maxSteps {
		b.trip(LimitInstructions, "instruction budget exhausted")
		return
	}
	if b.steps%memCheckInterval != 0 {
		return
	}
	if b.maxAllocs > 0 {
		now := readAllocs(b.sample)
		if now-b.allocStart > b.maxAllocs {
			b.trip(LimitMemory, "allocation budget exhausted")
			return
		}
	}
	if b.haveDeadline && !time.Now().Before(b.deadline) {
		b.trip(LimitWallClock, "deadline exceeded")
		return
	}
	select {
	case <-b.parent.Done():
		b.trip(LimitWallClock, b.parent.Err().Error())
	default:
	}
}

// Deadline implements context.Context.
func (b *budgetContext) Deadline() (time.Time, bool) { return b.deadline, b.haveDeadline }

// Done implements context.Context and doubles as the per-instruction hook.
func (b *budgetContext) Done() <-chan struct{} {
	b.check()
	return b.done
}

// Err implements context.Context.
func (b *budgetContext) Err() error {
	select {
	case <-b.done:
		return b.tripped
	default:
		return nil
	}
}

// Value implements context.Context.
func (b *budgetContext) Value(key any) any { return b.parent.Value(key) }

// limitErr returns the tripped limit, if any.
func (b *budgetContext) limitErr() *ErrLimitExceeded {
	select {
	case <-b.done:
		return b.tripped
	default:
		return nil
	}
}
