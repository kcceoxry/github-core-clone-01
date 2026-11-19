package gitproto_test

import (
	"bytes"
	"fmt"
	"sort"
	"testing"

	"forge/internal/gitproto"
	"forge/internal/store"
)

// testRepo is a small linear history: three commits, each changing one file,
// plus an annotated tag on the second commit.
type testRepo struct {
	st *store.Store
	b1, b2, b3 store.OID
	t1, t2, t3 store.OID
	c1, c2, c3 store.OID
	tag        store.OID
}

func buildTree(entries []struct {
	mode string
	name string
	oid  store.OID
}) []byte {
	sort.Slice(entries, func(i, j int) bool {
		left, right := entries[i].name, entries[j].name
		if entries[i].mode == "40000" {
			left += "/"
		}
		if entries[j].mode == "40000" {
			right += "/"
		}
		return left < right
	})
	var buf bytes.Buffer
	for _, entry := range entries {
		fmt.Fprintf(&buf, "%s %s\x00", entry.mode, entry.name)
		buf.Write(entry.oid[:])
	}
	return buf.Bytes()
}

func buildCommit(tree store.OID, parents []store.OID, timestamp int64, message string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree.Hex())
	for _, parent := range parents {
		fmt.Fprintf(&buf, "parent %s\n", parent.Hex())
	}
	fmt.Fprintf(&buf, "author A U Thor <a@example.invalid> %d +0000\n", timestamp)
	fmt.Fprintf(&buf, "committer A U Thor <a@example.invalid> %d +0000\n", timestamp)
	fmt.Fprintf(&buf, "\n%s\n", message)
	return buf.Bytes()
}

func buildTestRepo(t *testing.T) *testRepo {
	t.Helper()
	st := store.New()
	repo := &testRepo{st: st}

	put := func(typ store.ObjectType, data []byte) store.OID {
		obj := store.NewObject(typ, data)
		st.Put(obj)
		return obj.OID()
	}

	repo.b1 = put(store.TypeBlob, []byte("hello\n"))
	repo.b2 = put(store.TypeBlob, []byte("world\n"))
	repo.b3 = put(store.TypeBlob, []byte("third\n"))

	repo.t1 = put(store.TypeTree, buildTree([]struct {
		mode string
		name string
		oid  store.OID
	}{{"100644", "a.txt", repo.b1}}))
	repo.t2 = put(store.TypeTree, buildTree([]struct {
		mode string
		name string
		oid  store.OID
	}{{"100644", "a.txt", repo.b2}, {"100644", "b.txt", repo.b1}}))
	repo.t3 = put(store.TypeTree, buildTree([]struct {
		mode string
		name string
		oid  store.OID
	}{{"100644", "a.txt", repo.b3}, {"100644", "b.txt", repo.b1}}))

	repo.c1 = put(store.TypeCommit, buildCommit(repo.t1, nil, 1700000000, "first"))
	repo.c2 = put(store.TypeCommit, buildCommit(repo.t2, []store.OID{repo.c1}, 1700000100, "second"))
	repo.c3 = put(store.TypeCommit, buildCommit(repo.t3, []store.OID{repo.c2}, 1700000200, "third"))

	tagData := fmt.Sprintf("object %s\ntype commit\ntag v1\ntagger A U Thor <a@example.invalid> 1700000100 +0000\n\nrelease v1\n", repo.c2.Hex())
	repo.tag = put(store.TypeTag, []byte(tagData))

	st.SetRef("refs/heads/main", repo.c3)
	st.SetRef("refs/tags/v1", repo.tag)
	return repo
}

func planOIDs(plan *gitproto.FetchPlan) map[string]bool {
	out := make(map[string]bool)
	for _, obj := range plan.Objects {
		out[obj.OID().Hex()] = true
	}
	return out
}

