// The Model Context Protocol surface: JSON-RPC 2.0 over streamable HTTP
// at POST /mcp, speaking both protocol eras the suite's client knows.
//
//   modern  2026-07-28: server/discover answers identity, capabilities
//           and supportedVersions in one request; no handshake.
//   legacy  2025-11-25 and earlier: initialize echoes the revision the
//           client offered, then notifications/initialized.
//
// The six canonical tools are create_repo, get_repo, create_issue,
// list_issues, create_pull_request and merge_pull_request. Every tool
// result carries both a text block (a JSON string) and structuredContent,
// so either reader the client uses sees the same object. The tools/list
// answer is marshalled once, at construction, so two calls are
// byte-identical. All reads and writes go through the forge's domain
// methods, so MCP agrees with REST and GraphQL by construction.
//
// Errors: an unknown tool is -32601, missing required arguments are
// -32602, a body that is not JSON is HTTP 400 with -32700, and a request
// without Authorization is HTTP 401. The JSON-RPC id is echoed verbatim,
// so concurrent calls keep their own answers.
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"forge/internal/gitproto"
)

// Handler serves POST /mcp for one forge.
type Handler struct {
	forge    *gitproto.Forge
	tools    []tool
	toolsRaw json.RawMessage // the tools array, marshalled once
}

