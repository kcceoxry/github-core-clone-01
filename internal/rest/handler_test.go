package rest_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func do(t *testing.T, srv *httptest.Server, method, path string, body any) (int, http.Header, map[string]any) {
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			// A list answer decodes into a slice, not a map.
			var list []any
			if err2 := json.Unmarshal(raw, &list); err2 == nil {
				decoded = map[string]any{"_list": list}
			} else {
				t.Fatalf("%s %s: unparsable body %q", method, path, raw)
			}
		}
	}
	return resp.StatusCode, resp.Header, decoded
}

func doList(t *testing.T, srv *httptest.Server, method, path string, body any) (int, http.Header, []any) {
	t.Helper()
	status, header, decoded := do(t, srv, method, path, body)
	list, ok := decoded["_list"].([]any)
	if !ok {
		t.Fatalf("%s %s: expected a JSON array, got %v", method, path, decoded)
	}
	return status, header, list
}

func createRepo(t *testing.T, srv *httptest.Server, name string) map[string]any {
	t.Helper()
	status, _, body := do(t, srv, "POST", "/api/v3/user/repos", map[string]any{"name": name, "private": false})
	if status != http.StatusCreated {
		t.Fatalf("create repo %s: status %d, body %v", name, status, body)
	}
	return body
}

func TestRootAndAuth(t *testing.T) {
	srv := newTestServer(t)

	status, header, _ := do(t, srv, "GET", "/api/v3/", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v3/: status %d", status)
	}
	if header.Get("X-OAuth-Scopes") == "" {
		t.Fatal("missing X-OAuth-Scopes")
	}
	if header.Get("X-GitHub-Request-Id") == "" {
		t.Fatal("missing X-GitHub-Request-Id")
	}

	// A repository read without a token is a 401.
	createRepo(t, srv, "authcheck")
	req, _ := http.NewRequest("GET", srv.URL+"/api/v3/repos/adaiversen/authcheck", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET repo: status %d, want 401", resp.StatusCode)
	}
}

func TestRepoLifecycle(t *testing.T) {
	srv := newTestServer(t)
	created := createRepo(t, srv, "hello")
	if created["name"] != "hello" || created["default_branch"] != "main" {
		t.Fatalf("create repo shape: %v", created)
	}

	status, _, body := do(t, srv, "GET", "/api/v3/repos/adaiversen/hello", nil)
	if status != http.StatusOK || body["name"] != "hello" {
		t.Fatalf("GET repo: status %d, body %v", status, body)
	}

	status, _, body = do(t, srv, "PATCH", "/api/v3/repos/adaiversen/hello", map[string]any{"description": "patched"})
	if status != http.StatusOK || body["description"] != "patched" {
		t.Fatalf("PATCH repo: status %d, body %v", status, body)
	}

	status, _, _ = do(t, srv, "DELETE", "/api/v3/repos/adaiversen/hello", nil)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE repo: status %d, want 204", status)
	}
	status, _, _ = do(t, srv, "GET", "/api/v3/repos/adaiversen/hello", nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET deleted repo: status %d, want 404", status)
	}

	// A repository under another owner is a 404.
	status, _, _ = do(t, srv, "GET", "/api/v3/repos/someone-else/hello", nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET foreign repo: status %d, want 404", status)
	}
}

func TestContentsRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	createRepo(t, srv, "files")

	content := base64.StdEncoding.EncodeToString([]byte("hello world\n"))
	status, _, body := do(t, srv, "PUT", "/api/v3/repos/adaiversen/files/contents/hello.txt",
		map[string]any{"message": "add hello", "content": content})
	if status != http.StatusCreated {
		t.Fatalf("PUT contents: status %d, body %v", status, body)
	}
	contentObj, _ := body["content"].(map[string]any)
	commitObj, _ := body["commit"].(map[string]any)
	if contentObj["sha"] == "" || contentObj["encoding"] != "base64" {
		t.Fatalf("PUT contents content shape: %v", body)
	}
	if commitObj["sha"] == "" {
		t.Fatalf("PUT contents commit shape: %v", body)
	}
	blobSHA := contentObj["sha"].(string)

	// Read the file back: base64 round-trips.
	status, _, body = do(t, srv, "GET", "/api/v3/repos/adaiversen/files/contents/hello.txt", nil)
	if status != http.StatusOK {
		t.Fatalf("GET contents: status %d", status)
	}
	decoded, err := base64.StdEncoding.DecodeString(body["content"].(string))
	if err != nil || string(decoded) != "hello world\n" {
		t.Fatalf("GET contents body: %v, err %v", body, err)
	}

	// The directory view lists the file.
	_, _, list := doList(t, srv, "GET", "/api/v3/repos/adaiversen/files/contents/", nil)
	if len(list) != 1 {
		t.Fatalf("GET directory: %d entries, want 1", len(list))
	}

	// Update with the stale sha: 409.
	status, _, _ = do(t, srv, "PUT", "/api/v3/repos/adaiversen/files/contents/hello.txt",
		map[string]any{"message": "stale", "content": content, "sha": "0000000000000000000000000000000000000000"})
	if status != http.StatusConflict {
		t.Fatalf("PUT stale sha: status %d, want 409", status)
	}

	// Update with the right sha: 200, then delete with the new sha: 200
	// and GET 404.
	status, _, body = do(t, srv, "PUT", "/api/v3/repos/adaiversen/files/contents/hello.txt",
		map[string]any{"message": "update", "content": base64.StdEncoding.EncodeToString([]byte("v2\n")), "sha": blobSHA})
	if status != http.StatusOK {
		t.Fatalf("PUT update: status %d, want 200", status)
	}
	newSHA := body["content"].(map[string]any)["sha"].(string)
	status, _, _ = do(t, srv, "DELETE", "/api/v3/repos/adaiversen/files/contents/hello.txt",
		map[string]any{"message": "remove", "sha": newSHA})
	if status != http.StatusOK {
		t.Fatalf("DELETE contents: status %d, want 200", status)
	}
	status, _, _ = do(t, srv, "GET", "/api/v3/repos/adaiversen/files/contents/hello.txt", nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET deleted file: status %d, want 404", status)
	}
}

