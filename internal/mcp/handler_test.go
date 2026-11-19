package mcp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// rpc sends one JSON-RPC request and returns the HTTP status and the
// decoded envelope.
func rpc(t *testing.T, srv *httptest.Server, method string, params map[string]any, id any, withAuth bool) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.Header.Set("Authorization", testToken)
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
	var envelope map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("MCP response not JSON: %q", raw)
		}
	}
	return resp.StatusCode, envelope
}

func result(t *testing.T, srv *httptest.Server, method string, params map[string]any, id any) map[string]any {
	t.Helper()
	status, envelope := rpc(t, srv, method, params, id, true)
	if status != http.StatusOK {
		t.Fatalf("MCP %s: HTTP %d, envelope %v", method, status, envelope)
	}
	if envelope["error"] != nil {
		t.Fatalf("MCP %s: error %v", method, envelope["error"])
	}
	res, _ := envelope["result"].(map[string]any)
	if res == nil {
		t.Fatalf("MCP %s: no result in %v", method, envelope)
	}
	return res
}

func callTool(t *testing.T, srv *httptest.Server, name string, args map[string]any) map[string]any {
	t.Helper()
	res := result(t, srv, "tools/call", map[string]any{"name": name, "arguments": args}, 1)
	if res["isError"] == true {
		t.Fatalf("tools/call %s: isError, %v", name, res["content"])
	}
	structured, _ := res["structuredContent"].(map[string]any)
	if structured == nil {
		t.Fatalf("tools/call %s: no structuredContent in %v", name, res)
	}
	return structured
}

func TestDiscoverAndInitialize(t *testing.T) {
	srv := newTestServer(t)

	res := result(t, srv, "server/discover", map[string]any{}, "discover-1")
	versions, _ := res["supportedVersions"].([]any)
	found := false
	for _, version := range versions {
		if version == "2026-07-28" {
			found = true
		}
	}
	if !found {
		t.Fatalf("server/discover supportedVersions: %v", res)
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] == "" || info["version"] == "" {
		t.Fatalf("server/discover serverInfo: %v", res)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("server/discover capabilities.tools missing: %v", res)
	}

	res = result(t, srv, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1.0.0"},
	}, "initialize-1")
	if res["protocolVersion"] != "2025-11-25" {
		t.Fatalf("initialize protocolVersion: %v", res)
	}
	info, _ = res["serverInfo"].(map[string]any)
	if info["name"] == "" || info["version"] == "" {
		t.Fatalf("initialize serverInfo: %v", res)
	}
}

func TestToolsListDeterministic(t *testing.T) {
	srv := newTestServer(t)
	first := result(t, srv, "tools/list", map[string]any{}, 1)
	second := result(t, srv, "tools/list", map[string]any{}, 2)
	tools, _ := first["tools"].([]any)
	if len(tools) < 6 {
		t.Fatalf("tools/list: %d tools, want at least 6", len(tools))
	}
	names := map[string]bool{}
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		names[tool["name"].(string)] = true
		description, _ := tool["description"].(string)
		if description == "" {
			t.Fatalf("tool %v: empty description", tool["name"])
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Fatalf("tool %v: inputSchema.type %v, want object", tool["name"], schema["type"])
		}
	}
	for _, want := range []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"} {
		if !names[want] {
			t.Fatalf("tools/list missing %s: %v", want, names)
		}
	}
	firstJSON, _ := json.Marshal(first["tools"])
	secondJSON, _ := json.Marshal(second["tools"])
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("tools/list differs between calls:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestToolFlowRestParity(t *testing.T) {
	srv := newTestServer(t)

	created := callTool(t, srv, "create_repo", map[string]any{"name": "mcp-repo", "description": "via mcp", "private": true})
	if created["full_name"] != "adaiversen/mcp-repo" || created["name"] != "mcp-repo" {
		t.Fatalf("create_repo: %v", created)
	}

	got := callTool(t, srv, "get_repo", map[string]any{"owner": "adaiversen", "repo": "mcp-repo"})
	if got["name"] != "mcp-repo" {
		t.Fatalf("get_repo: %v", got)
	}

	issue := callTool(t, srv, "create_issue", map[string]any{"owner": "adaiversen", "repo": "mcp-repo", "title": "MCP issue", "body": "hello"})
	if issue["number"] != float64(1) {
		t.Fatalf("create_issue: %v", issue)
	}

	listed := callTool(t, srv, "list_issues", map[string]any{"owner": "adaiversen", "repo": "mcp-repo", "state": "open"})
	issues, _ := listed["issues"].([]any)
	if len(issues) != 1 || issues[0].(map[string]any)["title"] != "MCP issue" {
		t.Fatalf("list_issues: %v", listed)
	}

	// The same objects over REST, with stable numeric ids.
	status, body := restGet(t, srv, "/api/v3/repos/adaiversen/mcp-repo")
	if status != http.StatusOK || body["id"] == nil {
		t.Fatalf("REST repo after MCP create: status %d, body %v", status, body)
	}
	status, body = restGet(t, srv, "/api/v3/repos/adaiversen/mcp-repo/issues/1")
	if status != http.StatusOK || body["title"] != "MCP issue" || body["id"] == nil {
		t.Fatalf("REST issue after MCP create: status %d, body %v", status, body)
	}
}

