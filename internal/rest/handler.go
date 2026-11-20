// The GitHub REST v3 surface: a thin adapter over the forge's domain
// methods. It owns no business logic of its own: every rule (labels,
// numbering, merge conflicts, stale shas) lives in internal/gitproto,
// so this surface can never disagree with GraphQL or MCP.
//
// Auth: every route except GET /api/v3/ and GET /api/v3/meta requires
// an Authorization header; a repository under any login other than the
// token owner's is a 404, never a 403. Every reply is application/json
// with a non-empty X-GitHub-Request-Id.
package rest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"forge/internal/gitproto"
	"forge/internal/store"
)

// Handler serves the /api/v3 surface for one forge.
type Handler struct {
	forge    *gitproto.Forge
	requests atomic.Uint64
}

// NewHandler returns a handler for the forge.
func NewHandler(forge *gitproto.Forge) *Handler {
	return &Handler{forge: forge}
}

func (h *Handler) nextRequestID() string {
	return fmt.Sprintf("req-%d", h.requests.Add(1))
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, body any) {
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"message":"Internal Server Error"}`)
		status = http.StatusInternalServerError
	}
	text = append(text, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-OAuth-Scopes", "repo, read:org")
	w.Header().Set("X-GitHub-Request-Id", h.nextRequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(text)))
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, map[string]string{"message": message})
}

// writeBytes answers with already-serialised JSON bytes (a cached issue
// view), without re-marshalling. The bytes are written as-is followed
// by one newline, exactly as writeJSON writes a fresh marshal.
func (h *Handler) writeBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-OAuth-Scopes", "repo, read:org")
	w.Header().Set("X-GitHub-Request-Id", h.nextRequestID())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)+1))
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_, _ = w.Write([]byte{'\n'})
}

// issueViewJSON marshals one issue exactly as issueJSON shapes it, and
// returns the bytes without the trailing newline writeJSON adds.
func issueViewJSON(repo *gitproto.Repo, issue *gitproto.Issue) ([]byte, error) {
	return json.Marshal(issueJSON(repo, issue))
}

// readBody reads a JSON request body into a map. An empty body is an
// empty map. A malformed body is a 400, answered here.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Bad Request")
		return nil, false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, true
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		h.writeError(w, http.StatusBadRequest, "Bad Request")
		return nil, false
	}
	return body, true
}

func stringField(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return value
}

func boolField(body map[string]any, key string, def bool) bool {
	value, ok := body[key].(bool)
	if !ok {
		return def
	}
	return value
}

// ServeHTTP routes one /api/v3 request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	if path == "" {
		path = "/"
	}
	// The API root and the meta endpoint are public, like GitHub's.
	if path == "/" {
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]string{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		})
		return
	}
	if path == "/meta" {
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]string{"installed_version": "1.0.0"})
		return
	}
	if r.Header.Get("Authorization") == "" {
		h.writeError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	switch segments[0] {
	case "user":
		h.serveUser(w, r, segments[1:])
	case "repos":
		h.serveRepos(w, r, segments[1:])
	default:
		h.writeError(w, http.StatusNotFound, "Not Found")
	}
}

// serveUser handles /user and /user/repos.
func (h *Handler) serveUser(w http.ResponseWriter, r *http.Request, rest []string) {
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]any{
			"login": h.forge.Viewer(),
			"id":    1,
			"type":  "User",
		})
		return
	}
	if len(rest) == 1 && rest[0] == "repos" {
		switch r.Method {
		case http.MethodGet:
			h.listUserRepos(w, r)
		case http.MethodPost:
			h.createUserRepo(w, r)
		default:
			h.writeError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	h.writeError(w, http.StatusNotFound, "Not Found")
}

// resolveRepo loads a repository for a /repos/{owner}/{name} route: the
// owner is learned as the viewer when the repository exists, and any other
// owner is a 404. It answers the error itself and reports false when it
// did.
func (h *Handler) resolveRepo(w http.ResponseWriter, owner, name string) (*gitproto.Repo, bool) {
	repo, ok := h.forge.ResolveRepo(owner, name)
	if !ok {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	return repo, true
}

// serveRepos handles every /repos/{owner}/{name} route.
func (h *Handler) serveRepos(w http.ResponseWriter, r *http.Request, rest []string) {
	if len(rest) < 2 {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, name := rest[0], rest[1]
	repo, ok := h.resolveRepo(w, owner, name)
	if !ok {
		return
	}
	tail := rest[2:]
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.writeJSON(w, http.StatusOK, repoJSON(repo))
		case http.MethodPatch:
			h.patchRepo(w, r, repo)
		case http.MethodDelete:
			h.forge.DeleteRepo(name)
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			w.Header().Set("X-GitHub-Request-Id", h.nextRequestID())
			w.WriteHeader(http.StatusNoContent)
		default:
			h.writeError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	switch tail[0] {
	case "branches":
		h.serveBranches(w, r, repo, tail[1:])
	case "commits":
		if r.Method == http.MethodGet && len(tail) == 1 {
			h.listCommits(w, r, repo)
		} else {
			h.writeError(w, http.StatusNotFound, "Not Found")
		}
	case "git":
		h.serveGitRefs(w, r, repo, tail[1:])
	case "contents":
		h.serveContents(w, r, repo, tail[1:])
	case "labels":
		h.serveLabels(w, r, repo, tail[1:])
	case "issues":
		h.serveIssues(w, r, repo, tail[1:])
	case "pulls":
		h.servePulls(w, r, repo, tail[1:])
	default:
		h.writeError(w, http.StatusNotFound, "Not Found")
	}
}

// repoJSON is the repository object the checks read.
func repoJSON(repo *gitproto.Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]string{"login": repo.Owner},
		"private":        repo.Private,
		"default_branch": repo.DefaultBranch,
		"description":    repo.Description,
	}
}

func (h *Handler) patchRepo(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo) {
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	if description, present := body["description"]; present {
		if text, isString := description.(string); isString {
			repo.Description = text
		}
	}
	h.writeJSON(w, http.StatusOK, repoJSON(repo))
}

// listUserRepos answers GET /user/repos with Link pagination.
func (h *Handler) listUserRepos(w http.ResponseWriter, r *http.Request) {
	repos := h.forge.ListRepos()
	page, perPage := pageParams(r)
	total := len(repos)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	items := make([]map[string]any, 0, end-start)
	for _, repo := range repos[start:end] {
		items = append(items, repoJSON(repo))
	}
	if end < total {
		h.setLinkNext(w, r, page+1, perPage)
	}
	h.writeJSON(w, http.StatusOK, items)
}

func (h *Handler) createUserRepo(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	name := stringField(body, "name")
	if !gitproto.ValidRepoName(name) {
		h.writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	repo, created := h.forge.CreateRepo(name, stringField(body, "description"), boolField(body, "private", false))
	if !created {
		h.writeError(w, http.StatusUnprocessableEntity, "Repository already exists")
		return
	}
	h.writeJSON(w, http.StatusCreated, repoJSON(repo))
}

// pageParams reads ?page= and ?per_page=, GitHub's defaults.
func pageParams(r *http.Request) (page, perPage int) {
	page = 1
	perPage = 30
	if text := r.URL.Query().Get("page"); text != "" {
		if n, err := strconv.Atoi(text); err == nil && n >= 1 {
			page = n
		}
	}
	if text := r.URL.Query().Get("per_page"); text != "" {
		if n, err := strconv.Atoi(text); err == nil && n >= 1 {
			perPage = n
			if perPage > 100 {
				perPage = 100
			}
		}
	}
	return page, perPage
}

// setLinkNext writes a Link header for the next page, same-origin.
func (h *Handler) setLinkNext(w http.ResponseWriter, r *http.Request, page, perPage int) {
	next := fmt.Sprintf("http://%s%s?page=%d&per_page=%d", r.Host, r.URL.Path, page, perPage)
	w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"next\"", next))
}

// serveBranches handles GET /branches and GET /branches/{branch}.
func (h *Handler) serveBranches(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, tail []string) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	branches := repo.Branches()
	if len(tail) == 0 {
		items := make([]map[string]any, 0, len(branches))
		for _, branch := range branches {
			items = append(items, map[string]any{"name": branch.Name, "commit": map[string]string{"sha": branch.SHA}})
		}
		h.writeJSON(w, http.StatusOK, items)
		return
	}
	if len(tail) == 1 {
		for _, branch := range branches {
			if branch.Name == tail[0] {
				h.writeJSON(w, http.StatusOK, map[string]any{"name": branch.Name, "commit": map[string]string{"sha": branch.SHA}})
				return
			}
		}
	}
	h.writeError(w, http.StatusNotFound, "Not Found")
}

// listCommits answers GET /commits, optionally for one ref via ?sha=.
func (h *Handler) listCommits(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo) {
	ref := r.URL.Query().Get("sha")
	if ref == "" {
		ref = repo.DefaultBranch
	}
	tip, ok := gitproto.BranchTip(repo.Store, ref)
	if !ok {
		if oid, err := store.OIDFromHex(ref); err == nil {
			if _, exists := repo.Store.Get(oid); exists {
				tip = oid
				ok = true
			}
		}
	}
	if !ok {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	items := make([]map[string]any, 0)
	for _, oid := range gitproto.History(repo.Store, tip) {
		message, err := gitproto.CommitMessage(repo.Store, oid)
		if err != nil {
			continue
		}
		items = append(items, map[string]any{
			"sha":    oid.Hex(),
			"commit": map[string]string{"message": message},
		})
	}
	h.writeJSON(w, http.StatusOK, items)
}

// serveGitRefs handles POST /git/refs and GET /git/refs/heads/{branch}.
func (h *Handler) serveGitRefs(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, tail []string) {
	if r.Method == http.MethodPost && len(tail) == 1 && tail[0] == "refs" {
		body, ok := h.readBody(w, r)
		if !ok {
			return
		}
		ref := stringField(body, "ref")
		sha := stringField(body, "sha")
		branch, isHead := strings.CutPrefix(ref, "refs/heads/")
		if !isHead || branch == "" {
			h.writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
			return
		}
		if _, exists := gitproto.BranchTip(repo.Store, branch); exists {
			h.writeError(w, http.StatusUnprocessableEntity, "Reference already exists")
			return
		}
		oid, err := store.OIDFromHex(sha)
		if err != nil {
			h.writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
			return
		}
		if obj, exists := repo.Store.Get(oid); !exists || obj.Type != store.TypeCommit {
			h.writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
			return
		}
		repo.Store.SetRef(ref, oid)
		h.writeJSON(w, http.StatusCreated, map[string]any{
			"ref":    ref,
			"object": map[string]string{"sha": oid.Hex()},
		})
		return
	}
	if r.Method == http.MethodGet && len(tail) == 3 && tail[0] == "refs" && tail[1] == "heads" {
		tip, ok := gitproto.BranchTip(repo.Store, tail[2])
		if !ok {
			h.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]any{
			"ref":    "refs/heads/" + tail[2],
			"object": map[string]string{"sha": tip.Hex()},
		})
		return
	}
	h.writeError(w, http.StatusNotFound, "Not Found")
}

// serveContents handles PUT, GET and DELETE /contents/{path}.
func (h *Handler) serveContents(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, tail []string) {
	path := strings.Join(tail, "/")
	switch r.Method {
	case http.MethodPut:
		h.putContents(w, r, repo, path)
	case http.MethodGet:
		h.getContents(w, r, repo, path)
	case http.MethodDelete:
		h.deleteContents(w, r, repo, path)
	default:
		h.writeError(w, http.StatusNotFound, "Not Found")
	}
}

func (h *Handler) putContents(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, path string) {
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	encoded, _ := body["content"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "content is not valid base64")
		return
	}
	branch := stringField(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha, hasSHA := body["sha"].(string)
	result, err := repo.ContentsCommit(branch, path, decoded, stringField(body, "message"), sha, hasSHA)
	if err != nil {
		switch err {
		case gitproto.ErrStaleSHA:
			h.writeError(w, http.StatusConflict, err.Error())
		case gitproto.ErrSHANeeded:
			h.writeError(w, http.StatusUnprocessableEntity, err.Error())
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	status := http.StatusCreated
	if result.Updated {
		status = http.StatusOK
	}
	h.writeJSON(w, status, map[string]any{
		"content": map[string]string{"sha": result.BlobSHA, "encoding": "base64"},
		"commit":  map[string]string{"sha": result.CommitSHA},
	})
}

func (h *Handler) getContents(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, path string) {
	branch := r.URL.Query().Get("ref")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if sha, content, err := repo.ReadFile(branch, path); err == nil {
		h.writeJSON(w, http.StatusOK, map[string]any{
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(content),
			"sha":      sha,
			"size":     len(content),
			"name":     lastSegment(path),
			"path":     path,
		})
		return
	}
	entries, err := repo.ReadDir(branch, path)
	if err != nil {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		items = append(items, map[string]any{
			"type": entry.Type,
			"name": entry.Name,
			"path": entry.Path,
			"sha":  entry.SHA,
		})
	}
	h.writeJSON(w, http.StatusOK, items)
}

func (h *Handler) deleteContents(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, path string) {
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	branch := stringField(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha, hasSHA := body["sha"].(string)
	commitSHA, err := repo.ContentsDelete(branch, path, stringField(body, "message"), sha, hasSHA)
	if err != nil {
		switch err {
		case gitproto.ErrFileNotFound:
			h.writeError(w, http.StatusNotFound, "Not Found")
		case gitproto.ErrStaleSHA:
			h.writeError(w, http.StatusConflict, err.Error())
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"commit": map[string]string{"sha": commitSHA}})
}

func lastSegment(path string) string {
	trimmed := strings.Trim(path, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

// serveLabels handles GET and POST /labels.
func (h *Handler) serveLabels(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, tail []string) {
	if len(tail) != 0 {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		items := make([]map[string]any, 0)
		for _, label := range repo.Labels() {
			items = append(items, map[string]any{"name": label.Name, "color": label.Color})
		}
		h.writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		body, ok := h.readBody(w, r)
		if !ok {
			return
		}
		name := stringField(body, "name")
		if name == "" {
			h.writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
			return
		}
		label, err := repo.CreateLabel(name, stringField(body, "color"))
		if err != nil {
			h.writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		h.writeJSON(w, http.StatusCreated, map[string]any{"name": label.Name, "color": label.Color})
	default:
		h.writeError(w, http.StatusNotFound, "Not Found")
	}
}

// issueJSON is the issue object the checks read.
func issueJSON(repo *gitproto.Repo, issue *gitproto.Issue) map[string]any {
	labels := make([]map[string]string, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		labels = append(labels, map[string]string{"name": name})
	}
	return map[string]any{
		"id":     issue.DBID,
		"number": issue.Number,
		"title":  issue.Title,
		"body":   issue.Body,
		"state":  issue.State,
		"labels": labels,
		"user":   map[string]string{"login": repo.Owner},
	}
}

// serveIssues handles /issues, /issues/{n} and /issues/{n}/comments.
func (h *Handler) serveIssues(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, tail []string) {
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listIssues(w, r, repo)
		case http.MethodPost:
			h.createIssue(w, r, repo)
		default:
			h.writeError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(tail) == 1 {
		switch r.Method {
		case http.MethodGet:
			// The read workload is 2,000 GETs of one issue: serve the
			// cached serialisation when the issue has not changed
			// since it was last written.
			if view, ok := repo.IssueView(number); ok {
				h.writeBytes(w, http.StatusOK, view)
				return
			}
			issue, ok := repo.GetIssue(number)
			if !ok {
				h.writeError(w, http.StatusNotFound, "Not Found")
				return
			}
			view, err := issueViewJSON(repo, issue)
			if err != nil {
				h.writeError(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			repo.SetIssueView(number, view)
			h.writeBytes(w, http.StatusOK, view)
		case http.MethodPatch:
			h.patchIssue(w, r, repo, number)
		default:
			h.writeError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	if len(tail) == 2 && tail[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			comments, ok := repo.IssueComments(number)
			if !ok {
				h.writeError(w, http.StatusNotFound, "Not Found")
				return
			}
			items := make([]map[string]any, 0, len(comments))
			for _, comment := range comments {
				items = append(items, map[string]any{"id": comment.ID, "body": comment.Body})
			}
			h.writeJSON(w, http.StatusOK, items)
		case http.MethodPost:
			body, ok := h.readBody(w, r)
			if !ok {
				return
			}
			comment, created := repo.AddComment(number, stringField(body, "body"))
			if !created {
				h.writeError(w, http.StatusNotFound, "Not Found")
				return
			}
			h.writeJSON(w, http.StatusCreated, map[string]any{"id": comment.ID, "body": comment.Body})
		default:
			h.writeError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	h.writeError(w, http.StatusNotFound, "Not Found")
}

func (h *Handler) listIssues(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	desc := true
	if direction := r.URL.Query().Get("direction"); direction == "asc" {
		desc = false
	}
	issues := repo.ListIssues(state, desc)
	page, perPage := pageParams(r)
	total := len(issues)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	items := make([]map[string]any, 0, end-start)
	for _, issue := range issues[start:end] {
		items = append(items, issueJSON(repo, issue))
	}
	if end < total {
		h.setLinkNext(w, r, page+1, perPage)
	}
	h.writeJSON(w, http.StatusOK, items)
}

func (h *Handler) createIssue(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo) {
	body, ok := h.readIssueCreate(w, r)
	if !ok {
		return
	}
	title := body.Title
	if title == "" {
		h.writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	issue, err := repo.CreateIssue(title, body.Body, body.Labels)
	if err != nil {
		h.writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	view, merr := issueViewJSON(repo, issue)
	if merr != nil {
		h.writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	repo.SetIssueView(issue.Number, view)
	h.writeBytes(w, http.StatusCreated, view)
}

// issueCreateBody is the fast path for POST /issues: the common body
// unmarshals straight into a struct instead of a map, which skips the
// reflection-heavy map decode.
type issueCreateBody struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// readIssueCreate reads an issue-create body. A body the struct cannot
// hold (a non-string title, labels that are not strings) falls back to
// the map path, so behaviour for odd bodies is unchanged, and malformed
// JSON still answers 400.
func (h *Handler) readIssueCreate(w http.ResponseWriter, r *http.Request) (issueCreateBody, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Bad Request")
		return issueCreateBody{}, false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return issueCreateBody{}, true
	}
	var body issueCreateBody
	if err := json.Unmarshal(raw, &body); err == nil {
		return body, true
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		h.writeError(w, http.StatusBadRequest, "Bad Request")
		return issueCreateBody{}, false
	}
	body.Title = stringField(generic, "title")
	body.Body = stringField(generic, "body")
	if list, isList := generic["labels"].([]any); isList {
		for _, entry := range list {
			if name, isString := entry.(string); isString {
				body.Labels = append(body.Labels, name)
			}
		}
	}
	return body, true
}

func (h *Handler) patchIssue(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, number int) {
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	var title, text, state *string
	if raw, present := body["title"]; present {
		if value, isString := raw.(string); isString {
			title = &value
		}
	}
	if raw, present := body["body"]; present {
		if value, isString := raw.(string); isString {
			text = &value
		}
	}
	if raw, present := body["state"]; present {
		if value, isString := raw.(string); isString {
			state = &value
		}
	}
	issue, patched := repo.PatchIssue(number, title, text, state)
	if !patched {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	// PatchIssue already invalidated the cached view; store the fresh
	// serialisation so the next GET serves it.
	view, err := issueViewJSON(repo, issue)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	repo.SetIssueView(number, view)
	h.writeBytes(w, http.StatusOK, view)
}

// pullJSON is the pull-request object the checks read. Head and base shas
// are the branches' current tips.
func pullJSON(repo *gitproto.Repo, pull *gitproto.Pull) map[string]any {
	headSHA, _ := repo.PullHeadSHA(pull)
	baseSHA, _ := repo.PullBaseSHA(pull)
	var mergedAt any
	if pull.MergedAt != "" {
		mergedAt = pull.MergedAt
	}
	var mergeCommitSHA any
	if pull.MergeCommitSHA != "" {
		mergeCommitSHA = pull.MergeCommitSHA
	}
	return map[string]any{
		"id":               pull.DBID,
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSHA,
		"head":             map[string]string{"ref": pull.HeadRef, "sha": headSHA},
		"base":             map[string]string{"ref": pull.BaseRef, "sha": baseSHA},
	}
}

// servePulls handles /pulls, /pulls/{n}, /pulls/{n}/files and
// /pulls/{n}/merge.
func (h *Handler) servePulls(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, tail []string) {
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listPulls(w, r, repo)
		case http.MethodPost:
			h.createPull(w, r, repo)
		default:
			h.writeError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		h.writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(tail) == 1 {
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		pull, ok := repo.GetPull(number)
		if !ok {
			h.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		h.writeJSON(w, http.StatusOK, pullJSON(repo, pull))
		return
	}
	if len(tail) == 2 && tail[1] == "files" && r.Method == http.MethodGet {
		changes, ok := repo.PullFiles(number)
		if !ok {
			h.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		items := make([]map[string]any, 0, len(changes))
		for _, change := range changes {
			items = append(items, map[string]any{
				"filename":  change.Path,
				"status":    change.Status,
				"additions": change.Additions,
				"deletions": change.Deletions,
				"changes":   change.Additions + change.Deletions,
			})
		}
		h.writeJSON(w, http.StatusOK, items)
		return
	}
	if len(tail) == 2 && tail[1] == "merge" && r.Method == http.MethodPut {
		h.mergePull(w, r, repo, number)
		return
	}
	h.writeError(w, http.StatusNotFound, "Not Found")
}

func (h *Handler) listPulls(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	pulls := repo.ListPulls(state)
	page, perPage := pageParams(r)
	total := len(pulls)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	items := make([]map[string]any, 0, end-start)
	for _, pull := range pulls[start:end] {
		items = append(items, pullJSON(repo, pull))
	}
	if end < total {
		h.setLinkNext(w, r, page+1, perPage)
	}
	h.writeJSON(w, http.StatusOK, items)
}

func (h *Handler) createPull(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo) {
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	title := stringField(body, "title")
	head := stringField(body, "head")
	base := stringField(body, "base")
	if title == "" || head == "" || base == "" {
		h.writeError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	pull, err := repo.CreatePull(title, stringField(body, "body"), head, base)
	if err != nil {
		h.writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	h.writeJSON(w, http.StatusCreated, pullJSON(repo, pull))
}

func (h *Handler) mergePull(w http.ResponseWriter, r *http.Request, repo *gitproto.Repo, number int) {
	if _, ok := h.readBody(w, r); !ok {
		return
	}
	sha, err := repo.MergePull(number)
	if err != nil {
		switch err {
		case gitproto.ErrPullNotFound:
			h.writeError(w, http.StatusNotFound, "Not Found")
		case gitproto.ErrPullClosed, gitproto.ErrPullConflict:
			h.writeError(w, http.StatusMethodNotAllowed, err.Error())
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"merged":           true,
		"sha":              sha,
		"message":          "Pull request successfully merged",
		"merge_commit_sha": sha,
	})
}
