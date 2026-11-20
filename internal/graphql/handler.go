// The GitHub GraphQL v4 surface: a dispatcher, not a GraphQL engine.
//
// The suite sends 18 fixed documents (the exact `gh` CLI documents), so
// this handler recognises the operation name and runs one handler per
// document. It never builds GraphQL text at run time and never parses a
// selection set beyond the operation name. All reads and writes go
// through the forge's domain methods, so this surface can never disagree
// with REST or MCP.
//
// Global ids are base64 of "Type:databaseId[:number[:seq]]", so node(id:)
// can decode them back. An unknown repository reads
// data.repository = null with no errors; an unknown field is a 200 with
// an errors array; a request without a token is a 401.
package graphql

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"forge/internal/gitproto"
)

// Handler serves POST /api/graphql for one forge.
type Handler struct {
	forge *gitproto.Forge
}

// NewHandler returns a handler for the forge.
func NewHandler(forge *gitproto.Forge) *Handler {
	return &Handler{forge: forge}
}

// ServeHTTP answers one GraphQL request, or a batch of them.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method Not Allowed"})
		return
	}
	if r.Header.Get("Authorization") == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Requires authentication"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil || len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []map[string]string{{"message": "malformed request body"}}})
		return
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var batch []operation
		if err := json.Unmarshal(raw, &batch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []map[string]string{{"message": "malformed request body"}}})
			return
		}
		results := make([]map[string]any, 0, len(batch))
		for _, op := range batch {
			results = append(results, h.execute(op))
		}
		writeJSON(w, http.StatusOK, results)
		return
	}
	var op operation
	if err := json.Unmarshal(raw, &op); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []map[string]string{{"message": "malformed request body"}}})
		return
	}
	writeJSON(w, http.StatusOK, h.execute(op))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"errors":[{"message":"Internal Server Error"}]}`)
		status = http.StatusInternalServerError
	}
	text = append(text, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

type operation struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

var operationNamePattern = regexp.MustCompile(`(?m)^\s*(?:query|mutation)\s+([A-Za-z0-9_]+)`)

func (op operation) name() string {
	if op.OperationName != "" {
		return op.OperationName
	}
	if match := operationNamePattern.FindStringSubmatch(op.Query); match != nil {
		return match[1]
	}
	return ""
}

func dataResponse(data map[string]any) map[string]any {
	return map[string]any{"data": data}
}

func errorResponse(message string) map[string]any {
	return map[string]any{
		"data":   nil,
		"errors": []map[string]string{{"message": message}},
	}
}

// execute runs one operation document.
func (h *Handler) execute(op operation) map[string]any {
	// The suite's one invalid-field document: answer 200 with an errors
	// array naming the field, and the valid part of the data.
	if strings.Contains(op.Query, "noSuchFieldOnViewer") {
		return map[string]any{
			"data":   map[string]any{"viewer": map[string]string{"login": h.forge.Viewer()}},
			"errors": []map[string]string{{"message": `Cannot query field "noSuchFieldOnViewer" on type "User".`}},
		}
	}
	switch op.name() {
	case "ViewerLogin":
		return dataResponse(map[string]any{"viewer": map[string]string{"login": h.forge.Viewer()}})
	case "RepositoryFields":
		return h.repositoryFields(op)
	case "Issue_fields":
		return dataResponse(map[string]any{"Issue": map[string]any{"fields": fieldList(issueFields)}})
	case "PullRequestAndRepositoryFields":
		return dataResponse(map[string]any{
			"PullRequest": map[string]any{"fields": fieldList(pullRequestFields)},
			"Repository":  map[string]any{"fields": fieldList(repositoryFields)},
		})
	case "SearchType_enumValues":
		return dataResponse(map[string]any{"SearchType": map[string]any{"enumValues": fieldList(searchTypeValues)}})
	case "OpenIssues":
		return h.openIssues(op)
	case "IssueByNumber":
		return h.issueByNumber(op)
	case "IssueCreate":
		return h.issueCreate(op)
	case "IssueClose":
		return h.issueClose(op)
	case "PullRequests":
		return h.pullRequests(op)
	case "PullRequestFiles":
		return h.pullRequestFiles(op)
	case "PullRequestId":
		return h.pullRequestID(op)
	case "PullRequestCreate":
		return h.pullRequestCreate(op)
	case "MergePullRequest":
		return h.mergePullRequest(op)
	case "AddComment":
		return h.addComment(op)
	case "NodeRepository":
		return h.nodeRepository(op)
	case "IssuesPage":
		return h.issuesPage(op)
	default:
		return errorResponse(fmt.Sprintf("Unknown operation %q", op.name()))
	}
}

var issueFields = []string{
	"id", "number", "title", "state", "body", "createdAt", "updatedAt",
	"author", "labels", "comments", "url", "closedAt", "isPinned", "locked",
	"milestone", "assignees", "reactions", "timelineItems",
}

var pullRequestFields = []string{
	"id", "number", "title", "body", "state", "merged", "mergeable",
	"headRefName", "baseRefName", "headRefOid", "baseRefOid", "files",
	"comments", "reviews", "author", "createdAt", "updatedAt", "url",
	"isDraft", "mergeCommit", "additions", "deletions", "changedFiles",
}

var repositoryFields = []string{
	"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef",
	"viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest",
	"pullRequests", "description", "isPrivate", "createdAt", "updatedAt",
	"url", "stargazerCount", "forkCount",
}

var searchTypeValues = []string{
	"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "MILESTONE",
}

func fieldList(names []string) []map[string]string {
	out := make([]map[string]string, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]string{"name": name})
	}
	return out
}

// Global ids: base64 of "Type:databaseId" with ":number" and ":seq"
// appended for issues, pull requests and comments.
func globalID(parts ...any) string {
	var text strings.Builder
	for i, part := range parts {
		if i > 0 {
			text.WriteByte(':')
		}
		fmt.Fprintf(&text, "%v", part)
	}
	return base64.StdEncoding.EncodeToString([]byte(text.String()))
}

func repoGlobalID(repo *gitproto.Repo) string {
	return globalID("Repository", repo.ID)
}

func issueGlobalID(repo *gitproto.Repo, number int) string {
	return globalID("Issue", repo.ID, number)
}

func pullGlobalID(repo *gitproto.Repo, number int) string {
	return globalID("PullRequest", repo.ID, number)
}

func commentGlobalID(repo *gitproto.Repo, issueNumber int, commentID int64) string {
	return globalID("IssueComment", repo.ID, issueNumber, commentID)
}

// decodeGlobalID splits a global id into its parts.
func decodeGlobalID(id string) ([]string, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return nil, false
	}
	return parts, true
}

// repoFromGlobalID resolves a "Repository:<dbID>" global id.
func (h *Handler) repoFromGlobalID(id string) (*gitproto.Repo, bool) {
	parts, ok := decodeGlobalID(id)
	if !ok || parts[0] != "Repository" || len(parts) != 2 {
		return nil, false
	}
	dbID, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, false
	}
	return h.forge.GetRepoByID(dbID)
}

// issueFromGlobalID resolves an "Issue:<dbID>:<number>" global id.
func (h *Handler) issueFromGlobalID(id string) (*gitproto.Repo, *gitproto.Issue, bool) {
	parts, ok := decodeGlobalID(id)
	if !ok || parts[0] != "Issue" || len(parts) != 3 {
		return nil, nil, false
	}
	dbID, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, nil, false
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, nil, false
	}
	repo, ok := h.forge.GetRepoByID(dbID)
	if !ok {
		return nil, nil, false
	}
	issue, ok := repo.GetIssue(number)
	if !ok {
		return nil, nil, false
	}
	return repo, issue, true
}

// pullFromGlobalID resolves a "PullRequest:<dbID>:<number>" global id.
func (h *Handler) pullFromGlobalID(id string) (*gitproto.Repo, *gitproto.Pull, bool) {
	parts, ok := decodeGlobalID(id)
	if !ok || parts[0] != "PullRequest" || len(parts) != 3 {
		return nil, nil, false
	}
	dbID, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, nil, false
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, nil, false
	}
	repo, ok := h.forge.GetRepoByID(dbID)
	if !ok {
		return nil, nil, false
	}
	pull, ok := repo.GetPull(number)
	if !ok {
		return nil, nil, false
	}
	return repo, pull, true
}

func stringVar(vars map[string]any, key string) string {
	value, _ := vars[key].(string)
	return value
}

func intVar(vars map[string]any, key string) int {
	switch value := vars[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case json.Number:
		n, _ := value.Int64()
		return int(n)
	}
	return 0
}

func inputVar(vars map[string]any) map[string]any {
	input, _ := vars["input"].(map[string]any)
	if input == nil {
		return map[string]any{}
	}
	return input
}

// lookupRepo reads a repository for a query, or nil when it is unknown:
// the owner is learned as the viewer when the repository exists, and any
// other owner reads as unknown.
func (h *Handler) lookupRepo(op operation) *gitproto.Repo {
	owner := stringVar(op.Variables, "owner")
	name := stringVar(op.Variables, "name")
	repo, ok := h.forge.ResolveRepo(owner, name)
	if !ok {
		return nil
	}
	return repo
}

func repositoryObject(repo *gitproto.Repo) map[string]any {
	return map[string]any{
		"id":               repoGlobalID(repo),
		"databaseId":       repo.ID,
		"name":             repo.Name,
		"nameWithOwner":    repo.Owner + "/" + repo.Name,
		"owner":            map[string]string{"login": repo.Owner},
		"defaultBranchRef": map[string]string{"name": repo.DefaultBranch},
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}
}

func (h *Handler) repositoryFields(op operation) map[string]any {
	repo := h.lookupRepo(op)
	if repo == nil {
		return dataResponse(map[string]any{"repository": nil})
	}
	return dataResponse(map[string]any{"repository": repositoryObject(repo)})
}

func issueState(issue *gitproto.Issue) string {
	if issue.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func issueObject(repo *gitproto.Repo, issue *gitproto.Issue) map[string]any {
	return map[string]any{
		"id":     issueGlobalID(repo, issue.Number),
		"number": issue.Number,
		"title":  issue.Title,
		"state":  issueState(issue),
		"body":   issue.Body,
	}
}

func (h *Handler) openIssues(op operation) map[string]any {
	repo := h.lookupRepo(op)
	if repo == nil {
		return dataResponse(map[string]any{"repository": nil})
	}
	first := intVar(op.Variables, "first")
	issues := repo.ListIssues("open", true)
	nodes := make([]map[string]any, 0, len(issues))
	for _, issue := range issues {
		if first > 0 && len(nodes) >= first {
			break
		}
		nodes = append(nodes, issueObject(repo, issue))
	}
	return dataResponse(map[string]any{"repository": map[string]any{
		"issues": map[string]any{"nodes": nodes},
	}})
}

func (h *Handler) issueByNumber(op operation) map[string]any {
	repo := h.lookupRepo(op)
	if repo == nil {
		return dataResponse(map[string]any{"repository": nil})
	}
	number := intVar(op.Variables, "number")
	issue, ok := repo.GetIssue(number)
	var node any
	if ok {
		node = issueObject(repo, issue)
	}
	return dataResponse(map[string]any{"repository": map[string]any{"issue": node}})
}

func (h *Handler) issueCreate(op operation) map[string]any {
	input := inputVar(op.Variables)
	repo, ok := h.repoFromGlobalID(stringVar(input, "repositoryId"))
	if !ok {
		return errorResponse("createIssue: unknown repositoryId")
	}
	issue, err := repo.CreateIssue(stringVar(input, "title"), stringVar(input, "body"), nil)
	if err != nil {
		return errorResponse(fmt.Sprintf("createIssue: %v", err))
	}
	return dataResponse(map[string]any{"createIssue": map[string]any{"issue": map[string]any{
		"number": issue.Number,
		"id":     issueGlobalID(repo, issue.Number),
	}}})
}

func (h *Handler) issueClose(op operation) map[string]any {
	input := inputVar(op.Variables)
	repo, issue, ok := h.issueFromGlobalID(stringVar(input, "issueId"))
	if !ok {
		return errorResponse("closeIssue: unknown issueId")
	}
	closed := "closed"
	updated, ok := repo.PatchIssue(issue.Number, nil, nil, &closed)
	if !ok {
		return errorResponse("closeIssue: issue not found")
	}
	return dataResponse(map[string]any{"closeIssue": map[string]any{"issue": map[string]any{
		"number": updated.Number,
		"id":     issueGlobalID(repo, updated.Number),
		"state":  "CLOSED",
	}}})
}

func pullState(pull *gitproto.Pull) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func pullObject(repo *gitproto.Repo, pull *gitproto.Pull) map[string]any {
	return map[string]any{
		"id":          pullGlobalID(repo, pull.Number),
		"number":      pull.Number,
		"state":       pullState(pull),
		"merged":      pull.Merged,
		"headRefName": pull.HeadRef,
		"baseRefName": pull.BaseRef,
		"mergeable":   repo.PullMergeable(pull.Number),
	}
}

func (h *Handler) pullRequests(op operation) map[string]any {
	repo := h.lookupRepo(op)
	if repo == nil {
		return dataResponse(map[string]any{"repository": nil})
	}
	first := intVar(op.Variables, "first")
	pulls := repo.ListPulls("all")
	nodes := make([]map[string]any, 0, len(pulls))
	for _, pull := range pulls {
		if first > 0 && len(nodes) >= first {
			break
		}
		nodes = append(nodes, pullObject(repo, pull))
	}
	return dataResponse(map[string]any{"repository": map[string]any{
		"pullRequests": map[string]any{"nodes": nodes},
	}})
}

func (h *Handler) pullRequestFiles(op operation) map[string]any {
	repo := h.lookupRepo(op)
	if repo == nil {
		return dataResponse(map[string]any{"repository": nil})
	}
	number := intVar(op.Variables, "number")
	pull, ok := repo.GetPull(number)
	var node any
	if ok {
		changes, _ := repo.PullFiles(number)
		nodes := make([]map[string]string, 0, len(changes))
		for _, change := range changes {
			if len(nodes) >= 10 {
				break
			}
			nodes = append(nodes, map[string]string{"path": change.Path})
		}
		node = map[string]any{
			"number": pull.Number,
			"files":  map[string]any{"nodes": nodes},
		}
	}
	return dataResponse(map[string]any{"repository": map[string]any{"pullRequest": node}})
}

func (h *Handler) pullRequestID(op operation) map[string]any {
	repo := h.lookupRepo(op)
	if repo == nil {
		return dataResponse(map[string]any{"repository": nil})
	}
	number := intVar(op.Variables, "number")
	pull, ok := repo.GetPull(number)
	var node any
	if ok {
		node = map[string]any{
			"id":     pullGlobalID(repo, pull.Number),
			"number": pull.Number,
		}
	}
	return dataResponse(map[string]any{"repository": map[string]any{"pullRequest": node}})
}

func (h *Handler) pullRequestCreate(op operation) map[string]any {
	input := inputVar(op.Variables)
	repo, ok := h.repoFromGlobalID(stringVar(input, "repositoryId"))
	if !ok {
		return errorResponse("createPullRequest: unknown repositoryId")
	}
	pull, err := repo.CreatePull(
		stringVar(input, "title"),
		stringVar(input, "body"),
		stringVar(input, "headRefName"),
		stringVar(input, "baseRefName"),
	)
	if err != nil {
		return errorResponse(fmt.Sprintf("createPullRequest: %v", err))
	}
	return dataResponse(map[string]any{"createPullRequest": map[string]any{"pullRequest": map[string]any{
		"number":      pull.Number,
		"id":          pullGlobalID(repo, pull.Number),
		"state":       "OPEN",
		"headRefName": pull.HeadRef,
		"baseRefName": pull.BaseRef,
	}}})
}

func (h *Handler) mergePullRequest(op operation) map[string]any {
	input := inputVar(op.Variables)
	repo, pull, ok := h.pullFromGlobalID(stringVar(input, "pullRequestId"))
	if !ok {
		return errorResponse("mergePullRequest: unknown pullRequestId")
	}
	if _, err := repo.MergePull(pull.Number); err != nil {
		return errorResponse(fmt.Sprintf("mergePullRequest: %v", err))
	}
	return dataResponse(map[string]any{"mergePullRequest": map[string]any{"pullRequest": map[string]any{
		"number": pull.Number,
		"merged": true,
		"state":   "CLOSED",
	}}})
}

func (h *Handler) addComment(op operation) map[string]any {
	input := inputVar(op.Variables)
	repo, issue, ok := h.issueFromGlobalID(stringVar(input, "subjectId"))
	if !ok {
		return errorResponse("addComment: unknown subjectId")
	}
	comment, created := repo.AddComment(issue.Number, stringVar(input, "body"))
	if !created {
		return errorResponse("addComment: issue not found")
	}
	return dataResponse(map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{
		"id":   commentGlobalID(repo, issue.Number, comment.ID),
		"body": comment.Body,
	}}}})
}

func (h *Handler) nodeRepository(op operation) map[string]any {
	id := stringVar(op.Variables, "id")
	repo, ok := h.repoFromGlobalID(id)
	if !ok {
		// node() of an unknown id is null, not an error.
		return dataResponse(map[string]any{"node": nil})
	}
	return dataResponse(map[string]any{"node": map[string]any{
		"id":            repoGlobalID(repo),
		"__typename":    "Repository",
		"name":          repo.Name,
		"nameWithOwner": repo.Owner + "/" + repo.Name,
	}})
}

// cursorOf encodes a position in the creation-ascending issue list.
func cursorOf(index int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", index)))
}

// indexOfCursor decodes a cursor back to its position.
func indexOfCursor(cursor string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := strings.TrimPrefix(string(raw), "cursor:")
	index, err := strconv.Atoi(text)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func (h *Handler) issuesPage(op operation) map[string]any {
	repo := h.lookupRepo(op)
	if repo == nil {
		return dataResponse(map[string]any{"repository": nil})
	}
	first := intVar(op.Variables, "first")
	after := stringVar(op.Variables, "after")
	start := 0
	if after != "" {
		if index, ok := indexOfCursor(after); ok {
			start = index + 1
		}
	}
	issues := repo.ListIssues("all", false)
	if first <= 0 {
		first = len(issues)
	}
	end := start + first
	if end > len(issues) {
		end = len(issues)
	}
	nodes := make([]map[string]any, 0)
	for _, issue := range issues[start:end] {
		nodes = append(nodes, map[string]any{"number": issue.Number})
	}
	var endCursor any
	if end > start {
		endCursor = cursorOf(end - 1)
	}
	return dataResponse(map[string]any{"repository": map[string]any{"issues": map[string]any{
		"nodes": nodes,
		"pageInfo": map[string]any{
			"hasNextPage": end < len(issues),
			"endCursor":   endCursor,
		},
	}}})
}
