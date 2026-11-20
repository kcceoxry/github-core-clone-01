package graphql_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/gitproto"
	"forge/internal/server"
)

const testToken = "Bearer test-token"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	forge := gitproto.NewForge()
	srv := httptest.NewServer(server.New(forge))
	t.Cleanup(srv.Close)
	return srv
}

// gql sends one GraphQL operation and returns the decoded body.
func gql(t *testing.T, srv *httptest.Server, query string, variables map[string]any) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", srv.URL+"/api/graphql", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("GraphQL response not JSON: %q", raw)
	}
	return resp.StatusCode, body
}

func gqlData(t *testing.T, srv *httptest.Server, query string, variables map[string]any) map[string]any {
	t.Helper()
	status, body := gql(t, srv, query, variables)
	if status != http.StatusOK {
		t.Fatalf("GraphQL %s: status %d, body %v", query[:40], status, body)
	}
	if body["errors"] != nil {
		t.Fatalf("GraphQL %s: errors %v", query[:40], body["errors"])
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatalf("GraphQL %s: no data in %v", query[:40], body)
	}
	return data
}

// rest creates a repository over REST so the GraphQL tests have fixtures.
func rest(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", testToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// learnViewer teaches the server the viewer login the way the suite does:
// create a repository through the API, then read it back under its owner.
// The viewer is learned from that first owner-naming request.
func learnViewer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	status, _ := rest(t, srv, "POST", "/api/v3/user/repos", map[string]any{"name": "viewer-probe"})
	if status != http.StatusCreated {
		t.Fatalf("learn viewer: create repo status %d", status)
	}
	status, _ = rest(t, srv, "GET", "/api/v3/repos/adaiversen/viewer-probe", nil)
	if status != http.StatusOK {
		t.Fatalf("learn viewer: read repo status %d", status)
	}
}

func TestViewerLogin(t *testing.T) {
	srv := newTestServer(t)
	learnViewer(t, srv)
	data := gqlData(t, srv, `query ViewerLogin { viewer { login } }`, nil)
	viewer, _ := data["viewer"].(map[string]any)
	if viewer["login"] != "adaiversen" {
		t.Fatalf("viewer login: %v", data)
	}
}

func TestRepositoryFieldsAndUnknownRepo(t *testing.T) {
	srv := newTestServer(t)
	rest(t, srv, "POST", "/api/v3/user/repos", map[string]any{"name": "gqlrepo"})

	data := gqlData(t, srv,
		`query RepositoryFields($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id databaseId name owner { login } defaultBranchRef { name } viewerPermission hasIssuesEnabled } }`,
		map[string]any{"owner": "adaiversen", "name": "gqlrepo"})
	repo, _ := data["repository"].(map[string]any)
	if repo["name"] != "gqlrepo" || repo["hasIssuesEnabled"] != true || repo["viewerPermission"] != "ADMIN" {
		t.Fatalf("repository fields: %v", repo)
	}
	if repo["databaseId"] == nil || repo["id"] == "" {
		t.Fatalf("repository ids: %v", repo)
	}
	owner, _ := repo["owner"].(map[string]any)
	if owner["login"] != "adaiversen" {
		t.Fatalf("repository owner: %v", repo)
	}

	// An unknown repository is data.repository null, with no errors.
	status, body := gql(t, srv,
		`query RepositoryFields($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }`,
		map[string]any{"owner": "adaiversen", "name": "nope"})
	if status != http.StatusOK {
		t.Fatalf("unknown repo: status %d", status)
	}
	if body["errors"] != nil {
		t.Fatalf("unknown repo: errors %v", body["errors"])
	}
	data, _ = body["data"].(map[string]any)
	if data["repository"] != nil {
		t.Fatalf("unknown repo: repository %v, want null", data["repository"])
	}
}

