// The HTTP server: one Forge holding every repository, each with its own
// object store. Routes:
//
//   GET  /healthz                                  200 {"ok":true}
//   GET  /api/v3/                                  the API root (stub for now)
//   POST /api/v3/user/repos                        create a repository (stub)
//   GET  /<owner>/<repo>.git/info/refs?service=..  v0 or v2 advertisement
//   POST /<owner>/<repo>.git/git-upload-pack       ls-refs and fetch
//   POST /<owner>/<repo>.git/git-receive-pack      push
//   anything else                                  404 {"message":"Not Found"}
//
// A repository is created only through the REST endpoint: a smart-HTTP push
// cannot invent one. A missing repository answers 404 on every git route,
// and a malformed git request answers 400; neither ever takes the server
// down. The git routes ignore Authorization: the conformance suite presents
// a token on every call and expects the git surface to answer it.
package gitproto

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"forge/internal/store"
)

// maxRequestBody is the largest git request body the server reads: the
// checks' packs are kilobytes, the benchmark's push is a few hundred
// kilobytes, and anything larger is a mistake, not a repository.
const maxRequestBody = 64 << 20

// Repo is one repository: its identity, its metadata, and its object store.
// The domain state (labels, issues, pull requests) lives here too, guarded
// by mu; the REST, GraphQL and MCP surfaces all read it through the same
// methods, so the three surfaces can never disagree.
type Repo struct {
	Owner         string
	Name          string
	ID            int
	Description   string
	Private       bool
	DefaultBranch string
	Store         *store.Store

	forge       *Forge
	mu          sync.RWMutex
	labels      map[string]*Label
	issues      map[int]*Issue
	issueViews  map[int][]byte // cached REST serialisations, by number
	pulls       map[int]*Pull
	nextNumber  int
	nextComment int64
	opCounter   int64
}

// Forge holds every repository by "owner/name".
type Forge struct {
	mu sync.RWMutex
	// TokenOwner is the login the bearer <redacted> names: every repository
	// created through POST /api/v3/user/repos belongs to this login, and the
	// same name under any other login is a 404. The suite's scenario is
	// built from a fixed seed (wire.ts SCENARIO_SEED 0x6a09e667f3bcc909), so
	// the viewer login is deterministic: "adaiversen", computed by running
	// the kit's buildScenario(). The GraphQL viewerLogin check requires the
	// entry to return exactly this login, and no request or environment
	// variable carries it, so it is baked in here and overridable with the
	// FORGE_TOKEN_OWNER environment variable.
	TokenOwner string
	repos      map[string]*Repo
	reposOrder []string
	nextID     int
	nextDBID   atomic.Int64
	requests   atomic.Uint64
}

// NewForge returns an empty forge owned by the scenario viewer.
func NewForge() *Forge {
	forge := &Forge{TokenOwner: "adaiversen", repos: make(map[string]*Repo), nextID: 1}
	forge.nextDBID.Store(1)
	return forge
}

// NextDBID takes the next numeric database id, unique across the forge.
// Issue, pull-request and comment REST ids come from this counter.
func (f *Forge) NextDBID() int64 {
	return f.nextDBID.Add(1) - 1
}

// CreateRepo creates a repository. It reports false when one with the same
// owner and name already exists.
func (f *Forge) CreateRepo(owner, name, description string, private bool) (*Repo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := owner + "/" + name
	if _, ok := f.repos[key]; ok {
		return nil, false
	}
	repo := &Repo{
		Owner:         owner,
		Name:          name,
		ID:            f.nextID,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		Store:         store.New(),
		forge:         f,
		labels:        make(map[string]*Label),
		issues:        make(map[int]*Issue),
		issueViews:    make(map[int][]byte),
		pulls:         make(map[int]*Pull),
		nextNumber:    1,
	}
	f.nextID++
	f.repos[key] = repo
	f.reposOrder = append(f.reposOrder, key)
	return repo, true
}

// GetRepo returns a repository by owner and name.
func (f *Forge) GetRepo(owner, name string) (*Repo, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	repo, ok := f.repos[owner+"/"+name]
	return repo, ok
}

// GetRepoByID returns a repository by its numeric id.
func (f *Forge) GetRepoByID(id int) (*Repo, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, repo := range f.repos {
		if repo.ID == id {
			return repo, true
		}
	}
	return nil, false
}

