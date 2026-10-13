// Package screening decides which inbound calls are blocked. It keeps
// immutable in-memory indexes of the block/allow lists and contacts that
// are rebuilt after every change and swapped atomically, so lookups on
// the call path never wait for a lock or the database.
package screening

import (
	"slices"
	"strings"
)

// Ref is what a lookup found.
type Ref struct {
	EntryID  int64
	SourceID int64
	Source   string // source name, e.g. "PhoneBlock"
	Name     string // contact name or entry label
	Pattern  string
}

// Index maps E.164 numbers and number ranges ("+49201123*") to entries.
// Exact numbers are a hash map; ranges are one map per prefix length,
// checked longest first (there are only a few distinct lengths).
type Index struct {
	exact  map[string]Ref
	prefix map[string]Ref
	lens   []int // distinct prefix lengths, descending
}

func newIndex(sizeHint int) *Index {
	return &Index{exact: make(map[string]Ref, sizeHint), prefix: map[string]Ref{}}
}

// add inserts a pattern; the first entry for a pattern wins.
func (ix *Index) add(pattern string, r Ref) {
	r.Pattern = pattern
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		if _, dup := ix.prefix[p]; !dup {
			ix.prefix[p] = r
			if !slices.Contains(ix.lens, len(p)) {
				ix.lens = append(ix.lens, len(p))
			}
		}
		return
	}
	if _, dup := ix.exact[pattern]; !dup {
		ix.exact[pattern] = r
	}
}

func (ix *Index) seal() {
	slices.SortFunc(ix.lens, func(a, b int) int { return b - a })
}

// Lookup finds an exact match, else the longest matching range.
func (ix *Index) Lookup(e164 string) (Ref, bool) {
	if ix == nil {
		return Ref{}, false
	}
	if r, ok := ix.exact[e164]; ok {
		return r, true
	}
	for _, l := range ix.lens {
		if l <= len(e164) {
			if r, ok := ix.prefix[e164[:l]]; ok {
				return r, true
			}
		}
	}
	return Ref{}, false
}

// Len is the number of distinct patterns.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	return len(ix.exact) + len(ix.prefix)
}
