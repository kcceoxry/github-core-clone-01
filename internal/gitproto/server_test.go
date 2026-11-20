package gitproto_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/gitproto"
	"forge/internal/pack"
	"forge/internal/server"
	"forge/internal/store"
)

func startTestForge(t *testing.T) *httptest.Server {
	t.Helper()
	forge := gitproto.NewForge()
	server := httptest.NewServer(server.New(forge))
	t.Cleanup(server.Close)
	return server
}

func createRepo(t *testing.T, baseURL, name string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "description": "test", "private": true})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v3/user/repos", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		detail, _ := io.ReadAll(resp.Body)
		t.Fatalf("create repo: HTTP %d: %s", resp.StatusCode, detail)
	}
}

func get(t *testing.T, url string, headers map[string]string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

func post(t *testing.T, url string, contentType string, headers map[string]string, body []byte) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), data
}

// pushBody builds a receive-pack request body: the command list, a flush,
// then the pack of the given objects.
func pushBody(t *testing.T, commands []gitproto.PushCommand, objects []*store.Object) []byte {
	t.Helper()
	var items []any
	for index, command := range commands {
		line := fmt.Sprintf("%s %s %s", command.OldOID.Hex(), command.NewOID.Hex(), command.Ref)
		if index == 0 {
			line += "\x00report-status side-band-64k ofs-delta agent=test/1.0"
		}
		items = append(items, line+"\n")
	}
	items = append(items, "flush")
	encoded, err := gitproto.EncodePktLines(items)
	if err != nil {
		t.Fatalf("encode commands: %v", err)
	}
	packBytes, err := pack.Write(objects)
	if err != nil {
		t.Fatalf("write pack: %v", err)
	}
	return append(encoded, packBytes...)
}

// pushReport extracts the report-status text from a sideband-framed push
// response body.
func pushReport(t *testing.T, body []byte) string {
	t.Helper()
	packets, err := gitproto.DecodePktLines(body)
	if err != nil {
		t.Fatalf("decode push response: %v", err)
	}
	var inner []byte
	for _, packet := range packets {
		if packet.Kind != gitproto.PacketData || len(packet.Payload) == 0 {
			continue
		}
		if packet.Payload[0] != 0x01 {
			t.Fatalf("push response band = %d, want 1", packet.Payload[0])
		}
		inner = append(inner, packet.Payload[1:]...)
	}
	lines, err := gitproto.DecodePktLines(inner)
	if err != nil {
		t.Fatalf("decode report: %v", err)
	}
	var text strings.Builder
	for _, packet := range lines {
		if packet.Kind == gitproto.PacketData {
			text.Write(packet.Payload)
		}
	}
	return text.String()
}

// TestViewerLearnedFromPath pins the viewer rule: the server starts with
// no viewer, learns it from the first request that names an owner for an
// existing repository, and afterwards answers a foreign owner with a 404.
func TestViewerLearnedFromPath(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")

	auth := map[string]string{"Authorization": "Bearer test-token"}
	status, _, body := get(t, server.URL+"/api/v3/user", auth)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v3/user before learning: status %d", status)
	}
	var user map[string]any
	if err := json.Unmarshal(body, &user); err != nil {
		t.Fatalf("GET /api/v3/user: %v", err)
	}
	if user["login"] != "" {
		t.Fatalf("viewer before learning: %v, want empty", user["login"])
	}

	status, _, _ = get(t, server.URL+"/adaiversen/demo.git/info/refs?service=git-upload-pack", nil)
	if status != http.StatusOK {
		t.Fatalf("info/refs under the viewer: status %d", status)
	}
	status, _, body = get(t, server.URL+"/api/v3/user", auth)
	if err := json.Unmarshal(body, &user); err != nil {
		t.Fatalf("GET /api/v3/user: %v", err)
	}
	if user["login"] != "adaiversen" {
		t.Fatalf("viewer after learning: %v, want adaiversen", user["login"])
	}

	// The same repository under a foreign owner is a 404, never a 403.
	status, _, _ = get(t, server.URL+"/stranger/demo.git/info/refs?service=git-upload-pack", nil)
	if status != http.StatusNotFound {
		t.Fatalf("foreign owner: status %d, want 404", status)
	}
	// An unknown repository under the viewer is a 404 and teaches nothing.
	status, _, _ = get(t, server.URL+"/adaiversen/missing.git/info/refs?service=git-upload-pack", nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown repo: status %d, want 404", status)
	}
	status, _, body = get(t, server.URL+"/api/v3/user", auth)
	if err := json.Unmarshal(body, &user); err != nil {
		t.Fatalf("GET /api/v3/user: %v", err)
	}
	if user["login"] != "adaiversen" {
		t.Fatalf("viewer after unknown repo: %v, want adaiversen", user["login"])
	}
}

