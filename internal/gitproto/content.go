// Git-level helpers the domain and the contents API build on: commit
// creation, tree surgery, history walks, merge-base and diffs, plus the
// deterministic operation clock.
//
// Every timestamp in this file comes from the repository's op counter,
// never from the wall clock, so the same sequence of API calls always
// produces the same commit ids.
package gitproto

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"forge/internal/store"
)

// opEpoch is the scenario's day one, 2025-01-01T00:00:00Z. Operation N
// is stamped at opEpoch + N seconds.
const opEpoch = 1735689600

// opTimestamp returns the Unix timestamp of operation op.
func opTimestamp(op int64) int64 { return opEpoch + op }

// opTimestampString returns the RFC 3339 form of an operation timestamp.
func opTimestampString(op int64) string {
	return time.Unix(opTimestamp(op), 0).UTC().Format(time.RFC3339)
}

// nextOpLocked takes the repository's next operation number. Callers hold
// r.mu.
func (r *Repo) nextOpLocked() int64 {
	r.opCounter++
	return r.opCounter
}

// BranchTip returns the tip of a branch, or false when it does not exist.
func BranchTip(st *store.Store, branch string) (store.OID, bool) {
	return st.GetRef("refs/heads/" + branch)
}

// EmptyTreeOID is the identity of the tree with no entries.
var EmptyTreeOID = store.ComputeOID(store.TypeTree, nil)

// CreateCommit builds a commit object, stores it, and returns it. The
// author and committer are fixed; only the deterministic timestamp
// varies.
func CreateCommit(st *store.Store, tree store.OID, parents []store.OID, message string, ts int64) *store.Object {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree.Hex())
	for _, parent := range parents {
		fmt.Fprintf(&buf, "parent %s\n", parent.Hex())
	}
	identity := fmt.Sprintf("Forge <forge@forge.local> %d +0000", ts)
	fmt.Fprintf(&buf, "author %s\n", identity)
	fmt.Fprintf(&buf, "committer %s\n", identity)
	buf.WriteString("\n")
	buf.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		buf.WriteString("\n")
	}
	if tree == EmptyTreeOID {
		// The empty tree must exist in the store, or a later tree walk
		// cannot read the commit's tree.
		st.Put(store.NewObject(store.TypeTree, nil))
	}
	obj := store.NewObject(store.TypeCommit, buf.Bytes())
	st.Put(obj)
	return obj
}

// CommitMessage returns a commit's message, without trailing newlines.
func CommitMessage(st *store.Store, oid store.OID) (string, error) {
	obj, ok := st.Get(oid)
	if !ok {
		return "", fmt.Errorf("gitproto: commit %s is not in the store", oid.Hex())
	}
	blank := bytes.Index(obj.Data, []byte("\n\n"))
	if blank < 0 {
		return "", fmt.Errorf("gitproto: commit %s has no message", oid.Hex())
	}
	return strings.TrimRight(string(obj.Data[blank+2:]), "\n"), nil
}

// History walks the first-parent chain from a tip, newest first.
func History(st *store.Store, tip store.OID) []store.OID {
	var out []store.OID
	seen := make(map[store.OID]bool)
	current := tip
	for {
		if seen[current] {
			break
		}
		seen[current] = true
		obj, ok := st.Get(current)
		if !ok || obj.Type != store.TypeCommit {
			break
		}
		out = append(out, current)
		commit, err := ParseCommit(obj.Data)
		if err != nil || len(commit.Parents) == 0 {
			break
		}
		current = commit.Parents[0]
	}
	return out
}

// MergeBase returns the first commit reachable from both tips, found by
// breadth-first search from each side.
func MergeBase(st *store.Store, a, b store.OID) (store.OID, bool) {
	ancestors := make(map[store.OID]bool)
	queue := []store.OID{b}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if ancestors[current] {
			continue
		}
		ancestors[current] = true
		obj, ok := st.Get(current)
		if !ok || obj.Type != store.TypeCommit {
			continue
		}
		commit, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, commit.Parents...)
	}
	queue = []store.OID{a}
	seen := make(map[store.OID]bool)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		if ancestors[current] {
			return current, true
		}
		obj, ok := st.Get(current)
		if !ok || obj.Type != store.TypeCommit {
			continue
		}
		commit, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, commit.Parents...)
	}
	return store.OID{}, false
}

// sortKey is git's tree sort order: a directory sorts as its name with a
// trailing slash.
func treeSortKey(entry TreeEntry) string {
	if entry.Mode == "40000" || entry.Mode == "040000" {
		return entry.Name + "/"
	}
	return entry.Name
}

