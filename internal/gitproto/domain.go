// The forge's domain model: labels, issues, comments and pull requests.
//
// This file is the business logic layer. The REST, GraphQL and MCP
// packages are thin adapters over these methods, so every surface reads
// and writes the same objects and rest-parity holds automatically. Issue
// and pull-request numbers share one counter per repository, the way
// GitHub numbers them. Timestamps never come from the wall clock: every
// operation takes a deterministic sequence number from the repository's
// op counter, so two runs on the same requests leave byte-identical
// state.
package gitproto

import (
	"fmt"
	"sort"

	"forge/internal/store"
)

// Label is a repository label: a name and a hex colour without the hash.
type Label struct {
	Name  string
	Color string
}

// Comment is one issue comment. ID is a per-repository sequence starting
// at 1; the GraphQL global id encodes the repository, the issue and this
// sequence, so it is stable and resolvable.
type Comment struct {
	ID   int64
	Body string
}

// Issue is one issue. DBID is the numeric id the REST surface reports;
// it is unique across the forge and never changes.
type Issue struct {
	Number   int
	DBID     int64
	Title    string
	Body     string
	State    string // "open" or "closed"
	Labels   []string
	Comments []*Comment
}

// Pull is one pull request. HeadSHA and BaseSHA are resolved from the
// branch tips at read time, so a pull always reports the current tips.
// MergedAt is a deterministic timestamp string, never wall-clock time.
type Pull struct {
	Number         int
	DBID           int64
	Title          string
	Body           string
	State          string // "open" or "closed"
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	HeadRef        string
	BaseRef        string
}

// CreateLabel adds a label to the repository. It reports an error when
// the name already exists.
func (r *Repo) CreateLabel(name, color string) (*Label, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.labels[name]; ok {
		return nil, fmt.Errorf("gitproto: label %q already exists", name)
	}
	label := &Label{Name: name, Color: color}
	r.labels[name] = label
	return label, nil
}

// GetLabel returns a label by name.
func (r *Repo) GetLabel(name string) (*Label, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	label, ok := r.labels[name]
	return label, ok
}

// Labels returns every label, sorted by name.
func (r *Repo) Labels() []*Label {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Label, 0, len(r.labels))
	for _, label := range r.labels {
		out = append(out, label)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CreateIssue creates an issue. Every named label must already exist, or
// the creation fails with an unknown-label error.
func (r *Repo) CreateIssue(title, body string, labels []string) (*Issue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range labels {
		if _, ok := r.labels[name]; !ok {
			return nil, fmt.Errorf("gitproto: unknown label %q", name)
		}
	}
	issue := &Issue{
		Number: r.nextNumber,
		DBID:   r.forge.NextDBID(),
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string(nil), labels...),
	}
	r.nextNumber++
	r.issues[issue.Number] = issue
	return issue, nil
}

// GetIssue returns an issue by number.
func (r *Repo) GetIssue(number int) (*Issue, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	issue, ok := r.issues[number]
	return issue, ok
}

// ListIssues returns issues filtered by state ("open", "closed" or "all"),
// in creation order, ascending or descending by number.
func (r *Repo) ListIssues(state string, desc bool) []*Issue {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Issue, 0, len(r.issues))
	for _, issue := range r.issues {
		if state != "all" && issue.State != state {
			continue
		}
		out = append(out, issue)
	}
	sort.Slice(out, func(i, j int) bool {
		if desc {
			return out[i].Number > out[j].Number
		}
		return out[i].Number < out[j].Number
	})
	return out
}

// PatchIssue updates an issue's title, body or state. A nil field is left
// alone. It reports false when the issue does not exist. A patch
// invalidates the issue's cached REST view, so no surface can ever read
// a stale serialisation.
func (r *Repo) PatchIssue(number int, title, body, state *string) (*Issue, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	issue, ok := r.issues[number]
	if !ok {
		return nil, false
	}
	if title != nil {
		issue.Title = *title
	}
	if body != nil {
		issue.Body = *body
	}
	if state != nil {
		issue.State = *state
	}
	delete(r.issueViews, number)
	return issue, true
}

// IssueView returns the cached REST serialisation of an issue, when the
// REST handler stored one. The bytes are immutable: callers write them
// out, never into them.
func (r *Repo) IssueView(number int) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	view, ok := r.issueViews[number]
	return view, ok
}

// SetIssueView stores the REST serialisation of an issue. The
// serialisation is a pure function of the issue (number, title, body,
// state, labels): it holds no timestamp, so caching it cannot break
// determinism. PatchIssue deletes the entry; AddComment does not need
// to, because comments are not part of the issue view.
func (r *Repo) SetIssueView(number int, view []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.issueViews == nil {
		r.issueViews = make(map[int][]byte)
	}
	r.issueViews[number] = view
}