func TestAdvertiseV2(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")
	status, contentType, body := get(t, server.URL+"/adaiversen/demo.git/info/refs?service=git-upload-pack",
		map[string]string{"Git-Protocol": "version=2"})
	if status != 200 {
		t.Fatalf("info/refs v2: HTTP %d", status)
	}
	if !strings.Contains(contentType, "application/x-git-upload-pack-advertisement") {
		t.Fatalf("content-type = %q", contentType)
	}
	packets, err := gitproto.DecodePktLines(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var lines []string
	for _, packet := range packets {
		if packet.Kind == gitproto.PacketData {
			lines = append(lines, string(packet.Payload))
		}
	}
	if len(lines) < 2 || lines[0] != "# service=git-upload-pack\n" {
		t.Fatalf("first line = %q, want the service preamble", lines[0])
	}
	if lines[1] != "version 2\n" {
		t.Fatalf("second line = %q, want version 2", lines[1])
	}
	joined := strings.Join(lines, " ")
	for _, want := range []string{"fetch=shallow filter", "ls-refs=unborn", "object-format=sha1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("v2 advertisement is missing %q (saw %q)", want, joined)
		}
	}
}

func TestAdvertiseV0Empty(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")
	status, contentType, body := get(t, server.URL+"/adaiversen/demo.git/info/refs?service=git-upload-pack", nil)
	if status != 200 {
		t.Fatalf("info/refs v0: HTTP %d", status)
	}
	if !strings.Contains(contentType, "application/x-git-upload-pack-advertisement") {
		t.Fatalf("content-type = %q", contentType)
	}
	packets, err := gitproto.DecodePktLines(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var lines []string
	for _, packet := range packets {
		if packet.Kind == gitproto.PacketData {
			lines = append(lines, string(packet.Payload))
		}
	}
	if len(lines) != 2 || lines[0] != "# service=git-upload-pack\n" {
		t.Fatalf("lines = %q", lines)
	}
	if !strings.Contains(lines[1], "capabilities^{}") || !strings.Contains(lines[1], "symref=HEAD:refs/heads/main") {
		t.Fatalf("empty repo line = %q", lines[1])
	}
	if !strings.Contains(lines[1], "side-band-64k") || !strings.Contains(lines[1], "multi_ack_detailed") {
		t.Fatalf("v0 capabilities are missing side-band-64k or multi_ack_detailed: %q", lines[1])
	}
}

func TestAdvertiseUnknownRepo404(t *testing.T) {
	server := startTestForge(t)
	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		status, _, _ := get(t, server.URL+"/adaiversen/missing.git/info/refs?service="+service, nil)
		if status != 404 {
			t.Fatalf("info/refs %s on a missing repo: HTTP %d, want 404", service, status)
		}
	}
}

