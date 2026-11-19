// Fetch planning: which objects a fetch sends, which commits form the new
// shallow boundary, and which old shallow commits are now fully held.
//
// The rules, worked out against the kit client:
//
//   - The client already holds everything reachable from its `have` commits,
//     except that a commit it names in its `shallow` list is held shallow:
//     the commit, its tree and its blobs, but not its parents. The planner
//     never re-sends a held object.
//   - `deepen N` is a TOTAL depth from the wants, not an increment: a depth-1
//     clone deepened by 2 holds depth 2, so the parent at depth 2 is sent and
//     the grandparent at depth 3 is not.
//   - `deepen-since T` keeps only commits stamped T or later; `deepen-not
//     <ref>` keeps nothing reachable from that ref. A commit whose parent was
//     cut off by any of these limits is a new shallow boundary.
//   - A commit the client already held shallow whose parents are all now in
//     the wanted set is unshallow: its boundary moved past it.
package gitproto

import (
	"bytes"
	"fmt"
	"sort"

	"forge/internal/store"
)

// FetchOptions carries a fetch request's limits.
type FetchOptions struct {
	Depth       int      // total depth from the wants; 0 means unlimited
	Since       int64    // deepen-since timestamp; 0 means none
	NotRefs     []string // deepen-not ref names
	FilterBlobs bool     // filter blob:none: send no blobs
	FilterTrees bool     // filter tree:0: send no trees and no blobs
}

// FetchPlan is the answer to a fetch: the objects to send, the new shallow
// boundary commits, and the old shallow commits that are now fully held.
type FetchPlan struct {
	Objects   []*store.Object // sorted by object id, for a deterministic pack
	Shallow   []store.OID
	Unshallow []store.OID
}

// PlanFetch computes a fetch plan for a store. Every want must be in the
// store; a missing want is an error. Objects the client already holds are
// never sent, and the plan is deterministic for the same inputs.
func PlanFetch(st *store.Store, wants, haves, clientShallow []store.OID, opts FetchOptions) (*FetchPlan, error) {
	for _, want := range wants {
		if !st.Has(want) {
			return nil, fmt.Errorf("gitproto: fetch wants %s, which is not in the store", want.Hex())
		}
	}

	shallowSet := make(map[store.OID]bool, len(clientShallow))
	for _, oid := range clientShallow {
		shallowSet[oid] = true
	}

	// Everything reachable from a deepen-not ref is excluded from the walk.
	notExcluded := make(map[store.OID]bool)
	for _, name := range opts.NotRefs {
		tip, ok := st.GetRef(name)
		if !ok {
			continue
		}
		for oid := range walkClosure(st, []store.OID{tip}, nil) {
			notExcluded[oid] = true
		}
	}

	// Everything the client already holds, with its shallow commits as
	// leaves: their trees are held, their parents are not.
	held := walkClosure(st, haves, shallowSet)

	// The wanted set, depth and time limited.
	wanted, commits, err := walkWanted(st, wants, opts, notExcluded)
	if err != nil {
		return nil, err
	}

	plan := &FetchPlan{}
	for oid, obj := range wanted {
		if held[oid] != nil {
			continue
		}
		plan.Objects = append(plan.Objects, obj)
	}
	// Objects sort by raw identity bytes. Hex order is the same order
	// (each byte maps to two hex digits monotonically), so the pack
	// bytes are unchanged, but no hex string is allocated per compare.
	sort.Slice(plan.Objects, func(i, j int) bool {
		a := plan.Objects[i].OID()
		b := plan.Objects[j].OID()
		return bytes.Compare(a[:], b[:]) < 0
	})

	// The new shallow boundary, only when a limit cut the walk short: a sent
	// commit whose parent did not make it into the wanted set.
	limited := opts.Depth > 0 || opts.Since > 0 || len(opts.NotRefs) > 0
	if limited {
		sent := make(map[store.OID]bool, len(plan.Objects))
		for _, obj := range plan.Objects {
			sent[obj.OID()] = true
		}
		for oid := range commits {
			if !sent[oid] {
				continue
			}
			commit, err := ParseCommit(wanted[oid].Data)
			if err != nil {
				continue
			}
			for _, parent := range commit.Parents {
				if !commits[parent] {
					plan.Shallow = append(plan.Shallow, oid)
					break
				}
			}
		}
		sort.Slice(plan.Shallow, func(i, j int) bool { return bytes.Compare(plan.Shallow[i][:], plan.Shallow[j][:]) < 0 })
	}

	// Old shallow commits whose parents are all now wanted are unshallow.
	for _, oid := range clientShallow {
		obj, ok := wanted[oid]
		if !ok || obj.Type != store.TypeCommit {
			continue
		}
		commit, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		complete := true
		for _, parent := range commit.Parents {
			if !commits[parent] {
				complete = false
				break
			}
		}
		if complete {
			plan.Unshallow = append(plan.Unshallow, oid)
		}
	}
	sort.Slice(plan.Unshallow, func(i, j int) bool { return bytes.Compare(plan.Unshallow[i][:], plan.Unshallow[j][:]) < 0 })

	return plan, nil
}