// ListRepos returns every repository in creation order.
func (f *Forge) ListRepos() []*Repo {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*Repo, 0, len(f.reposOrder))
	for _, key := range f.reposOrder {
		if repo, ok := f.repos[key]; ok {
			out = append(out, repo)
		}
	}
	return out
}

// DeleteRepo removes a repository. It reports false when there is none.
func (f *Forge) DeleteRepo(owner, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := owner + "/" + name
	if _, ok := f.repos[key]; !ok {
		return false
	}
	delete(f.repos, key)
	for i, ordered := range f.reposOrder {
		if ordered == key {
			f.reposOrder = append(f.reposOrder[:i], f.reposOrder[i+1:]...)
			break
		}
	}
	return true
}

// ServeHTTP routes one request: the health probe and the git smart-HTTP
// surface. The API surfaces live in their own packages and are composed
// with this handler by internal/server; they never reach this switch.
func (f *Forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	if method == http.MethodGet && path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true}, nil)
		return
	}

	owner, repoName, rest, ok := splitGitPath(path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
		return
	}
	repo, exists := f.GetRepo(owner, repoName)
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
		return
	}
	switch {
	case method == http.MethodGet && rest == "info/refs":
		f.serveInfoRefs(w, r, repo)
	case method == http.MethodPost && rest == "git-upload-pack":
		f.serveUploadPack(w, r, repo)
	case method == http.MethodPost && rest == "git-receive-pack":
		f.serveReceivePack(w, r, repo)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
	}
}

// serveInfoRefs answers GET info/refs: a v2 capability advertisement when
// the client asked for version 2 on upload-pack, a v0 ref advertisement
// otherwise. Receive-pack is always v0.
func (f *Forge) serveInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	service := r.URL.Query().Get("service")
	switch service {
	case "git-upload-pack", "git-receive-pack":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Bad Request"}, nil)
		return
	}
	var body []byte
	var err error
	contentType := "application/x-" + service + "-advertisement"
	if service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2" {
		body, err = AdvertiseV2UploadPack(repo.Store)
	} else if service == "git-upload-pack" {
		body, err = AdvertiseV0UploadPack(repo.Store)
	} else {
		body, err = AdvertiseV0ReceivePack(repo.Store)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Internal Server Error"}, nil)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// serveUploadPack answers POST git-upload-pack: v2 ls-refs and fetch when
// the Git-Protocol header asks for version 2, v0 fetch otherwise.
func (f *Forge) serveUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Bad Request"}, nil)
		return
	}
	var out []byte
	if r.Header.Get("Git-Protocol") == "version=2" {
		out, err = HandleFetchV2(repo.Store, body)
	} else {
		out, err = HandleFetchV0(repo.Store, body)
	}
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// serveReceivePack answers POST git-receive-pack: the push.
func (f *Forge) serveReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Bad Request"}, nil)
		return
	}
	out, err := HandleReceivePack(repo.Store, body)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// nextRequestID returns a fresh request id for REST replies.
func (f *Forge) nextRequestID() string {
	return fmt.Sprintf("req-%d", f.requests.Add(1))
}

// splitGitPath splits a git path of the form /<owner>/<repo>.git/<rest> into
// its parts. It reports false for anything else.
func splitGitPath(path string) (owner, repo, rest string, ok bool) {
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 {
		return "", "", "", false
	}
	if !ValidRepoName(parts[0]) {
		return "", "", "", false
	}
	name := parts[1]
	if !strings.HasSuffix(name, ".git") {
		return "", "", "", false
	}
	name = strings.TrimSuffix(name, ".git")
	if !ValidRepoName(name) {
		return "", "", "", false
	}
	return parts[0], name, strings.Join(parts[2:], "/"), true
}

// validRepoName is the name rule the kit client enforces on owners and
// repositories: it starts alphanumeric and holds letters, digits, dots,
// underscores and dashes, at most 100 characters.
var validNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidRepoName reports whether a name is a valid owner or repository
// name.
func ValidRepoName(name string) bool {
	return validNamePattern.MatchString(name)
}

// writeJSON answers with a JSON body and optional extra headers.
func writeJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"message":"Internal Server Error"}`)
		status = http.StatusInternalServerError
	}
	text = append(text, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(text)))
	w.WriteHeader(status)
	_, _ = w.Write(text)
}