// NewHandler returns a handler for the forge.
func NewHandler(forge *gitproto.Forge) *Handler {
	h := &Handler{forge: forge, tools: canonicalTools()}
	raw, err := json.Marshal(h.tools)
	if err != nil {
		raw = []byte("[]")
	}
	h.toolsRaw = raw
	return h
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func schema(properties map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func booleanProperty(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// canonicalTools is the fixed tool list, in a fixed order. The required
// arrays name only properties the suite has values for (see the kit's
// ARGUMENT_SYNONYMS table): a required property outside that table fails
// the suite's mapping before the call is even made.
func canonicalTools() []tool {
	return []tool{
		{
			Name:        "create_repo",
			Description: "Create a repository owned by the authenticated user.",
			InputSchema: schema(map[string]any{
				"name":        stringProperty("The repository name."),
				"description": stringProperty("A short description of the repository."),
				"private":     booleanProperty("Whether the repository is private."),
			}, []string{"name"}),
		},
		{
			Name:        "get_repo",
			Description: "Read one repository by owner and name.",
			InputSchema: schema(map[string]any{
				"owner": stringProperty("The login of the repository owner."),
				"repo":  stringProperty("The repository name."),
			}, []string{"owner", "repo"}),
		},
		{
			Name:        "create_issue",
			Description: "Create an issue in a repository.",
			InputSchema: schema(map[string]any{
				"owner": stringProperty("The login of the repository owner."),
				"repo":  stringProperty("The repository name."),
				"title": stringProperty("The issue title."),
				"body":  stringProperty("The issue body."),
			}, []string{"owner", "repo", "title"}),
		},
		{
			Name:        "list_issues",
			Description: "List the issues of a repository.",
			InputSchema: schema(map[string]any{
				"owner": stringProperty("The login of the repository owner."),
				"repo":  stringProperty("The repository name."),
				"state": stringProperty("Filter by state: open, closed or all."),
			}, []string{"owner", "repo"}),
		},
		{
			Name:        "create_pull_request",
			Description: "Open a pull request from a head branch into a base branch.",
			InputSchema: schema(map[string]any{
				"owner": stringProperty("The login of the repository owner."),
				"repo":  stringProperty("The repository name."),
				"title": stringProperty("The pull request title."),
				"body":  stringProperty("The pull request body."),
				"head":  stringProperty("The head branch name."),
				"base":  stringProperty("The base branch name."),
			}, []string{"owner", "repo", "title", "head", "base"}),
		},
		{
			Name:        "merge_pull_request",
			Description: "Merge a pull request with a squash merge.",
			InputSchema: schema(map[string]any{
				"owner":       stringProperty("The login of the repository owner."),
				"repo":        stringProperty("The repository name."),
				"pull_number": map[string]any{"type": "integer", "description": "The pull request number."},
			}, []string{"owner", "repo", "pull_number"}),
		},
	}
}

// ServeHTTP answers one JSON-RPC request at /mcp.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		// Session teardown: nothing is held, so there is nothing to close.
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method Not Allowed"})
		return
	}
	if r.Header.Get("Authorization") == "" {
		writeError(w, http.StatusUnauthorized, nil, -32000, "Requires authentication")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil || len(raw) == 0 {
		writeError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	// A notification has no id; a request must carry one.
	var probe struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		writeError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	if probe.Method == "" {
		writeError(w, http.StatusBadRequest, idOf(probe.ID), -32600, "Invalid Request")
		return
	}
	isNotification := len(bytes.TrimSpace(probe.ID)) == 0
	if isNotification {
		// Notifications get an empty 202; the client ignores the answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	id := idOf(probe.ID)
	var params map[string]any
	if len(probe.Params) > 0 {
		_ = json.Unmarshal(probe.Params, &params)
	}
	if params == nil {
		params = map[string]any{}
	}
	switch probe.Method {
	case "server/discover":
		writeResult(w, id, map[string]any{
			"protocolVersion":   "2026-07-28",
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "forge", "version": "1.0.0"},
			},
		})
	case "initialize":
		version := "2025-11-25"
		if offered, ok := params["protocolVersion"].(string); ok && offered != "" {
			version = offered
		}
		writeResult(w, id, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
		})
	case "ping":
		writeResult(w, id, map[string]any{})
	case "tools/list":
		writeRawResult(w, id, []byte(`{"tools":`+string(h.toolsRaw)+`}`))
	case "tools/call":
		h.callTool(w, id, params)
	default:
		writeError(w, http.StatusOK, id, -32601, fmt.Sprintf("Method not found: %s", probe.Method))
	}
}

// idOf decodes a JSON-RPC id, keeping its type (string or number).
func idOf(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"message":"Internal Server Error"}`)
		status = http.StatusInternalServerError
	}
	text = append(text, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

func writeResult(w http.ResponseWriter, id any, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// writeRawResult answers with a result whose JSON was built once.
func writeRawResult(w http.ResponseWriter, id any, resultJSON []byte) {
	idText, _ := json.Marshal(id)
	body := append([]byte(`{"jsonrpc":"2.0","id":`), idText...)
	body = append(body, []byte(`,"result":`)...)
	body = append(body, resultJSON...)
	body = append(body, []byte("}\n")...)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, id any, code int, message string) {
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

// callTool runs one tools/call request.
func (h *Handler) callTool(w http.ResponseWriter, id any, params map[string]any) {
	name, _ := params["name"].(string)
	arguments, _ := params["arguments"].(map[string]any)
	if arguments == nil {
		arguments = map[string]any{}
	}
	known := false
	for _, tool := range h.tools {
		if tool.Name == name {
			known = true
			break
		}
	}
	if !known {
		writeError(w, http.StatusOK, id, -32601, fmt.Sprintf("Unknown tool: %s", name))
		return
	}
	result, err := h.runTool(name, arguments)
	if err != nil {
		// A tool that cannot run because its arguments are missing is an
		// invalid-params error; anything else is a tool error the client
		// reads from isError.
		if missingErr, ok := err.(*missingArgumentsError); ok {
			writeError(w, http.StatusOK, id, -32602, missingErr.Error())
			return
		}
		writeToolResult(w, id, map[string]any{"error": err.Error()}, true)
		return
	}
	writeToolResult(w, id, result, false)
}

// writeToolResult answers a tools/call with text and structured content
// carrying the same object.
func writeToolResult(w http.ResponseWriter, id any, result any, isError bool) {
	text, _ := json.Marshal(result)
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content":           []map[string]string{{"type": "text", "text": string(text)}},
			"structuredContent": result,
			"isError":           isError,
		},
	})
}

type missingArgumentsError struct{ names []string }

func (e *missingArgumentsError) Error() string {
	return fmt.Sprintf("missing required arguments: %s", strings.Join(e.names, ", "))
}

// --- argument helpers -------------------------------------------------

// firstString returns the first non-empty string among the keys.
func firstString(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func firstBool(args map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		if value, ok := args[key].(bool); ok {
			return value, true
		}
	}
	return false, false
}

func firstInt(args map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		switch value := args[key].(type) {
		case float64:
			return int(value), true
		case int:
			return value, true
		case json.Number:
			n, err := value.Int64()
			if err == nil {
				return int(n), true
			}
		case string:
			if n, err := strconv.Atoi(value); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// resolveRepo finds the repository the arguments name. The owner
// defaults to the viewer learned so far; a full "owner/repo" name is
// split. An owner other than the viewer resolves to not found.
func (h *Handler) resolveRepo(args map[string]any) (*gitproto.Repo, error) {
	owner := firstString(args, "owner", "owner_login", "org", "organization", "organisation", "user", "user_login", "login", "account", "namespace", "owner_name")
	name := firstString(args, "repo", "repository", "repository_name", "repo_name", "repo_slug", "project", "project_name")
	if name == "" {
		if full := firstString(args, "full_name", "repo_full_name", "repository_full_name", "fullName"); full != "" {
			owner, name = splitFullName(full)
		}
	}
	if name == "" {
		return nil, &missingArgumentsError{names: []string{"repo"}}
	}
	if owner == "" {
		owner = h.forge.Viewer()
	}
	repo, ok := h.forge.ResolveRepo(owner, name)
	if !ok {
		return nil, fmt.Errorf("repository %s/%s not found", owner, name)
	}
	return repo, nil
}

func splitFullName(full string) (string, string) {
	parts := strings.SplitN(full, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", full
}

func repoObject(repo *gitproto.Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]string{"login": repo.Owner},
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
	}
}

func issueObject(repo *gitproto.Repo, issue *gitproto.Issue) map[string]any {
	return map[string]any{
		"id":     issue.DBID,
		"number": issue.Number,
		"title":  issue.Title,
		"body":   issue.Body,
		"state":  issue.State,
		"labels": issue.Labels,
	}
}

func pullObject(repo *gitproto.Repo, pull *gitproto.Pull) map[string]any {
	return map[string]any{
		"id":     pull.DBID,
		"number": pull.Number,
		"title":  pull.Title,
		"body":   pull.Body,
		"state":  pull.State,
		"merged": pull.Merged,
		"head":   map[string]string{"ref": pull.HeadRef},
		"base":   map[string]string{"ref": pull.BaseRef},
	}
}

// runTool executes one of the six tools.
func (h *Handler) runTool(name string, args map[string]any) (map[string]any, error) {
	switch name {
	case "create_repo":
		repoName := firstString(args, "name", "repo", "repository", "repository_name", "repo_name")
		if repoName == "" {
			return nil, &missingArgumentsError{names: []string{"name"}}
		}
		description := firstString(args, "description", "about", "summary", "body")
		private, _ := firstBool(args, "private", "is_private")
		repo, created := h.forge.CreateRepo(repoName, description, private)
		if !created {
			return nil, fmt.Errorf("repository %s already exists", repoName)
		}
		return repoObject(repo), nil
	case "get_repo":
		repo, err := h.resolveRepo(args)
		if err != nil {
			return nil, err
		}
		return repoObject(repo), nil
	case "create_issue":
		repo, err := h.resolveRepo(args)
		if err != nil {
			return nil, err
		}
		title := firstString(args, "title", "issue_title", "pr_title", "pull_request_title", "subject")
		if title == "" {
			return nil, &missingArgumentsError{names: []string{"title"}}
		}
		body := firstString(args, "body", "issue_body", "description", "content", "text", "comment")
		issue, err := repo.CreateIssue(title, body, nil)
		if err != nil {
			return nil, err
		}
		return issueObject(repo, issue), nil
	case "list_issues":
		repo, err := h.resolveRepo(args)
		if err != nil {
			return nil, err
		}
		state := firstString(args, "state", "status")
		if state == "" {
			state = "open"
		}
		issues := repo.ListIssues(state, false)
		list := make([]map[string]any, 0, len(issues))
		for _, issue := range issues {
			list = append(list, issueObject(repo, issue))
		}
		return map[string]any{"issues": list}, nil
	case "create_pull_request":
		repo, err := h.resolveRepo(args)
		if err != nil {
			return nil, err
		}
		title := firstString(args, "title", "issue_title", "pr_title", "pull_request_title", "subject")
		head := firstString(args, "head", "head_ref", "head_branch", "pull_request_head", "pr_head", "source", "source_ref", "source_branch", "from", "from_branch", "branch")
		base := firstString(args, "base", "base_ref", "base_branch", "pull_request_base", "pr_base", "target", "target_ref", "target_branch", "into", "into_branch")
		var missing []string
		if title == "" {
			missing = append(missing, "title")
		}
		if head == "" {
			missing = append(missing, "head")
		}
		if base == "" {
			missing = append(missing, "base")
		}
		if len(missing) > 0 {
			return nil, &missingArgumentsError{names: missing}
		}
		body := firstString(args, "body", "issue_body", "description", "content", "text", "comment")
		pull, err := repo.CreatePull(title, body, head, base)
		if err != nil {
			return nil, err
		}
		return pullObject(repo, pull), nil
	case "merge_pull_request":
		repo, err := h.resolveRepo(args)
		if err != nil {
			return nil, err
		}
		number, ok := firstInt(args, "pull_number", "pull_request_number", "pr_number", "number", "issue_number", "index")
		if !ok {
			return nil, &missingArgumentsError{names: []string{"pull_number"}}
		}
		sha, err := repo.MergePull(number)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"merged":  true,
			"number":  number,
			"state":   "closed",
			"sha":     sha,
			"message": fmt.Sprintf("Merge pull request #%d", number),
		}, nil
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}
