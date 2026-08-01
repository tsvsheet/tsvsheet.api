// Package conformance is the document plane's shared behaviour suite: one
// table of assertions every document.Port implementation must satisfy,
// runnable against any of them.
//
// It exists as a package rather than a test file because the suite is the
// contract. An implementation that cannot run it — a future port over object
// storage, a database, another server — has not been shown to be
// interchangeable with the ones that can, and "interchangeable" is the whole
// claim the document package makes. This mirrors testing/fstest in the
// standard library: the specification ships as something you can execute.
package conformance

import (
	"testing"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// Seed is the document every behaviour starts from.
const Seed = "1\t2\n3\t=A1+B1\n"

// Factory builds a Port over a namespace seeded with the given documents. A
// factory returns a port with no shared state between calls, so one behaviour
// does not observe another's writes.
type Factory func(t *testing.T, files map[string]string) document.Port

// Seeded is the fixture set every behaviour starts from.
func Seeded() map[string]string { return map[string]string{"a.tsvt": Seed} }