// serializeTree writes tree entries in git order.
func serializeTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return treeSortKey(sorted[i]) < treeSortKey(sorted[j]) })
	var buf bytes.Buffer
	for _, entry := range sorted {
		buf.WriteString(entry.Mode)
		buf.WriteByte(' ')
		buf.WriteString(entry.Name)
		buf.WriteByte(0)
		buf.Write(entry.OID[:])
	}
	return buf.Bytes()
}

// UpdateTree returns a new tree with one path set to a blob, or deleted
// when blob is nil. Intermediate directories are created as needed, and
// a directory left empty by a delete is removed.
func UpdateTree(st *store.Store, treeOID store.OID, path string, blob *store.OID) (store.OID, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return store.OID{}, fmt.Errorf("gitproto: empty path")
	}
	return updateTreeAt(st, treeOID, parts, blob)
}

func updateTreeAt(st *store.Store, treeOID store.OID, parts []string, blob *store.OID) (store.OID, error) {
	var entries []TreeEntry
	if !treeOID.IsZero() {
		obj, ok := st.Get(treeOID)
		if !ok {
			return store.OID{}, fmt.Errorf("gitproto: tree %s is not in the store", treeOID.Hex())
		}
		var err error
		entries, err = ParseTree(obj.Data)
		if err != nil {
			return store.OID{}, err
		}
	}
	name := parts[0]
	index := -1
	for i, entry := range entries {
		if entry.Name == name {
			index = i
			break
		}
	}
	if len(parts) == 1 {
		if blob == nil {
			if index >= 0 {
				entries = append(entries[:index], entries[index+1:]...)
			}
		} else {
			if index >= 0 {
				entries[index] = TreeEntry{Mode: "100644", Name: name, OID: *blob}
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: *blob})
			}
		}
	} else {
		var childOID store.OID
		if index >= 0 && (entries[index].Mode == "40000" || entries[index].Mode == "040000") {
			childOID = entries[index].OID
		}
		updated, err := updateTreeAt(st, childOID, parts[1:], blob)
		if err != nil {
			return store.OID{}, err
		}
		if updated == EmptyTreeOID {
			// The subtree is empty now: drop the directory entry.
			if index >= 0 {
				entries = append(entries[:index], entries[index+1:]...)
			}
		} else if index >= 0 {
			entries[index] = TreeEntry{Mode: "40000", Name: name, OID: updated}
		} else {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: updated})
		}
	}
	if len(entries) == 0 {
		return EmptyTreeOID, nil
	}
	obj := store.NewObject(store.TypeTree, serializeTree(entries))
	st.Put(obj)
	return obj.OID(), nil
}