func TestIssuesAndComments(t *testing.T) {
	srv := newTestServer(t)
	createRepo(t, srv, "tracker")

	status, _, body := do(t, srv, "POST", "/api/v3/repos/adaiversen/tracker/labels",
		map[string]any{"name": "bug", "color": "ff0000"})
	if status != http.StatusCreated {
		t.Fatalf("POST label: status %d, body %v", status, body)
	}

	status, _, body = do(t, srv, "POST", "/api/v3/repos/adaiversen/tracker/issues",
		map[string]any{"title": "First bug", "body": "it broke", "labels": []string{"bug"}})
	if status != http.StatusCreated {
		t.Fatalf("POST issue: status %d, body %v", status, body)
	}
	if body["number"] != float64(1) || body["state"] != "open" {
		t.Fatalf("issue shape: %v", body)
	}
	labels, _ := body["labels"].([]any)
	if len(labels) != 1 {
		t.Fatalf("issue labels: %v", body["labels"])
	}

	// An unknown label is a 422.
	status, _, _ = do(t, srv, "POST", "/api/v3/repos/adaiversen/tracker/issues",
		map[string]any{"title": "Bad label", "labels": []string{"nope"}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("POST issue with unknown label: status %d, want 422", status)
	}

	// Second issue, then list sorted descending.
	do(t, srv, "POST", "/api/v3/repos/adaiversen/tracker/issues", map[string]any{"title": "Second"})
	_, _, list := doList(t, srv, "GET", "/api/v3/repos/adaiversen/tracker/issues?sort=created&direction=desc", nil)
	if len(list) != 2 {
		t.Fatalf("list issues: %d, want 2", len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["number"] != float64(2) {
		t.Fatalf("list issues desc: first number %v, want 2", first["number"])
	}

	// Comments.
	status, _, body = do(t, srv, "POST", "/api/v3/repos/adaiversen/tracker/issues/1/comments",
		map[string]any{"body": "noted"})
	if status != http.StatusCreated || body["body"] != "noted" {
		t.Fatalf("POST comment: status %d, body %v", status, body)
	}
	_, _, list = doList(t, srv, "GET", "/api/v3/repos/adaiversen/tracker/issues/1/comments", nil)
	if len(list) != 1 {
		t.Fatalf("list comments: %d, want 1", len(list))
	}

	// Close the issue and check it stays closed.
	status, _, body = do(t, srv, "PATCH", "/api/v3/repos/adaiversen/tracker/issues/1",
		map[string]any{"state": "closed"})
	if status != http.StatusOK || body["state"] != "closed" {
		t.Fatalf("PATCH issue close: status %d, body %v", status, body)
	}
	_, _, list = doList(t, srv, "GET", "/api/v3/repos/adaiversen/tracker/issues?state=open", nil)
	if len(list) != 1 {
		t.Fatalf("list open issues after close: %d, want 1", len(list))
	}
}

func TestPullsAndMerge(t *testing.T) {
	srv := newTestServer(t)
	createRepo(t, srv, "prrepo")

	// A base commit on main.
	status, _, base := do(t, srv, "PUT", "/api/v3/repos/adaiversen/prrepo/contents/base.txt",
		map[string]any{"message": "base", "content": base64.StdEncoding.EncodeToString([]byte("base\n"))})
	if status != http.StatusCreated {
		t.Fatalf("PUT base: status %d, body %v", status, base)
	}
	baseSHA := base["commit"].(map[string]any)["sha"].(string)

	// The head branch, from the base commit.
	status, _, _ = do(t, srv, "POST", "/api/v3/repos/adaiversen/prrepo/git/refs",
		map[string]any{"ref": "refs/heads/feature", "sha": baseSHA})
	if status != http.StatusCreated {
		t.Fatalf("POST git/refs: status %d", status)
	}

	// A commit on the head branch.
	status, _, _ = do(t, srv, "PUT", "/api/v3/repos/adaiversen/prrepo/contents/head.txt",
		map[string]any{"message": "head", "content": base64.StdEncoding.EncodeToString([]byte("head\n")), "branch": "feature"})
	if status != http.StatusCreated {
		t.Fatalf("PUT head: status %d", status)
	}

	// Open the pull request.
	status, _, body := do(t, srv, "POST", "/api/v3/repos/adaiversen/prrepo/pulls",
		map[string]any{"title": "Add head", "head": "feature", "base": "main"})
	if status != http.StatusCreated {
		t.Fatalf("POST pull: status %d, body %v", status, body)
	}
	if body["number"] != float64(1) {
		t.Fatalf("pull shape: %v", body)
	}
	headObj, _ := body["head"].(map[string]any)
	if headObj["ref"] != "feature" || headObj["sha"] == "" {
		t.Fatalf("pull head shape: %v", body)
	}

	// Files list.
	_, _, list := doList(t, srv, "GET", "/api/v3/repos/adaiversen/prrepo/pulls/1/files", nil)
	if len(list) != 1 {
		t.Fatalf("pull files: %d, want 1", len(list))
	}
	file, _ := list[0].(map[string]any)
	if file["filename"] != "head.txt" {
		t.Fatalf("pull file shape: %v", file)
	}

	// Merge: 200, merged:true, merge_commit_sha == sha.
	status, _, body = do(t, srv, "PUT", "/api/v3/repos/adaiversen/prrepo/pulls/1/merge",
		map[string]any{"merge_method": "squash"})
	if status != http.StatusOK || body["merged"] != true || body["sha"] == "" {
		t.Fatalf("PUT merge: status %d, body %v", status, body)
	}
	if body["merge_commit_sha"] != body["sha"] {
		t.Fatalf("merge_commit_sha %v != sha %v", body["merge_commit_sha"], body["sha"])
	}

	// The merged pull reads closed with a merged_at.
	status, _, body = do(t, srv, "GET", "/api/v3/repos/adaiversen/prrepo/pulls/1", nil)
	if status != http.StatusOK || body["state"] != "closed" || body["merged"] != true || body["merged_at"] == "" {
		t.Fatalf("GET merged pull: status %d, body %v", status, body)
	}

	// The merged content is visible on the base branch.
	status, _, body = do(t, srv, "GET", "/api/v3/repos/adaiversen/prrepo/contents/head.txt?ref=main", nil)
	if status != http.StatusOK {
		t.Fatalf("GET merged file on main: status %d", status)
	}

	// Merging an unknown pull is a 404.
	status, _, _ = do(t, srv, "PUT", "/api/v3/repos/adaiversen/prrepo/pulls/99/merge", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("PUT merge unknown: status %d, want 404", status)
	}

	// Two branches change the same file differently; merging the first
	// succeeds and the second then conflicts: 405.
	status, _, _ = do(t, srv, "POST", "/api/v3/repos/adaiversen/prrepo/git/refs",
		map[string]any{"ref": "refs/heads/conflict-a", "sha": baseSHA})
	if status != http.StatusCreated {
		t.Fatalf("POST git/refs conflict-a: status %d", status)
	}
	status, _, _ = do(t, srv, "POST", "/api/v3/repos/adaiversen/prrepo/git/refs",
		map[string]any{"ref": "refs/heads/conflict-b", "sha": baseSHA})
	if status != http.StatusCreated {
		t.Fatalf("POST git/refs conflict-b: status %d", status)
	}
	// Both branches update base.txt with different content.
	status, _, baseFile := do(t, srv, "GET", "/api/v3/repos/adaiversen/prrepo/contents/base.txt", nil)
	if status != http.StatusOK {
		t.Fatalf("GET base file: status %d", status)
	}
	baseBlobSHA := baseFile["sha"].(string)
	status, _, _ = do(t, srv, "PUT", "/api/v3/repos/adaiversen/prrepo/contents/base.txt",
		map[string]any{"message": "a", "content": base64.StdEncoding.EncodeToString([]byte("a\n")), "sha": baseBlobSHA, "branch": "conflict-a"})
	if status != http.StatusOK {
		t.Fatalf("PUT conflict-a: status %d", status)
	}
	status, _, _ = do(t, srv, "PUT", "/api/v3/repos/adaiversen/prrepo/contents/base.txt",
		map[string]any{"message": "b", "content": base64.StdEncoding.EncodeToString([]byte("b\n")), "sha": baseBlobSHA, "branch": "conflict-b"})
	if status != http.StatusOK {
		t.Fatalf("PUT conflict-b: status %d", status)
	}
	status, _, _ = do(t, srv, "POST", "/api/v3/repos/adaiversen/prrepo/pulls",
		map[string]any{"title": "A", "head": "conflict-a", "base": "main"})
	if status != http.StatusCreated {
		t.Fatalf("POST pull A: status %d", status)
	}
	status, _, _ = do(t, srv, "POST", "/api/v3/repos/adaiversen/prrepo/pulls",
		map[string]any{"title": "B", "head": "conflict-b", "base": "main"})
	if status != http.StatusCreated {
		t.Fatalf("POST pull B: status %d", status)
	}
	status, _, _ = do(t, srv, "PUT", "/api/v3/repos/adaiversen/prrepo/pulls/2/merge", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("PUT merge A: status %d, want 200", status)
	}
	status, _, _ = do(t, srv, "PUT", "/api/v3/repos/adaiversen/prrepo/pulls/3/merge", map[string]any{})
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("PUT merge conflict: status %d, want 405", status)
	}
}

func TestUserReposPagination(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < 30; i++ {
		createRepo(t, srv, fmt.Sprintf("page-%02d", i))
	}
	seen := map[string]bool{}
	url := "/api/v3/user/repos?per_page=10"
	pages := 0
	for url != "" {
		req, _ := http.NewRequest("GET", srv.URL+url, nil)
		req.Header.Set("Authorization", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var list []map[string]any
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(list) > 10 {
			t.Fatalf("page %d: %d items, more than per_page=10", pages, len(list))
		}
		for _, repo := range list {
			seen[repo["name"].(string)] = true
		}
		pages++
		url = ""
		for _, part := range strings.Split(resp.Header.Get("Link"), ",") {
			part = strings.TrimSpace(part)
			if strings.HasSuffix(part, `rel="next"`) {
				start := strings.Index(part, "<")
				end := strings.Index(part, ">")
				next := part[start+1 : end]
				if !strings.HasPrefix(next, srv.URL) {
					t.Fatalf("Link next is not same-origin: %s", next)
				}
				url = strings.TrimPrefix(next, srv.URL)
			}
		}
		if pages > 10 {
			t.Fatal("pagination looped")
		}
	}
	if pages < 3 {
		t.Fatalf("30 repos in %d pages, want at least 3", pages)
	}
	if len(seen) != 30 {
		t.Fatalf("paginated repos: %d distinct, want 30", len(seen))
	}
}

func TestErrors(t *testing.T) {
	srv := newTestServer(t)
	createRepo(t, srv, "errs")

	// Malformed JSON: 400.
	req, _ := http.NewRequest("POST", srv.URL+"/api/v3/repos/adaiversen/errs/issues", strings.NewReader("{not json"))
	req.Header.Set("Authorization", testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status %d, want 400", resp.StatusCode)
	}

	// Unknown route: 404 with a non-empty message.
	status, _, body := do(t, srv, "GET", "/api/v3/nope", nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown route: status %d, want 404", status)
	}
	if body["message"] == "" {
		t.Fatalf("unknown route: empty message: %v", body)
	}
}