// walkClosure returns every object reachable from roots, walking commits
// (tree and parents), tags (their target) and trees (their entries, skipping
// gitlinks). A commit in stopParents is a leaf: its tree is included but its
// parents are not walked. Objects missing from the store are skipped, never
// an error: a caller that needs a seed to exist checks that itself.
func walkClosure(st *store.Store, roots []store.OID, stopParents map[store.OID]bool) map[store.OID]*store.Object {
	out := make(map[store.OID]*store.Object)
	queue := append([]store.OID{}, roots...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if out[oid] != nil {
			continue
		}
		obj, ok := st.Get(oid)
		if !ok {
			continue
		}
		out[oid] = obj
		switch obj.Type {
		case store.TypeCommit:
			commit, err := ParseCommit(obj.Data)
			if err != nil {
				continue
			}
			walkTreeInto(st, commit.Tree, out)
			if !stopParents[oid] {
				queue = append(queue, commit.Parents...)
			}
		case store.TypeTag:
			tag, err := ParseTag(obj.Data)
			if err != nil {
				continue
			}
			queue = append(queue, tag.Object)
		case store.TypeTree:
			walkTreeEntries(st, obj.Data, out)
		}
	}
	return out
}

// walkTreeInto adds a tree and everything under it to out.
func walkTreeInto(st *store.Store, root store.OID, out map[store.OID]*store.Object) {
	obj, ok := st.Get(root)
	if !ok || obj.Type != store.TypeTree {
		return
	}
	if out[root] != nil {
		return
	}
	out[root] = obj
	walkTreeEntries(st, obj.Data, out)
}

// walkTreeEntries adds the entries of one tree's bytes to out, recursing
// into subtrees. Gitlinks are skipped: they name commits the repository
// does not hold.
func walkTreeEntries(st *store.Store, data []byte, out map[store.OID]*store.Object) {
	entries, err := ParseTree(data)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsGitlink() {
			continue
		}
		if out[entry.OID] != nil {
			continue
		}
		obj, ok := st.Get(entry.OID)
		if !ok {
			continue
		}
		out[entry.OID] = obj
		if obj.Type == store.TypeTree {
			walkTreeEntries(st, obj.Data, out)
		}
	}
}