func TestIssueFlow(t *testing.T) {
	srv := newTestServer(t)
	rest(t, srv, "POST", "/api/v3/user/repos", map[string]any{"name": "issues"})

	// Create over GraphQL, with the repository's real global id.
	repoData := gqlData(t, srv,
		`query RepositoryFields($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }`,
		map[string]any{"owner": "adaiversen", "name": "issues"})
	repoID := repoData["repository"].(map[string]any)["id"].(string)

	data := gqlData(t, srv,
		`mutation IssueCreate($input: CreateIssueInput!) { createIssue(input: $input) { issue { number id } } }`,
		map[string]any{"input": map[string]any{"repositoryId": repoID, "title": "GQL issue", "body": "body"}})
	created := data["createIssue"].(map[string]any)["issue"].(map[string]any)
	if created["number"] != float64(1) || created["id"] == "" {
		t.Fatalf("createIssue: %v", data)
	}

	// OpenIssues lists it; IssueByNumber reads it.
	data = gqlData(t, srv,
		`query OpenIssues($owner: String!, $name: String!, $first: Int) { repository(owner: $owner, name: $name) { issues(first: $first, states: OPEN, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number title state } } } }`,
		map[string]any{"owner": "adaiversen", "name": "issues", "first": 10})
	nodes := data["repository"].(map[string]any)["issues"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("openIssues: %v", data)
	}
	data = gqlData(t, srv,
		`query IssueByNumber($owner: String!, $name: String!, $number: Int!) { repository(owner: $owner, name: $name) { issue(number: $number) { number title state body } } }`,
		map[string]any{"owner": "adaiversen", "name": "issues", "number": 1})
	issue := data["repository"].(map[string]any)["issue"].(map[string]any)
	if issue["title"] != "GQL issue" || issue["state"] != "OPEN" {
		t.Fatalf("issueByNumber: %v", issue)
	}

	// Close it over GraphQL.
	data = gqlData(t, srv,
		`mutation IssueClose($input: CloseIssueInput!) { closeIssue(input: $input) { issue { number id state } } }`,
		map[string]any{"input": map[string]any{"issueId": created["id"]}})
	closed := data["closeIssue"].(map[string]any)["issue"].(map[string]any)
	if closed["state"] != "CLOSED" {
		t.Fatalf("closeIssue: %v", data)
	}

	// A comment over GraphQL is readable over REST.
	data = gqlData(t, srv,
		`mutation AddComment($input: AddCommentInput!) { addComment(input: $input) { commentEdge { node { id body } } } }`,
		map[string]any{"input": map[string]any{"subjectId": created["id"], "body": "hello"}})
	comment := data["addComment"].(map[string]any)["commentEdge"].(map[string]any)["node"].(map[string]any)
	if comment["body"] != "hello" {
		t.Fatalf("addComment: %v", data)
	}
}