func restGet(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body
}

func TestPullRequestFlow(t *testing.T) {
	srv := newTestServer(t)
	callTool(t, srv, "create_repo", map[string]any{"name": "mcp-pr"})

	// A base commit on main and a head branch, over REST.
	restPut(t, srv, "/api/v3/repos/adaiversen/mcp-pr/contents/base.txt",
		map[string]any{"message": "base", "content": "YmFzZQ=="})
	_, base := restGet(t, srv, "/api/v3/repos/adaiversen/mcp-pr/branches/main")
	baseSHA := base["commit"].(map[string]any)["sha"].(string)
	restPost(t, srv, "/api/v3/repos/adaiversen/mcp-pr/git/refs",
		map[string]any{"ref": "refs/heads/feature", "sha": baseSHA})
	restPut(t, srv, "/api/v3/repos/adaiversen/mcp-pr/contents/head.txt",
		map[string]any{"message": "head", "content": "aGVhZA==", "branch": "feature"})

	pull := callTool(t, srv, "create_pull_request", map[string]any{
		"owner": "adaiversen", "repo": "mcp-pr", "title": "MCP PR", "body": "body",
		"head": "feature", "base": "main",
	})
	if pull["number"] != float64(1) {
		t.Fatalf("create_pull_request: %v", pull)
	}
	merged := callTool(t, srv, "merge_pull_request", map[string]any{
		"owner": "adaiversen", "repo": "mcp-pr", "pull_number": 1,
	})
	if merged["merged"] != true {
		t.Fatalf("merge_pull_request: %v", merged)
	}
	status, body := restGet(t, srv, "/api/v3/repos/adaiversen/mcp-pr/pulls/1")
	if status != http.StatusOK || body["merged"] != true {
		t.Fatalf("REST pull after MCP merge: status %d, body %v", status, body)
	}
}

func restPost(t *testing.T, srv *httptest.Server, path string, body any) (int, map[string]any) {
	return restWrite(t, srv, "POST", path, body)
}

func restPut(t *testing.T, srv *httptest.Server, path string, body any) (int, map[string]any) {
	return restWrite(t, srv, "PUT", path, body)
}

func restWrite(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(raw))
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
	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	if resp.StatusCode >= 400 {
		t.Fatalf("%s %s: status %d, body %v", method, path, resp.StatusCode, decoded)
	}
	return resp.StatusCode, decoded
}

func TestErrorsAndConcurrency(t *testing.T) {
	srv := newTestServer(t)

	// Unknown tool: -32601.
	_, envelope := rpc(t, srv, "tools/call", map[string]any{"name": "mcp_no_such_tool", "arguments": map[string]any{}}, 10, true)
	errObj, _ := envelope["error"].(map[string]any)
	if errObj["code"] != float64(-32601) {
		t.Fatalf("unknown tool: %v", envelope)
	}

	// Missing required arguments: -32602.
	_, envelope = rpc(t, srv, "tools/call", map[string]any{"name": "create_issue", "arguments": map[string]any{}}, 11, true)
	errObj, _ = envelope["error"].(map[string]any)
	if errObj["code"] != float64(-32602) {
		t.Fatalf("missing arguments: %v", envelope)
	}

	// A body that is not JSON: HTTP 400 with -32700.
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc": "2.0", "id": 424242, "method": "tools/list"`))
	req.Header.Set("Authorization", testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var parseErr map[string]any
	_ = json.Unmarshal(raw, &parseErr)
	errObj, _ = parseErr["error"].(map[string]any)
	if resp.StatusCode != http.StatusBadRequest || errObj["code"] != float64(-32700) {
		t.Fatalf("parse error: HTTP %d, body %q", resp.StatusCode, raw)
	}

	// No Authorization: HTTP 401.
	status, _ := rpc(t, srv, "tools/call", map[string]any{"name": "create_issue", "arguments": map[string]any{}}, 12, false)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tools/call: HTTP %d, want 401", status)
	}

	// Two concurrent calls keep their own ids and their own objects.
	callTool(t, srv, "create_repo", map[string]any{"name": "concurrent-left"})
	callTool(t, srv, "create_repo", map[string]any{"name": "concurrent-right"})
	var wg sync.WaitGroup
	results := make([]map[string]any, 2)
	for i, repo := range []string{"concurrent-left", "concurrent-right"} {
		wg.Add(1)
		go func(index int, name string) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%s", name)
			_, envelope := rpc(t, srv, "tools/call",
				map[string]any{"name": "get_repo", "arguments": map[string]any{"owner": "adaiversen", "repo": name}},
				id, true)
			results[index] = envelope
		}(i, repo)
	}
	wg.Wait()
	for i, repo := range []string{"concurrent-left", "concurrent-right"} {
		envelope := results[i]
		if envelope["id"] != fmt.Sprintf("concurrent-%s", repo) {
			t.Fatalf("concurrent call %d: id %v", i, envelope["id"])
		}
		res, _ := envelope["result"].(map[string]any)
		text, _ := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, repo) {
			t.Fatalf("concurrent call %d: result does not name %s: %s", i, repo, text)
		}
	}
}