// FlattenTree returns every file in a tree, keyed by its full path.
// Submodules are skipped.
func FlattenTree(st *store.Store, treeOID store.OID) (map[string]TreeEntry, error) {
	out := make(map[string]TreeEntry)
	if treeOID.IsZero() || treeOID == EmptyTreeOID {
		return out, nil
	}
	if err := flattenTreeInto(st, treeOID, "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func flattenTreeInto(st *store.Store, treeOID store.OID, prefix string, out map[string]TreeEntry) error {
	obj, ok := st.Get(treeOID)
	if !ok {
		return fmt.Errorf("gitproto: tree %s is not in the store", treeOID.Hex())
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsGitlink() {
			continue
		}
		path := prefix + entry.Name
		if entry.Mode == "40000" || entry.Mode == "040000" {
			if err := flattenTreeInto(st, entry.OID, path+"/", out); err != nil {
				return err
			}
			continue
		}
		out[path] = entry
	}
	return nil
}

// entryAtPath returns one path's tree entry, walking directories.
func entryAtPath(st *store.Store, treeOID store.OID, path string) (TreeEntry, bool) {
	flat, err := FlattenTree(st, treeOID)
	if err != nil {
		return TreeEntry{}, false
	}
	entry, ok := flat[strings.Trim(path, "/")]
	return entry, ok
}

// blobHexAtPath returns the hex blob id at a path, or false when the path
// is not a file.
func blobHexAtPath(st *store.Store, treeOID store.OID, path string) (string, bool) {
	entry, ok := entryAtPath(st, treeOID, path)
	if !ok {
		return "", false
	}
	return entry.OID.Hex(), true
}

// FileChange is one path's change between two trees.
type FileChange struct {
	Path      string
	Status    string // "added", "removed" or "modified"
	Additions int
	Deletions int
}

// lineCount counts the lines of a blob's content.
func lineCount(content []byte) int {
	return len(splitLines(content))
}

// splitLines splits content into lines; empty content has no lines.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}

// lineDiff counts added and removed lines between two contents, by
// multiset difference of their lines.
func lineDiff(oldContent, newContent []byte) (additions, deletions int) {
	counts := make(map[string]int)
	for _, line := range splitLines(oldContent) {
		counts[line]++
	}
	for _, line := range splitLines(newContent) {
		if counts[line] > 0 {
			counts[line]--
		} else {
			additions++
		}
	}
	for _, remaining := range counts {
		deletions += remaining
	}
	return additions, deletions
}

// DiffTrees diffs two trees and returns the changed paths, sorted by
// path.
func DiffTrees(st *store.Store, baseTree, headTree store.OID) ([]FileChange, error) {
	base, err := FlattenTree(st, baseTree)
	if err != nil {
		return nil, err
	}
	head, err := FlattenTree(st, headTree)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool)
	for path := range base {
		paths[path] = true
	}
	for path := range head {
		paths[path] = true
	}
	sorted := make([]string, 0, len(paths))
	for path := range paths {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	var changes []FileChange
	for _, path := range sorted {
		baseEntry, inBase := base[path]
		headEntry, inHead := head[path]
		switch {
		case inHead && !inBase:
			obj, _ := st.Get(headEntry.OID)
			var content []byte
			if obj != nil {
				content = obj.Data
			}
			changes = append(changes, FileChange{Path: path, Status: "added", Additions: lineCount(content)})
		case inBase && !inHead:
			obj, _ := st.Get(baseEntry.OID)
			var content []byte
			if obj != nil {
				content = obj.Data
			}
			changes = append(changes, FileChange{Path: path, Status: "removed", Deletions: lineCount(content)})
		case baseEntry.OID != headEntry.OID:
			var oldContent, newContent []byte
			if obj, ok := st.Get(baseEntry.OID); ok {
				oldContent = obj.Data
			}
			if obj, ok := st.Get(headEntry.OID); ok {
				newContent = obj.Data
			}
			additions, deletions := lineDiff(oldContent, newContent)
			changes = append(changes, FileChange{Path: path, Status: "modified", Additions: additions, Deletions: deletions})
		}
	}
	return changes, nil
}

// DirEntry is one entry of a contents directory listing.
type DirEntry struct {
	Type string // "file" or "dir"
	Name string
	Path string
	SHA  string
}

// ListDir lists a directory's direct children at a branch tip.
func ListDir(st *store.Store, treeOID store.OID, path string) ([]DirEntry, error) {
	current := treeOID
	if strings.Trim(path, "/") != "" {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		for _, part := range parts {
			obj, ok := st.Get(current)
			if !ok {
				return nil, fmt.Errorf("gitproto: directory %q does not exist", path)
			}
			entries, err := ParseTree(obj.Data)
			if err != nil {
				return nil, err
			}
			found := false
			for _, entry := range entries {
				if entry.Name == part && (entry.Mode == "40000" || entry.Mode == "040000") {
					current = entry.OID
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("gitproto: directory %q does not exist", path)
			}
		}
	}
	obj, ok := st.Get(current)
	if !ok {
		if current == EmptyTreeOID {
			return []DirEntry{}, nil
		}
		return nil, fmt.Errorf("gitproto: directory %q does not exist", path)
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsGitlink() {
			continue
		}
		kind := "file"
		if entry.Mode == "40000" || entry.Mode == "040000" {
			kind = "dir"
		}
		full := entry.Name
		if trimmed := strings.Trim(path, "/"); trimmed != "" {
			full = trimmed + "/" + entry.Name
		}
		out = append(out, DirEntry{Type: kind, Name: entry.Name, Path: full, SHA: entry.OID.Hex()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ErrStaleSHA, ErrSHANeeded and ErrFileNotFound are the contents API
// failures the adapters map to 409, 422 and 404.
var (
	ErrStaleSHA     = fmt.Errorf("gitproto: the sha does not match the current file")
	ErrSHANeeded    = fmt.Errorf("gitproto: sha is required to update an existing file")
	ErrFileNotFound = fmt.Errorf("gitproto: file not found")
)

// ContentsResult is one contents write's answer.
type ContentsResult struct {
	CommitSHA string
	BlobSHA   string
	Updated   bool // true when the file already existed (a 200, not a 201)
}

// ContentsCommit writes one file on a branch and returns the new commit.
// When the branch does not exist yet it is created from the default
// branch's tip, the way GitHub's contents API does it; an unborn
// repository starts from the empty tree.
func (r *Repo) ContentsCommit(branch, path string, content []byte, message, sha string, hasSHA bool) (*ContentsResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tip, hasTip := BranchTip(r.Store, branch)
	if !hasTip && branch != r.DefaultBranch {
		// A new branch starts from the default branch, when it has one.
		tip, hasTip = BranchTip(r.Store, r.DefaultBranch)
	}
	// A zero base tree is the empty tree: updateTreeAt treats it as no
	// entries, which is how the first commit on an unborn repository is
	// built. The well-known empty-tree oid itself is never stored.
	var baseTree store.OID
	if hasTip {
		commit, err := commitOf(r.Store, tip)
		if err != nil {
			return nil, err
		}
		baseTree = commit.Tree
	}
	current, exists := entryAtPath(r.Store, baseTree, path)
	if hasSHA {
		if !exists || current.OID.Hex() != strings.ToLower(sha) {
			return nil, ErrStaleSHA
		}
	} else if exists {
		return nil, ErrSHANeeded
	}
	blob := store.NewObject(store.TypeBlob, content)
	r.Store.Put(blob)
	blobOID := blob.OID()
	newTree, err := UpdateTree(r.Store, baseTree, path, &blobOID)
	if err != nil {
		return nil, err
	}
	var parents []store.OID
	if hasTip {
		parents = []store.OID{tip}
	}
	op := r.nextOpLocked()
	commit := CreateCommit(r.Store, newTree, parents, message, opTimestamp(op))
	r.Store.SetRef("refs/heads/"+branch, commit.OID())
	return &ContentsResult{CommitSHA: commit.OID().Hex(), BlobSHA: blob.OID().Hex(), Updated: exists}, nil
}

// ContentsDelete removes one file on a branch and returns the new commit.
func (r *Repo) ContentsDelete(branch, path, message, sha string, hasSHA bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tip, hasTip := BranchTip(r.Store, branch)
	if !hasTip {
		return "", ErrFileNotFound
	}
	commit, err := commitOf(r.Store, tip)
	if err != nil {
		return "", err
	}
	current, exists := entryAtPath(r.Store, commit.Tree, path)
	if !exists {
		return "", ErrFileNotFound
	}
	if hasSHA && current.OID.Hex() != strings.ToLower(sha) {
		return "", ErrStaleSHA
	}
	newTree, err := UpdateTree(r.Store, commit.Tree, path, nil)
	if err != nil {
		return "", err
	}
	op := r.nextOpLocked()
	deleteCommit := CreateCommit(r.Store, newTree, []store.OID{tip}, message, opTimestamp(op))
	r.Store.SetRef("refs/heads/"+branch, deleteCommit.OID())
	return deleteCommit.OID().Hex(), nil
}

// ReadFile returns a file's blob id and bytes at a branch tip.
func (r *Repo) ReadFile(branch, path string) (string, []byte, error) {
	tip, ok := BranchTip(r.Store, branch)
	if !ok {
		return "", nil, ErrFileNotFound
	}
	commit, err := commitOf(r.Store, tip)
	if err != nil {
		return "", nil, err
	}
	entry, ok := entryAtPath(r.Store, commit.Tree, path)
	if !ok {
		return "", nil, ErrFileNotFound
	}
	obj, ok := r.Store.Get(entry.OID)
	if !ok || obj.Type != store.TypeBlob {
		return "", nil, ErrFileNotFound
	}
	return entry.OID.Hex(), obj.Data, nil
}

// ReadDir lists a directory at a branch tip.
func (r *Repo) ReadDir(branch, path string) ([]DirEntry, error) {
	tip, ok := BranchTip(r.Store, branch)
	if !ok {
		return nil, ErrFileNotFound
	}
	commit, err := commitOf(r.Store, tip)
	if err != nil {
		return nil, err
	}
	return ListDir(r.Store, commit.Tree, path)
}

// BranchInfo is one branch with its tip.
type BranchInfo struct {
	Name string
	SHA  string
}

// Branches returns every branch with its tip, sorted by name.
func (r *Repo) Branches() []BranchInfo {
	var out []BranchInfo
	for _, name := range r.Store.RefNames() {
		if !strings.HasPrefix(name, "refs/heads/") {
			continue
		}
		oid, _ := r.Store.GetRef(name)
		out = append(out, BranchInfo{Name: strings.TrimPrefix(name, "refs/heads/"), SHA: oid.Hex()})
	}
	return out
}