// AddComment appends a comment to an issue. It reports false when the
// issue does not exist.
func (r *Repo) AddComment(number int, body string) (*Comment, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	issue, ok := r.issues[number]
	if !ok {
		return nil, false
	}
	r.nextComment++
	comment := &Comment{ID: r.nextComment, Body: body}
	issue.Comments = append(issue.Comments, comment)
	return comment, true
}

// IssueComments returns an issue's comments in creation order.
func (r *Repo) IssueComments(number int) ([]*Comment, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	issue, ok := r.issues[number]
	if !ok {
		return nil, false
	}
	return append([]*Comment(nil), issue.Comments...), true
}

// CreatePull opens a pull request from a head branch into a base branch.
// The head branch must exist, and an already-open pull request from the
// same head into the same base is a duplicate.
func (r *Repo) CreatePull(title, body, head, base string) (*Pull, error) {
	if _, ok := BranchTip(r.Store, head); !ok {
		return nil, fmt.Errorf("gitproto: head branch %q does not exist", head)
	}
	if _, ok := BranchTip(r.Store, base); !ok {
		return nil, fmt.Errorf("gitproto: base branch %q does not exist", base)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pull := range r.pulls {
		if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
			return nil, fmt.Errorf("gitproto: an open pull request from %q already exists", head)
		}
	}
	pull := &Pull{
		Number:  r.nextNumber,
		DBID:    r.forge.NextDBID(),
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: head,
		BaseRef: base,
	}
	r.nextNumber++
	r.pulls[pull.Number] = pull
	return pull, nil
}

// GetPull returns a pull request by number.
func (r *Repo) GetPull(number int) (*Pull, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pull, ok := r.pulls[number]
	return pull, ok
}