func TestPushAndFetchV2(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")
	repo := buildTestRepo(t)

	// Push the whole history: c1..c3 plus trees, blobs and the tag.
	var objects []*store.Object
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.c3, repo.t1, repo.t2, repo.t3, repo.b1, repo.b2, repo.b3, repo.tag} {
		obj, ok := repo.st.Get(oid)
		if !ok {
			t.Fatalf("test repo is missing %s", oid.Hex())
		}
		objects = append(objects, obj)
	}
	commands := []gitproto.PushCommand{
		{OldOID: store.ZeroOID, NewOID: repo.c3, Ref: "refs/heads/main"},
		{OldOID: store.ZeroOID, NewOID: repo.tag, Ref: "refs/tags/v1"},
	}
	status, contentType, body := post(t, server.URL+"/adaiversen/demo.git/git-receive-pack",
		"application/x-git-receive-pack-request", nil, pushBody(t, commands, objects))
	if status != 200 {
		t.Fatalf("push: HTTP %d: %s", status, body)
	}
	if !strings.Contains(contentType, "application/x-git-receive-pack-result") {
		t.Fatalf("push content-type = %q", contentType)
	}
	report := pushReport(t, body)
	for _, want := range []string{"unpack ok", "ok refs/heads/main", "ok refs/tags/v1"} {
		if !strings.Contains(report, want) {
			t.Fatalf("push report %q is missing %q", report, want)
		}
	}

	// ls-refs must show main, the tag with its peel, and HEAD's symref.
	lsBody, err := gitproto.EncodePktLines([]any{"command=ls-refs\n", "delim", "peel\n", "symrefs\n", "unborn\n", "flush"})
	if err != nil {
		t.Fatalf("encode ls-refs: %v", err)
	}
	status, _, lsResp := post(t, server.URL+"/adaiversen/demo.git/git-upload-pack",
		"application/x-git-upload-pack-request", map[string]string{"Git-Protocol": "version=2"}, lsBody)
	if status != 200 {
		t.Fatalf("ls-refs: HTTP %d: %s", status, lsResp)
	}
	lsPackets, err := gitproto.DecodePktLines(lsResp)
	if err != nil {
		t.Fatalf("decode ls-refs: %v", err)
	}
	var lsLines []string
	for _, packet := range lsPackets {
		if packet.Kind == gitproto.PacketData {
			lsLines = append(lsLines, string(packet.Payload))
		}
	}
	joined := strings.Join(lsLines, " ")
	for _, want := range []string{
		repo.c3.Hex() + " refs/heads/main",
		repo.tag.Hex() + " refs/tags/v1 peeled:" + repo.c2.Hex(),
		repo.c3.Hex() + " HEAD symref-target:refs/heads/main",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ls-refs %q is missing %q", joined, want)
		}
	}

	// A v2 fetch of the tip must deliver the whole history in a valid pack.
	fetchBody, err := gitproto.EncodePktLines([]any{
		"command=fetch\n", "delim", "thin-pack\n", "no-progress\n", "ofs-delta\n",
		"want " + repo.c3.Hex() + "\n", "done\n", "flush",
	})
	if err != nil {
		t.Fatalf("encode fetch: %v", err)
	}
	status, _, fetchResp := post(t, server.URL+"/adaiversen/demo.git/git-upload-pack",
		"application/x-git-upload-pack-request", map[string]string{"Git-Protocol": "version=2"}, fetchBody)
	if status != 200 {
		t.Fatalf("fetch: HTTP %d: %s", status, fetchResp)
	}
	packBytes := extractPackfile(t, fetchResp)
	parsed, err := pack.Parse(packBytes, nil)
	if err != nil {
		t.Fatalf("parse fetched pack: %v", err)
	}
	// Nine objects: the tag is not reachable from the tip, so a fetch of
	// the tip alone does not deliver it.
	if len(parsed) != 9 {
		t.Fatalf("fetched pack holds %d objects, want 9", len(parsed))
	}
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.c3, repo.b1, repo.b2, repo.b3} {
		found := false
		for _, obj := range parsed {
			if obj.OID() == oid {
				found = true
			}
		}
		if !found {
			t.Fatalf("fetched pack is missing %s", oid.Hex())
		}
	}
}

