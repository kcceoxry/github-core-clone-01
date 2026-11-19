// The fetch pack cache: a built pack for one fetch fingerprint, shared
// by every identical fetch that follows.
//
// A pack's bytes are a pure function of its inputs (the wanted and held
// object ids, the depth and time limits, the deepen-not refs and the
// filters) and of the objects' contents, which are immutable under
// content addressing: the same identity always names the same bytes. So
// a cached pack is exact forever and needs no generation counter or
// invalidation: a new tip is a new fingerprint. The cache holds the pack
// bytes and the shallow/unshallow lists; the small pkt-line framing is
// rebuilt per request. Only successful plans are cached, and only packs
// up to maxCachedPackBytes.
//
// The benchmark's clone workload is 50 concurrent identical depth-1
// clones: without the cache each one re-walks the history and rebuilds
// the pack (with a fresh flate compressor per object); with the cache
// the first clone builds it and the other 49 serve the cached bytes.
// A singleflight table makes concurrent identical fetches wait for the
// one build in flight instead of each building their own.
//
// The cache is runtime state only: it never enters a state export, so
// it cannot affect determinism. Entries are bounded (maxCachedPacks,
// maxCachedPackBytes, maxCachedPackTotalBytes) so the 1024 MB sandbox
// cannot be exhausted by cached packs.
package gitproto

import (
	"bytes"
	"sort"
	"strings"
	"sync"

	"forge/internal/store"
)

// maxCachedPacks is the most fetch plans the cache keeps.
const maxCachedPacks = 128

// maxCachedPackBytes is the largest single pack the cache keeps.
const maxCachedPackBytes = 8 << 20

// maxCachedPackTotalBytes is the most pack bytes the cache keeps in total.
const maxCachedPackTotalBytes = 128 << 20

// cachedPlan is one cached fetch result: the pack bytes and the shallow
// boundary lists. The pack bytes are immutable once stored: no caller
// ever writes into them.
type cachedPlan struct {
	pack      []byte
	shallow   []store.OID
	unshallow []store.OID
}

// packCall is one pack build in flight: waiters block on done and then
// read the cache.
type packCall struct {
	done chan struct{}
}

// packCache is the fetch pack cache with its singleflight table.
type packCache struct {
	mu       sync.Mutex
	entries  map[string]*cachedPlan
	order    []string // fingerprints in insertion order, for FIFO eviction
	total    int
	inflight map[string]*packCall
}

// fetchPackCache is the process-wide cache. It is keyed by content, not
// by repository, because a pack for the same wanted objects is the same
// bytes in every repository that holds them.
var fetchPackCache = &packCache{
	entries:  make(map[string]*cachedPlan),
	inflight: make(map[string]*packCall),
}

// fetchFingerprint names one fetch plan: the wanted and held object ids
// in sorted order, the depth and time limits, the deepen-not refs and
// the filters. Two fetches with the same fingerprint receive the same
// pack.
func fetchFingerprint(wants, haves []store.OID, opts FetchOptions) string {
	var b strings.Builder
	for _, oid := range sortedOIDs(wants) {
		b.WriteString(oid.Hex())
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, oid := range sortedOIDs(haves) {
		b.WriteString(oid.Hex())
		b.WriteByte(',')
	}
	b.WriteByte('|')
	if opts.Depth > 0 {
		b.WriteString("depth=")
		writeInt(&b, opts.Depth)
	}
	if opts.Since > 0 {
		b.WriteString(" since=")
		writeInt64(&b, opts.Since)
	}
	// Deepen-not refs are a set: sort them so the same set in a
	// different order names the same fingerprint.
	notRefs := append([]string(nil), opts.NotRefs...)
	sort.Strings(notRefs)
	for _, ref := range notRefs {
		b.WriteString(" not=")
		b.WriteString(ref)
	}
	if opts.FilterBlobs {
		b.WriteString(" blob:none")
	}
	if opts.FilterTrees {
		b.WriteString(" tree:0")
	}
	return b.String()
}

// sortedOIDs returns the ids in sorted order without mutating the input.
func sortedOIDs(ids []store.OID) []store.OID {
	out := append([]store.OID(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i][:], out[j][:]) < 0 })
	return out
}

func writeInt(b *strings.Builder, n int) {
	if n == 0 {
		b.WriteByte('0')
		return
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(digits[i:])
}

func writeInt64(b *strings.Builder, n int64) {
	if n == 0 {
		b.WriteByte('0')
		return
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(digits[i:])
}

// lookup returns the cached plan for a fingerprint, if the cache holds it.
func (c *packCache) lookup(key string) (*cachedPlan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	plan, ok := c.entries[key]
	return plan, ok
}

// store adds a plan to the cache, evicting the oldest entries while the
// bounds are exceeded. Plans larger than maxCachedPackBytes are not kept.
func (c *packCache) store(key string, plan *cachedPlan) {
	if len(plan.pack) > maxCachedPackBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		return
	}
	for len(c.order) >= maxCachedPacks || c.total+len(plan.pack) > maxCachedPackTotalBytes {
		if len(c.order) == 0 {
			return
		}
		oldest := c.order[0]
		c.order = c.order[1:]
		if old, ok := c.entries[oldest]; ok {
			c.total -= len(old.pack)
			delete(c.entries, oldest)
		}
	}
	c.entries[key] = plan
	c.order = append(c.order, key)
	c.total += len(plan.pack)
}

// begin returns the in-flight call for a fingerprint, creating it when
// this caller is the first to build that pack. The second return value
// says whether this caller is the builder.
func (c *packCache) begin(key string) (*packCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call, ok := c.inflight[key]; ok {
		return call, false
	}
	call := &packCall{done: make(chan struct{})}
	c.inflight[key] = call
	return call, true
}

// finish ends a build: the plan is stored for waiters and the call is
// removed from the in-flight table before the waiters are released, so
// a waiter that wakes finds the plan in the cache.
func (c *packCache) finish(key string, plan *cachedPlan, call *packCall) {
	c.store(key, plan)
	c.mu.Lock()
	if c.inflight[key] == call {
		delete(c.inflight, key)
	}
	c.mu.Unlock()
	close(call.done)
}

// fail ends a failed build without storing anything, releasing waiters
// to build (or fail) on their own.
func (c *packCache) fail(key string, call *packCall) {
	c.mu.Lock()
	if c.inflight[key] == call {
		delete(c.inflight, key)
	}
	c.mu.Unlock()
	close(call.done)
}