// ListPulls returns pull requests filtered by state ("open", "closed" or
// "all"), in creation order.
func (r *Repo) ListPulls(state string) []*Pull {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Pull, 0, len(r.pulls))
	for _, pull := range r.pulls {
		if state != "all" && pull.State != state {
			continue
		}
		out = append(out, pull)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// PullHeadSHA returns the head branch's current tip, or false when the
// branch is gone.
func (r *Repo) PullHeadSHA(pull *Pull) (string, bool) {
	tip, ok := BranchTip(r.Store, pull.HeadRef)
	if !ok {
		return "", false
	}
	return tip.Hex(), true
}

// PullBaseSHA returns the base branch's current tip, or false when the
// branch is gone.
func (r *Repo) PullBaseSHA(pull *Pull) (string, bool) {
	tip, ok := BranchTip(r.Store, pull.BaseRef)
	if !ok {
		return "", false
	}
	return tip.Hex(), true
}

// PullFiles returns the paths a pull request changes, as the diff between
// its merge-base and its head tip.
func (r *Repo) PullFiles(number int) ([]FileChange, bool) {
	pull, ok := r.GetPull(number)
	if !ok {
		return nil, false
	}
	headTip, ok := BranchTip(r.Store, pull.HeadRef)
	if !ok {
		return nil, false
	}
	baseTip, ok := BranchTip(r.Store, pull.BaseRef)
	if !ok {
		return nil, false
	}
	base, ok := MergeBase(r.Store, headTip, baseTip)
	if !ok {
		return nil, false
	}
	headObj, ok := r.Store.Get(headTip)
	if !ok {
		return nil, false
	}
	headCommit, err := ParseCommit(headObj.Data)
	if err != nil {
		return nil, false
	}
	baseObj, ok := r.Store.Get(base)
	if !ok {
		return nil, false
	}
	baseCommit, err := ParseCommit(baseObj.Data)
	if err != nil {
		return nil, false
	}
	changes, err := DiffTrees(r.Store, baseCommit.Tree, headCommit.Tree)
	if err != nil {
		return nil, false
	}
	return changes, true
}

// PullMergeable reports whether an open pull request can merge cleanly:
// MERGEABLE when its changes apply, CONFLICTING when the base moved under
// a file it touches, UNKNOWN when it is already closed or its branches
// are gone.
func (r *Repo) PullMergeable(number int) string {
	pull, ok := r.GetPull(number)
	if !ok || pull.State != "open" {
		return "UNKNOWN"
	}
	headTip, ok := BranchTip(r.Store, pull.HeadRef)
	if !ok {
		return "UNKNOWN"
	}
	baseTip, ok := BranchTip(r.Store, pull.BaseRef)
	if !ok {
		return "UNKNOWN"
	}
	if _, conflict := mergeAnalysis(r.Store, headTip, baseTip); conflict {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// ErrPullNotFound, ErrPullClosed and ErrPullConflict are the merge
// failures the adapters map to 404, 405 and 405.
var (
	ErrPullNotFound = fmt.Errorf("gitproto: pull request not found")
	ErrPullClosed   = fmt.Errorf("gitproto: pull request is already closed")
	ErrPullConflict = fmt.Errorf("gitproto: pull request conflicts with the base branch")
)

// MergePull squash-merges an open pull request onto its base branch: one
// new commit on the base whose tree is the base tree with the pull's
// changes applied. It returns the merge commit's hex id.
func (r *Repo) MergePull(number int) (string, error) {
	pull, ok := r.GetPull(number)
	if !ok {
		return "", ErrPullNotFound
	}
	if pull.State != "open" {
		return "", ErrPullClosed
	}
	headTip, ok := BranchTip(r.Store, pull.HeadRef)
	if !ok {
		return "", ErrPullConflict
	}
	baseTip, ok := BranchTip(r.Store, pull.BaseRef)
	if !ok {
		return "", ErrPullConflict
	}
	newTree, conflict := mergeAnalysis(r.Store, headTip, baseTip)
	if conflict {
		return "", ErrPullConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check under the lock: the pull may have been merged concurrently.
	if pull.State != "open" {
		return "", ErrPullClosed
	}
	op := r.nextOpLocked()
	message := fmt.Sprintf("Merge pull request #%d from %s/%s", pull.Number, r.Owner, pull.HeadRef)
	mergeCommit := CreateCommit(r.Store, newTree, []store.OID{baseTip}, message, opTimestamp(op))
	r.Store.SetRef("refs/heads/"+pull.BaseRef, mergeCommit.OID())
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = opTimestampString(op)
	pull.MergeCommitSHA = mergeCommit.OID().Hex()
	return mergeCommit.OID().Hex(), nil
}

// mergeAnalysis applies a pull's changes onto its base tip and reports
// the resulting tree. It reports a conflict when the head and the base
// both changed the same path from the merge-base in different ways.
func mergeAnalysis(st *store.Store, headTip, baseTip store.OID) (store.OID, bool) {
	base, ok := MergeBase(st, headTip, baseTip)
	if !ok {
		return store.OID{}, true
	}
	headCommit, err := commitOf(st, headTip)
	if err != nil {
		return store.OID{}, true
	}
	baseCommit, err := commitOf(st, baseTip)
	if err != nil {
		return store.OID{}, true
	}
	mergeBaseCommit, err := commitOf(st, base)
	if err != nil {
		return store.OID{}, true
	}
	headChanges, err := DiffTrees(st, mergeBaseCommit.Tree, headCommit.Tree)
	if err != nil {
		return store.OID{}, true
	}
	baseChanges, err := DiffTrees(st, mergeBaseCommit.Tree, baseCommit.Tree)
	if err != nil {
		return store.OID{}, true
	}
	changedByBase := make(map[string]bool, len(baseChanges))
	for _, change := range baseChanges {
		changedByBase[change.Path] = true
	}
	for _, change := range headChanges {
		if !changedByBase[change.Path] {
			continue
		}
		// Both sides touched the path. It merges cleanly only when the
		// two sides ended with the same blob (or both deleted it).
		headBlob, headOK := blobHexAtPath(st, headCommit.Tree, change.Path)
		baseBlob, baseOK := blobHexAtPath(st, baseCommit.Tree, change.Path)
		if headOK != baseOK || headBlob != baseBlob {
			return store.OID{}, true
		}
	}
	merged, err := applyChanges(st, baseCommit.Tree, headCommit.Tree, headChanges)
	if err != nil {
		return store.OID{}, true
	}
	return merged, false
}

// applyChanges applies a diff onto a tree: every changed path takes its
// new blob from the head tree, or is deleted. It returns the new tree id.
func applyChanges(st *store.Store, baseTree, headTree store.OID, changes []FileChange) (store.OID, error) {
	current := baseTree
	for _, change := range changes {
		var err error
		if change.Status == "removed" {
			current, err = UpdateTree(st, current, change.Path, nil)
		} else {
			entry, ok := entryAtPath(st, headTree, change.Path)
			if !ok {
				return store.OID{}, fmt.Errorf("gitproto: changed path %q is not in the head tree", change.Path)
			}
			oid := entry.OID
			current, err = UpdateTree(st, current, change.Path, &oid)
		}
		if err != nil {
			return store.OID{}, err
		}
	}
	return current, nil
}

// commitOf parses a commit object, or reports an error when it is
// absent or malformed.
func commitOf(st *store.Store, oid store.OID) (*Commit, error) {
	obj, ok := st.Get(oid)
	if !ok {
		return nil, fmt.Errorf("gitproto: commit %s is not in the store", oid.Hex())
	}
	return ParseCommit(obj.Data)
}