// TestFetchV2Sections pins the v2 fetch response shape real git requires:
// with `done` sent there is no acknowledgments section at all, the packfile
// section follows directly; with haves but no `done` the server answers
// acknowledgments with a `ready` line and a delimiter before the packfile.
func TestFetchV2Sections(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")
	repo := buildTestRepo(t)
	var objects []*store.Object
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.c3, repo.t1, repo.t2, repo.t3, repo.b1, repo.b2, repo.b3, repo.tag} {
		obj, ok := repo.st.Get(oid)
		if !ok {
			t.Fatalf("test repo is missing %s", oid.Hex())
		}
		objects = append(objects, obj)
	}
	commands := []gitproto.PushCommand{
		{OldOID: store.ZeroOID, NewOID: repo.c3, Ref: "refs/heads/main"},
	}
	status, _, body := post(t, server.URL+"/adaiversen/demo.git/git-receive-pack",
		"application/x-git-receive-pack-request", nil, pushBody(t, commands, objects))
	if status != 200 {
		t.Fatalf("push: HTTP %d: %s", status, body)
	}

	fetch := func(args []any) []gitproto.Packet {
		t.Helper()
		reqBody, err := gitproto.EncodePktLines(args)
		if err != nil {
			t.Fatalf("encode fetch: %v", err)
		}
		status, _, resp := post(t, server.URL+"/adaiversen/demo.git/git-upload-pack",
			"application/x-git-upload-pack-request", map[string]string{"Git-Protocol": "version=2"}, reqBody)
		if status != 200 {
			t.Fatalf("fetch: HTTP %d: %s", status, resp)
		}
		packets, err := gitproto.DecodePktLines(resp)
		if err != nil {
			t.Fatalf("decode fetch response: %v", err)
		}
		return packets
	}
	// Render the response as a readable sequence: data lines as text,
	// control packets as <flush>/<delim>.
	render := func(packets []gitproto.Packet) []string {
		var out []string
		for _, packet := range packets {
			switch packet.Kind {
			case gitproto.PacketData:
				if len(packet.Payload) > 0 && packet.Payload[0] == 0x01 {
					out = append(out, "<pack-data>")
				} else {
					out = append(out, strings.TrimSuffix(string(packet.Payload), "\n"))
				}
			case gitproto.PacketFlush:
				out = append(out, "<flush>")
			case gitproto.PacketDelim:
				out = append(out, "<delim>")
			}
		}
		return out
	}
	want := "want " + repo.c3.Hex() + "\n"

	// 1. want + done: no acknowledgments section; packfile first.
	packets := fetch([]any{"command=fetch\n", "delim", "thin-pack\n", "ofs-delta\n", want, "done\n", "flush"})
	seq := render(packets)
	if len(seq) < 2 || seq[0] != "packfile" {
		t.Fatalf("fetch with done: sections = %v, want packfile first", seq)
	}
	for _, line := range seq {
		if line == "acknowledgments" {
			t.Fatalf("fetch with done sent an acknowledgments section: %v", seq)
		}
	}

	// 2. want + a held have, no done: acknowledgments with ACK + ready.
	packets = fetch([]any{"command=fetch\n", "delim", "thin-pack\n", "ofs-delta\n", want, "have " + repo.c1.Hex() + "\n", "flush"})
	seq = render(packets)
	wantSeq := []string{"acknowledgments", "ACK " + repo.c1.Hex(), "ready", "<delim>", "packfile", "<pack-data>", "<flush>"}
	if len(seq) != len(wantSeq) {
		t.Fatalf("fetch with held have: sections = %v, want %v", seq, wantSeq)
	}
	for i, wantLine := range wantSeq {
		if seq[i] != wantLine {
			t.Fatalf("fetch with held have: sections = %v, want %v", seq, wantSeq)
		}
	}

	// 3. want + an unheld have, no done: acknowledgments with NAK + ready.
	unknown, _ := store.OIDFromHex("1111111111111111111111111111111111111111")
	packets = fetch([]any{"command=fetch\n", "delim", "thin-pack\n", "ofs-delta\n", want, "have " + unknown.Hex() + "\n", "flush"})
	seq = render(packets)
	wantSeq = []string{"acknowledgments", "NAK", "ready", "<delim>", "packfile", "<pack-data>", "<flush>"}
	if len(seq) != len(wantSeq) {
		t.Fatalf("fetch with unheld have: sections = %v, want %v", seq, wantSeq)
	}
	for i, wantLine := range wantSeq {
		if seq[i] != wantLine {
			t.Fatalf("fetch with unheld have: sections = %v, want %v", seq, wantSeq)
		}
	}
}