func TestPlanFetchFull(t *testing.T) {
	repo := buildTestRepo(t)
	plan, err := gitproto.PlanFetch(repo.st, []store.OID{repo.c3}, nil, nil, gitproto.FetchOptions{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := planOIDs(plan)
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.c3, repo.t1, repo.t2, repo.t3, repo.b1, repo.b2, repo.b3} {
		if !got[oid.Hex()] {
			t.Fatalf("full fetch is missing %s", oid.Hex())
		}
	}
	if len(plan.Objects) != 9 {
		t.Fatalf("full fetch sent %d objects, want 9", len(plan.Objects))
	}
	if len(plan.Shallow) != 0 || len(plan.Unshallow) != 0 {
		t.Fatalf("full fetch has shallow=%v unshallow=%v, want none", plan.Shallow, plan.Unshallow)
	}
}

func TestPlanFetchIncremental(t *testing.T) {
	repo := buildTestRepo(t)
	plan, err := gitproto.PlanFetch(repo.st, []store.OID{repo.c3}, []store.OID{repo.c1}, nil, gitproto.FetchOptions{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := planOIDs(plan)
	// Only the new objects: c2, c3, t2, t3, b2, b3. The old tip, its tree
	// and its blob must not be re-sent.
	for _, oid := range []store.OID{repo.c2, repo.c3, repo.t2, repo.t3, repo.b2, repo.b3} {
		if !got[oid.Hex()] {
			t.Fatalf("incremental fetch is missing %s", oid.Hex())
		}
	}
	for _, oid := range []store.OID{repo.c1, repo.t1, repo.b1} {
		if got[oid.Hex()] {
			t.Fatalf("incremental fetch re-sent already-held %s", oid.Hex())
		}
	}
}

func TestPlanFetchNothingNew(t *testing.T) {
	repo := buildTestRepo(t)
	plan, err := gitproto.PlanFetch(repo.st, []store.OID{repo.c1}, []store.OID{repo.c1}, nil, gitproto.FetchOptions{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Objects) != 0 {
		t.Fatalf("fetch of a held tip sent %d objects, want 0", len(plan.Objects))
	}
}

func TestPlanFetchDepth1(t *testing.T) {
	repo := buildTestRepo(t)
	plan, err := gitproto.PlanFetch(repo.st, []store.OID{repo.c3}, nil, nil, gitproto.FetchOptions{Depth: 1})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := planOIDs(plan)
	for _, oid := range []store.OID{repo.c3, repo.t3, repo.b3, repo.b1} {
		if !got[oid.Hex()] {
			t.Fatalf("depth-1 fetch is missing %s", oid.Hex())
		}
	}
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.t1, repo.t2, repo.b2} {
		if got[oid.Hex()] {
			t.Fatalf("depth-1 fetch carried history beyond depth 1: %s", oid.Hex())
		}
	}
	if len(plan.Shallow) != 1 || plan.Shallow[0] != repo.c3 {
		t.Fatalf("depth-1 shallow = %v, want [%s]", plan.Shallow, repo.c3.Hex())
	}
}

func TestPlanFetchDeepen(t *testing.T) {
	repo := buildTestRepo(t)
	// A depth-1 clone holds c3 shallow; deepen 2 must deliver c2 (depth 2)
	// but not c1 (depth 3), and move the boundary to c2.
	plan, err := gitproto.PlanFetch(repo.st,
		[]store.OID{repo.c3}, []store.OID{repo.c3}, []store.OID{repo.c3},
		gitproto.FetchOptions{Depth: 2})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := planOIDs(plan)
	if !got[repo.c2.Hex()] {
		t.Fatalf("deepen 2 did not deliver the parent")
	}
	if got[repo.c1.Hex()] {
		t.Fatalf("deepen 2 delivered the grandparent")
	}
	if len(plan.Shallow) != 1 || plan.Shallow[0] != repo.c2 {
		t.Fatalf("deepen shallow = %v, want [%s]", plan.Shallow, repo.c2.Hex())
	}
	if len(plan.Unshallow) != 1 || plan.Unshallow[0] != repo.c3 {
		t.Fatalf("deepen unshallow = %v, want [%s]", plan.Unshallow, repo.c3.Hex())
	}
}

func TestPlanFetchShallowNewTip(t *testing.T) {
	repo := buildTestRepo(t)
	// A depth-1 clone holds c2 shallow; a depth-1 fetch of the new tip c3
	// must deliver c3 only, not the held parent and not the old history.
	plan, err := gitproto.PlanFetch(repo.st,
		[]store.OID{repo.c3}, []store.OID{repo.c2}, []store.OID{repo.c2},
		gitproto.FetchOptions{Depth: 1})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := planOIDs(plan)
	if !got[repo.c3.Hex()] {
		t.Fatalf("shallow fetch did not deliver the new tip")
	}
	for _, oid := range []store.OID{repo.c1, repo.c2} {
		if got[oid.Hex()] {
			t.Fatalf("shallow fetch delivered already-held history: %s", oid.Hex())
		}
	}
	if len(plan.Shallow) != 1 || plan.Shallow[0] != repo.c3 {
		t.Fatalf("shallow = %v, want [%s]", plan.Shallow, repo.c3.Hex())
	}
}

func TestPlanFetchFilterBlobNone(t *testing.T) {
	repo := buildTestRepo(t)
	plan, err := gitproto.PlanFetch(repo.st, []store.OID{repo.c3}, nil, nil, gitproto.FetchOptions{FilterBlobs: true})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, obj := range plan.Objects {
		if obj.Type == store.TypeBlob {
			t.Fatalf("blob:none fetch sent blob %s", obj.ID())
		}
	}
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.c3, repo.t1, repo.t2, repo.t3} {
		found := false
		for _, obj := range plan.Objects {
			if obj.OID() == oid {
				found = true
			}
		}
		if !found {
			t.Fatalf("blob:none fetch is missing %s", oid.Hex())
		}
	}
}

// TestPlanFetchDirectBlobWithFilter pins the lazy-fetch rule: a blob the
// client asked for directly is sent even when the request still carries
// filter=blob:none (the filter only applies to walked tree entries).
func TestPlanFetchDirectBlobWithFilter(t *testing.T) {
	repo := buildTestRepo(t)
	plan, err := gitproto.PlanFetch(repo.st, []store.OID{repo.b2}, nil, nil, gitproto.FetchOptions{FilterBlobs: true})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Objects) != 1 || plan.Objects[0].OID() != repo.b2 {
		t.Fatalf("direct blob fetch sent %v, want only %s", planOIDs(plan), repo.b2.Hex())
	}
}

func TestPlanFetchDeepenSince(t *testing.T) {
	repo := buildTestRepo(t)
	// deepen-since 1700000100 keeps c3 (1700000200) and c2 (1700000100),
	// but not c1 (1700000000); c2 becomes the boundary.
	plan, err := gitproto.PlanFetch(repo.st, []store.OID{repo.c3}, nil, nil, gitproto.FetchOptions{Since: 1700000100})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := planOIDs(plan)
	if !got[repo.c2.Hex()] || !got[repo.c3.Hex()] {
		t.Fatalf("deepen-since is missing a recent commit")
	}
	if got[repo.c1.Hex()] {
		t.Fatalf("deepen-since delivered a commit older than the cutoff")
	}
	if len(plan.Shallow) != 1 || plan.Shallow[0] != repo.c2 {
		t.Fatalf("deepen-since shallow = %v, want [%s]", plan.Shallow, repo.c2.Hex())
	}
}

func TestPlanFetchMissingWant(t *testing.T) {
	repo := buildTestRepo(t)
	missing, _ := store.OIDFromHex("1111111111111111111111111111111111111111")
	if _, err := gitproto.PlanFetch(repo.st, []store.OID{missing}, nil, nil, gitproto.FetchOptions{}); err == nil {
		t.Fatalf("expected an error for a missing want, got none")
	}
}

func TestPeelTag(t *testing.T) {
	repo := buildTestRepo(t)
	peeled, typ, err := gitproto.PeelTag(repo.st, repo.tag)
	if err != nil {
		t.Fatalf("peel: %v", err)
	}
	if peeled != repo.c2 {
		t.Fatalf("tag peeled to %s, want %s", peeled.Hex(), repo.c2.Hex())
	}
	if typ != store.TypeCommit {
		t.Fatalf("tag peeled to type %v, want commit", typ)
	}
}