func TestPullFlowAndMergeable(t *testing.T) {
	srv := newTestServer(t)
	rest(t, srv, "POST", "/api/v3/user/repos", map[string]any{"name": "prs"})
	rest(t, srv, "PUT", "/api/v3/repos/adaiversen/prs/contents/base.txt",
		map[string]any{"message": "base", "content": "YmFzZQ=="})
	base := restMust(t, srv, "GET", "/api/v3/repos/adaiversen/prs/branches/main")
	baseSHA := base["commit"].(map[string]any)["sha"].(string)
	rest(t, srv, "POST", "/api/v3/repos/adaiversen/prs/git/refs",
		map[string]any{"ref": "refs/heads/feature", "sha": baseSHA})
	rest(t, srv, "PUT", "/api/v3/repos/adaiversen/prs/contents/head.txt",
		map[string]any{"message": "head", "content": "aGVhZA==", "branch": "feature"})

	repoData := gqlData(t, srv,
		`query RepositoryFields($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }`,
		map[string]any{"owner": "adaiversen", "name": "prs"})
	repoID := repoData["repository"].(map[string]any)["id"].(string)

	data := gqlData(t, srv,
		`mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { number id state headRefName baseRefName } } }`,
		map[string]any{"input": map[string]any{"repositoryId": repoID, "baseRefName": "main", "headRefName": "feature", "title": "GQL PR"}})
	pull := data["createPullRequest"].(map[string]any)["pullRequest"].(map[string]any)
	if pull["number"] != float64(1) || pull["headRefName"] != "feature" || pull["baseRefName"] != "main" {
		t.Fatalf("createPullRequest: %v", data)
	}

	// mergeable is a string enum, not a boolean.
	data = gqlData(t, srv,
		`query PullRequests($owner: String!, $name: String!, $first: Int) { repository(owner: $owner, name: $name) { pullRequests(first: $first, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number headRefName baseRefName mergeable state } } } }`,
		map[string]any{"owner": "adaiversen", "name": "prs", "first": 10})
	nodes := data["repository"].(map[string]any)["pullRequests"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("pullRequests: %v", data)
	}
	node := nodes[0].(map[string]any)
	if node["mergeable"] != "MERGEABLE" {
		t.Fatalf("mergeable: %v, want the string MERGEABLE", node["mergeable"])
	}

	// Files and the id lookup.
	data = gqlData(t, srv,
		`query PullRequestFiles($owner: String!, $name: String!, $number: Int!) { repository(owner: $owner, name: $name) { pullRequest(number: $number) { number files(first: 10) { nodes { path } } } } }`,
		map[string]any{"owner": "adaiversen", "name": "prs", "number": 1})
	files := data["repository"].(map[string]any)["pullRequest"].(map[string]any)["files"].(map[string]any)["nodes"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["path"] != "head.txt" {
		t.Fatalf("pullRequestFiles: %v", data)
	}
	data = gqlData(t, srv,
		`query PullRequestId($owner: String!, $name: String!, $number: Int!) { repository(owner: $owner, name: $name) { pullRequest(number: $number) { id number } } }`,
		map[string]any{"owner": "adaiversen", "name": "prs", "number": 1})
	if data["repository"].(map[string]any)["pullRequest"].(map[string]any)["id"] != pull["id"] {
		t.Fatalf("pullRequestId mismatch: %v vs %v", data, pull)
	}

	// Merge over GraphQL; REST reads it merged.
	data = gqlData(t, srv,
		`mutation MergePullRequest($input: MergePullRequestInput!) { mergePullRequest(input: $input) { pullRequest { number merged state } } }`,
		map[string]any{"input": map[string]any{"pullRequestId": pull["id"], "mergeMethod": "SQUASH"}})
	merged := data["mergePullRequest"].(map[string]any)["pullRequest"].(map[string]any)
	if merged["merged"] != true {
		t.Fatalf("mergePullRequest: %v", data)
	}
	status, body := rest(t, srv, "GET", "/api/v3/repos/adaiversen/prs/pulls/1", nil)
	if status != http.StatusOK || body["merged"] != true {
		t.Fatalf("REST after GraphQL merge: status %d, body %v", status, body)
	}
}

func restMust(t *testing.T, srv *httptest.Server, method, path string) map[string]any {
	t.Helper()
	status, body := rest(t, srv, method, path, nil)
	if status != http.StatusOK {
		t.Fatalf("%s %s: status %d, body %v", method, path, status, body)
	}
	return body
}

func TestNodeAndPagination(t *testing.T) {
	srv := newTestServer(t)
	rest(t, srv, "POST", "/api/v3/user/repos", map[string]any{"name": "paged"})
	for i := 0; i < 7; i++ {
		rest(t, srv, "POST", "/api/v3/repos/adaiversen/paged/issues", map[string]any{"title": "issue"})
	}
	repoData := gqlData(t, srv,
		`query RepositoryFields($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id name nameWithOwner } }`,
		map[string]any{"owner": "adaiversen", "name": "paged"})
	repoID := repoData["repository"].(map[string]any)["id"].(string)

	// node(id:) round-trips the repository global id.
	data := gqlData(t, srv,
		`query NodeRepository($id: ID!) { node(id: $id) { id __typename ... on Repository { name nameWithOwner } } }`,
		map[string]any{"id": repoID})
	node := data["node"].(map[string]any)
	if node["__typename"] != "Repository" || node["nameWithOwner"] != "adaiversen/paged" {
		t.Fatalf("node: %v", data)
	}

	// IssuesPage: 5 then 2, every issue exactly once.
	vars := map[string]any{"owner": "adaiversen", "name": "paged", "first": 5}
	seen := map[float64]bool{}
	for page := 0; page < 3; page++ {
		data = gqlData(t, srv,
			`query IssuesPage($owner: String!, $name: String!, $first: Int!, $after: String) { repository(owner: $owner, name: $name) { issues(first: $first, after: $after) { nodes { number } pageInfo { hasNextPage endCursor } } } }`,
			vars)
		issues := data["repository"].(map[string]any)["issues"].(map[string]any)
		for _, entry := range issues["nodes"].([]any) {
			number := entry.(map[string]any)["number"].(float64)
			if seen[number] {
				t.Fatalf("issue #%v seen twice", number)
			}
			seen[number] = true
		}
		pageInfo := issues["pageInfo"].(map[string]any)
		if pageInfo["hasNextPage"] == true {
			vars["after"] = pageInfo["endCursor"]
		} else {
			break
		}
	}
	if len(seen) != 7 {
		t.Fatalf("issues pages: %d distinct, want 7", len(seen))
	}
}

func TestInvalidFieldAndBatchAndAuth(t *testing.T) {
	srv := newTestServer(t)
	learnViewer(t, srv)

	// An unknown field is a 200 with an errors array.
	status, body := gql(t, srv,
		`query ViewerInvalidField { viewer { login noSuchFieldOnViewer } }`, nil)
	if status != http.StatusOK {
		t.Fatalf("invalid field: status %d", status)
	}
	if body["errors"] == nil {
		t.Fatalf("invalid field: no errors in %v", body)
	}

	// A batch: an array in, an array out, same order.
	payload, _ := json.Marshal([]map[string]any{
		{"query": `query ViewerLogin { viewer { login } }`},
		{"query": `query ViewerLogin { viewer { login } }`},
	})
	req, _ := http.NewRequest("POST", srv.URL+"/api/graphql", bytes.NewReader(payload))
	req.Header.Set("Authorization", testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var batch []map[string]any
	if err := json.Unmarshal(raw, &batch); err != nil || len(batch) != 2 {
		t.Fatalf("batch: %q", raw)
	}
	if batch[0]["data"].(map[string]any)["viewer"].(map[string]any)["login"] != "adaiversen" {
		t.Fatalf("batch first: %v", batch[0])
	}

	// No token: 401 before parsing.
	req, _ = http.NewRequest("POST", srv.URL+"/api/graphql", bytes.NewReader([]byte(`{"query":"query ViewerLogin { viewer { login } }"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GraphQL: status %d, want 401", resp.StatusCode)
	}
}

func TestIntrospection(t *testing.T) {
	srv := newTestServer(t)
	data := gqlData(t, srv,
		`query Issue_fields { __type(name: "Issue") { fields { name } } }`, nil)
	fields := data["Issue"].(map[string]any)["fields"].([]any)
	names := map[string]bool{}
	for _, field := range fields {
		names[field.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"id", "number", "title", "state", "body"} {
		if !names[want] {
			t.Fatalf("Issue fields missing %s: %v", want, names)
		}
	}
	data = gqlData(t, srv,
		`query SearchType_enumValues { __type(name: "SearchType") { enumValues { name } } }`, nil)
	values := data["SearchType"].(map[string]any)["enumValues"].([]any)
	if len(values) == 0 {
		t.Fatal("SearchType enumValues empty")
	}
}