// walkWanted walks from the wants under the fetch limits and returns the
// wanted objects plus the set of wanted commits. The walk is breadth-first,
// so the first visit to a commit is at its shallowest depth: a parent is
// enqueued only when its depth is within the limit, its timestamp is within
// the time limit, and it is not excluded by a deepen-not ref.
func walkWanted(st *store.Store, wants []store.OID, opts FetchOptions, notExcluded map[store.OID]bool) (map[store.OID]*store.Object, map[store.OID]bool, error) {
	wanted := make(map[store.OID]*store.Object)
	commits := make(map[store.OID]bool)

	type item struct {
		oid   store.OID
		depth int
	}
	queue := []item{}
	// depthOf records the shallowest depth each commit was reached at;
	// processed records the depth at which its parents were last walked.
	depthOf := make(map[store.OID]int)
	processed := make(map[store.OID]int)
	for _, want := range wants {
		if _, seen := depthOf[want]; !seen {
			depthOf[want] = 1
			queue = append(queue, item{oid: want, depth: 1})
		}
	}

	// include adds one object to the wanted set; trees are walked on the
	// spot, since they carry no depth of their own. The filter applies to
	// objects reached by walking (tree entries); an object the client
	// asked for directly — a lazy blob fetch from a partial clone — is
	// always sent, even when the request still carries the filter.
	direct := make(map[store.OID]bool, len(wants))
	for _, want := range wants {
		direct[want] = true
	}
	var includeTree func(oid store.OID) error
	includeTree = func(oid store.OID) error {
		if wanted[oid] != nil {
			return nil
		}
		obj, ok := st.Get(oid)
		if !ok || obj.Type != store.TypeTree {
			return nil
		}
		wanted[oid] = obj
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsGitlink() {
				continue
			}
			child, ok := st.Get(entry.OID)
			if !ok {
				continue
			}
			switch child.Type {
			case store.TypeTree:
				if opts.FilterTrees {
					continue
				}
				if err := includeTree(entry.OID); err != nil {
					return err
				}
			case store.TypeBlob:
				if opts.FilterBlobs || opts.FilterTrees {
					continue
				}
				wanted[entry.OID] = child
			}
		}
		return nil
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		// A commit reached first at a deeper depth is re-walked when a
		// shallower path to it appears: diamond histories must not lose a
		// parent to a depth cut taken on the longer path.
		if best, ok := depthOf[current.oid]; ok && current.depth > best {
			continue
		}
		if processed[current.oid] <= current.depth && wanted[current.oid] != nil {
			continue
		}
		obj, ok := st.Get(current.oid)
		if !ok {
			return nil, nil, fmt.Errorf("gitproto: fetch walk hit a missing object %s", current.oid.Hex())
		}
		processed[current.oid] = current.depth
		switch obj.Type {
		case store.TypeCommit:
			wanted[obj.OID()] = obj
			commits[obj.OID()] = true
			commit, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, nil, err
			}
			if !opts.FilterTrees {
				if err := includeTree(commit.Tree); err != nil {
					return nil, nil, err
				}
			}
			for _, parent := range commit.Parents {
				if commits[parent] && processed[parent] <= current.depth+1 {
					continue
				}
				if opts.Depth > 0 && current.depth+1 > opts.Depth {
					continue
				}
				if notExcluded[parent] {
					continue
				}
				if opts.Since > 0 {
					parentObj, ok := st.Get(parent)
					if !ok {
						continue
					}
					parentCommit, err := ParseCommit(parentObj.Data)
					if err != nil || parentCommit.Committer < opts.Since {
						continue
					}
				}
				if best, seen := depthOf[parent]; seen && current.depth+1 >= best {
					continue
				}
				depthOf[parent] = current.depth + 1
				queue = append(queue, item{oid: parent, depth: current.depth + 1})
			}
		case store.TypeTag:
			wanted[obj.OID()] = obj
			tag, err := ParseTag(obj.Data)
			if err != nil {
				return nil, nil, err
			}
			target, ok := st.Get(tag.Object)
			if !ok {
				return nil, nil, fmt.Errorf("gitproto: tag %s points at a missing object %s", obj.ID(), tag.Object.Hex())
			}
			switch target.Type {
			case store.TypeCommit:
				queue = append(queue, item{oid: tag.Object, depth: current.depth})
			case store.TypeTree:
				if !opts.FilterTrees {
					if err := includeTree(tag.Object); err != nil {
						return nil, nil, err
					}
				}
			case store.TypeBlob:
				if !opts.FilterBlobs && !opts.FilterTrees {
					wanted[tag.Object] = target
				}
			case store.TypeTag:
				queue = append(queue, item{oid: tag.Object, depth: current.depth})
			}
		case store.TypeTree:
			if !opts.FilterTrees || direct[obj.OID()] {
				if err := includeTree(obj.OID()); err != nil {
					return nil, nil, err
				}
			}
		case store.TypeBlob:
			if !opts.FilterBlobs && !opts.FilterTrees || direct[obj.OID()] {
				wanted[obj.OID()] = obj
			}
		}
	}
	return wanted, commits, nil
}