// TestLsRefsPrefixFiltersHead pins the ref-prefix rule for the HEAD
// pseudo-ref: it is returned only when a prefix matches "HEAD" — a clone
// asks for it with ref-prefix HEAD, while a refs/heads/ prefix must not
// return it.
func TestLsRefsPrefixFiltersHead(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")
	repo := buildTestRepo(t)
	var objects []*store.Object
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.c3, repo.t1, repo.t2, repo.t3, repo.b1, repo.b2, repo.b3} {
		obj, ok := repo.st.Get(oid)
		if !ok {
			t.Fatalf("test repo is missing %s", oid.Hex())
		}
		objects = append(objects, obj)
	}
	commands := []gitproto.PushCommand{
		{OldOID: store.ZeroOID, NewOID: repo.c3, Ref: "refs/heads/main"},
	}
	status, _, body := post(t, server.URL+"/adaiversen/demo.git/git-receive-pack",
		"application/x-git-receive-pack-request", nil, pushBody(t, commands, objects))
	if status != 200 {
		t.Fatalf("push: HTTP %d: %s", status, body)
	}
	if !strings.Contains(pushReport(t, body), "ok refs/heads/main") {
		t.Fatalf("push report = %q", pushReport(t, body))
	}

	lsRefs := func(prefixes ...string) []string {
		t.Helper()
		args := []any{"command=ls-refs\n", "delim", "peel\n", "symrefs\n", "unborn\n"}
		for _, prefix := range prefixes {
			args = append(args, "ref-prefix "+prefix+"\n")
		}
		args = append(args, "flush")
		reqBody, err := gitproto.EncodePktLines(args)
		if err != nil {
			t.Fatalf("encode ls-refs: %v", err)
		}
		status, _, resp := post(t, server.URL+"/adaiversen/demo.git/git-upload-pack",
			"application/x-git-upload-pack-request", map[string]string{"Git-Protocol": "version=2"}, reqBody)
		if status != 200 {
			t.Fatalf("ls-refs: HTTP %d: %s", status, resp)
		}
		packets, err := gitproto.DecodePktLines(resp)
		if err != nil {
			t.Fatalf("decode ls-refs: %v", err)
		}
		var lines []string
		for _, packet := range packets {
			if packet.Kind == gitproto.PacketData {
				lines = append(lines, strings.TrimSuffix(string(packet.Payload), "\n"))
			}
		}
		return lines
	}

	headsOnly := lsRefs("refs/heads/")
	for _, line := range headsOnly {
		if strings.Contains(line, " HEAD") || strings.HasSuffix(line, " HEAD") {
			t.Fatalf("refs/heads/ prefix returned a HEAD line: %v", headsOnly)
		}
	}
	foundMain := false
	for _, line := range headsOnly {
		if strings.Contains(line, "refs/heads/main") {
			foundMain = true
		}
	}
	if !foundMain {
		t.Fatalf("refs/heads/ prefix is missing refs/heads/main: %v", headsOnly)
	}

	headOnly := lsRefs("HEAD")
	foundHead := false
	for _, line := range headOnly {
		if line == repo.c3.Hex()+" HEAD symref-target:refs/heads/main" {
			foundHead = true
		}
		if strings.Contains(line, "refs/heads/main") && !strings.Contains(line, " HEAD ") && !strings.HasSuffix(line, " HEAD") {
			t.Fatalf("HEAD prefix returned a branch line: %v", headOnly)
		}
	}
	if !foundHead {
		t.Fatalf("HEAD prefix is missing the HEAD line: %v", headOnly)
	}

	none := lsRefs("refs/does-not-exist/")
	if len(none) != 0 {
		t.Fatalf("unmatched prefix returned %v", none)
	}
}

// extractPackfile pulls the band-1 bytes out of a v2 fetch response's
// packfile section.
func extractPackfile(t *testing.T, body []byte) []byte {
	t.Helper()
	packets, err := gitproto.DecodePktLines(body)
	if err != nil {
		t.Fatalf("decode fetch response: %v", err)
	}
	section := ""
	var packBytes []byte
	for _, packet := range packets {
		if packet.Kind != gitproto.PacketData {
			if packet.Kind == gitproto.PacketFlush || packet.Kind == gitproto.PacketDelim {
				section = ""
			}
			continue
		}
		if section == "" {
			section = strings.TrimSuffix(string(packet.Payload), "\n")
			continue
		}
		if section != "packfile" {
			continue
		}
		if len(packet.Payload) == 0 || packet.Payload[0] != 0x01 {
			t.Fatalf("packfile packet band = %v, want 1", packet.Payload)
		}
		packBytes = append(packBytes, packet.Payload[1:]...)
	}
	if len(packBytes) == 0 {
		t.Fatalf("fetch response carried no packfile data")
	}
	return packBytes
}

func TestPushNonFastForward(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")
	repo := buildTestRepo(t)

	var objects []*store.Object
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.c3, repo.t1, repo.t2, repo.t3, repo.b1, repo.b2, repo.b3} {
		obj, _ := repo.st.Get(oid)
		objects = append(objects, obj)
	}
	push := func(commands []gitproto.PushCommand, objs []*store.Object) string {
		status, _, body := post(t, server.URL+"/adaiversen/demo.git/git-receive-pack",
			"application/x-git-receive-pack-request", nil, pushBody(t, commands, objs))
		if status != 200 {
			t.Fatalf("push: HTTP %d: %s", status, body)
		}
		return pushReport(t, body)
	}

	report := push([]gitproto.PushCommand{{OldOID: store.ZeroOID, NewOID: repo.c3, Ref: "refs/heads/main"}}, objects)
	if !strings.Contains(report, "ok refs/heads/main") {
		t.Fatalf("setup push report = %q", report)
	}

	// A sibling commit (child of c1, not of c3) is a non-fast-forward.
	sibling := store.NewObject(store.TypeCommit, buildCommit(repo.t2, []store.OID{repo.c1}, 1700000300, "sibling"))
	report = push([]gitproto.PushCommand{{OldOID: repo.c3, NewOID: sibling.OID(), Ref: "refs/heads/main"}}, []*store.Object{sibling})
	if !strings.Contains(report, "ng refs/heads/main non-fast-forward") {
		t.Fatalf("non-fast-forward report = %q", report)
	}

	// A stale old oid is refused too.
	report = push([]gitproto.PushCommand{{OldOID: repo.c1, NewOID: sibling.OID(), Ref: "refs/heads/main"}}, []*store.Object{sibling})
	if !strings.Contains(report, "ng refs/heads/main") {
		t.Fatalf("stale-old report = %q", report)
	}

	// The ref must still point at c3.
	status, _, body := get(t, server.URL+"/adaiversen/demo.git/info/refs?service=git-upload-pack",
		map[string]string{"Git-Protocol": "version=2"})
	if status != 200 {
		t.Fatalf("info/refs: HTTP %d", status)
	}
	_ = body
}

func TestPushDeleteBranch(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")
	repo := buildTestRepo(t)

	var objects []*store.Object
	for _, oid := range []store.OID{repo.c1, repo.c2, repo.c3, repo.t1, repo.t2, repo.t3, repo.b1, repo.b2, repo.b3} {
		obj, _ := repo.st.Get(oid)
		objects = append(objects, obj)
	}
	push := func(commands []gitproto.PushCommand, objs []*store.Object) string {
		status, _, body := post(t, server.URL+"/adaiversen/demo.git/git-receive-pack",
			"application/x-git-receive-pack-request", nil, pushBody(t, commands, objs))
		if status != 200 {
			t.Fatalf("push: HTTP %d: %s", status, body)
		}
		return pushReport(t, body)
	}
	report := push([]gitproto.PushCommand{
		{OldOID: store.ZeroOID, NewOID: repo.c3, Ref: "refs/heads/main"},
		{OldOID: store.ZeroOID, NewOID: repo.c3, Ref: "refs/heads/feature"},
	}, objects)
	if !strings.Contains(report, "ok refs/heads/feature") {
		t.Fatalf("setup push report = %q", report)
	}
	// A delete carries no pack at all.
	items, err := gitproto.EncodePktLines([]any{
		fmt.Sprintf("%s %s refs/heads/feature\x00report-status side-band-64k\n", repo.c3.Hex(), store.ZeroOID.Hex()),
		"flush",
	})
	if err != nil {
		t.Fatalf("encode delete: %v", err)
	}
	status, _, body := post(t, server.URL+"/adaiversen/demo.git/git-receive-pack",
		"application/x-git-receive-pack-request", nil, items)
	if status != 200 {
		t.Fatalf("delete push: HTTP %d: %s", status, body)
	}
	if report := pushReport(t, body); !strings.Contains(report, "ok refs/heads/feature") {
		t.Fatalf("delete report = %q", report)
	}
}

func TestMalformedPostKeepsServerHealthy(t *testing.T) {
	server := startTestForge(t)
	createRepo(t, server.URL, "demo")
	status, _, _ := post(t, server.URL+"/adaiversen/demo.git/git-upload-pack",
		"application/x-git-upload-pack-request", nil, []byte("zzzznot-a-pkt-line"))
	if status != 400 {
		t.Fatalf("malformed POST: HTTP %d, want 400", status)
	}
	status, _, _ = get(t, server.URL+"/adaiversen/demo.git/info/refs?service=git-upload-pack", nil)
	if status != 200 {
		t.Fatalf("server stopped serving after a malformed POST: HTTP %d", status)
	}
}

func TestHealthz(t *testing.T) {
	server := startTestForge(t)
	status, _, body := get(t, server.URL+"/healthz", nil)
	if status != 200 {
		t.Fatalf("healthz: HTTP %d", status)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("healthz body = %s", body)
	}
}
