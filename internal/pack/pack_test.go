package pack

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/store"
)

// --- test helpers: small encoders that mirror the git format -----------

// encodeSizeVarint encodes a plain 7-bit group varint (delta header sizes).
func encodeSizeVarint(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n > 0 {
			out = append(out, b|0x80)
		} else {
			out = append(out, b)
			return out
		}
	}
}

// encodeCopy builds one COPY instruction for offset/size.
func encodeCopy(offset, size int) []byte {
	op := byte(0x80)
	var tail []byte
	for bit := 0; bit < 4; bit++ {
		if offset&(0xff<<(8*bit)) != 0 {
			op |= 1 << bit
			tail = append(tail, byte(offset>>(8*bit)))
		}
	}
	for bit := 0; bit < 3; bit++ {
		if size&(0xff<<(8*bit)) != 0 {
			op |= 0x10 << bit
			tail = append(tail, byte(size>>(8*bit)))
		}
	}
	return append([]byte{op}, tail...)
}

// encodeOfsOffset encodes an OFS_DELTA negative offset with git's plus-one
// bias: the least significant 7 bits go last, each earlier byte is one more
// than the plain value.
func encodeOfsOffset(v int) []byte {
	out := []byte{byte(v & 0x7f)}
	v >>= 7
	for v > 0 {
		v--
		out = append([]byte{0x80 | byte(v&0x7f)}, out...)
		v >>= 7
	}
	return out
}

// entryHeader builds a pack entry type and size header.
func entryHeader(t store.ObjectType, size int) []byte {
	var out []byte
	b := byte(t<<4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		out = append(out, b|0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, b)
	return out
}

// compress returns the zlib stream of data.
func compress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeDelta builds a delta from base to target with one COPY of the common
// prefix, INSERT literals for the middle, and one COPY of the common suffix.
func makeDelta(t *testing.T, base, target []byte) []byte {
	t.Helper()
	prefix := 0
	for prefix < len(base) && prefix < len(target) && base[prefix] == target[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(base)-prefix && suffix < len(target)-prefix && base[len(base)-1-suffix] == target[len(target)-1-suffix] {
		suffix++
	}
	var d []byte
	d = append(d, encodeSizeVarint(len(base))...)
	d = append(d, encodeSizeVarint(len(target))...)
	if prefix > 0 {
		d = append(d, encodeCopy(0, prefix)...)
	}
	mid := target[prefix : len(target)-suffix]
	for len(mid) > 0 {
		n := len(mid)
		if n > 127 {
			n = 127
		}
		d = append(d, byte(n))
		d = append(d, mid[:n]...)
		mid = mid[n:]
	}
	if suffix > 0 {
		d = append(d, encodeCopy(len(base)-suffix, suffix)...)
	}
	return d
}

// buildPack assembles a packfile from already encoded entries and appends
// the correct SHA-1 trailer.
func buildPack(t *testing.T, entries [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(entries)))
	buf.Write(hdr[:])
	for _, e := range entries {
		buf.Write(e)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// plainEntry encodes one non-delta entry.
func plainEntry(t *testing.T, typ store.ObjectType, data []byte) []byte {
	t.Helper()
	var e []byte
	e = append(e, entryHeader(typ, len(data))...)
	e = append(e, compress(t, data)...)
	return e
}

// --- the verified examples ---------------------------------------------

func TestBlobOIDHello(t *testing.T) {
	obj := store.NewObject(store.TypeBlob, []byte("hello\n"))
	if got, want := obj.ID(), "ce013625030ba8dba906f756967f9e9ca394464a"; got != want {
		t.Fatalf("blob hello OID = %s, want %s", got, want)
	}
}

func TestDeltaHelloWorld(t *testing.T) {
	delta := []byte{0x06, 0x0c, 0x90, 0x05, 0x06, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64, 0x91, 0x05, 0x01}
	out, err := ApplyDelta([]byte("hello\n"), delta)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if string(out) != "hello world\n" {
		t.Fatalf("ApplyDelta = %q, want %q", string(out), "hello world\n")
	}
	obj := store.NewObject(store.TypeBlob, out)
	if got, want := obj.ID(), "3b18e512dba79e4c8300dd08aeb37f8e728b8dad"; got != want {
		t.Fatalf("delta target OID = %s, want %s", got, want)
	}
}

// --- pack round trip ----------------------------------------------------

func TestPackRoundTrip(t *testing.T) {
	blob1 := store.NewObject(store.TypeBlob, []byte("hello\n"))
	blob2 := store.NewObject(store.TypeBlob, []byte("world\n"))

	var tree bytes.Buffer
	tree.WriteString("100644 a\x00")
	oid1 := blob1.OID()
	tree.Write(oid1[:])
	tree.WriteString("100644 b\x00")
	oid2 := blob2.OID()
	tree.Write(oid2[:])
	treeObj := store.NewObject(store.TypeTree, tree.Bytes())

	commitText := "tree " + treeObj.ID() + "\n" +
		"author Test <t@t.example> 1700000000 +0000\n" +
		"committer Test <t@t.example> 1700000000 +0000\n\ninitial\n"
	commitObj := store.NewObject(store.TypeCommit, []byte(commitText))

	want := []*store.Object{commitObj, treeObj, blob1, blob2}
	packed, err := Write(want)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Parse(packed, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Parse returned %d objects, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].OID() != w.OID() {
			t.Fatalf("object %d OID = %s, want %s", i, got[i].ID(), w.ID())
		}
		if got[i].Type != w.Type {
			t.Fatalf("object %d type = %v, want %v", i, got[i].Type, w.Type)
		}
		if !bytes.Equal(got[i].Data, w.Data) {
			t.Fatalf("object %d data differs", i)
		}
	}
}

func TestPackDeterministic(t *testing.T) {
	objs := []*store.Object{
		store.NewObject(store.TypeBlob, []byte("one\n")),
		store.NewObject(store.TypeBlob, []byte("two\n")),
	}
	a, err := Write(objs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Write(objs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("Write of the same objects twice produced different bytes")
	}
}

// --- delta packs built by hand ------------------------------------------

func TestParseOfsDeltaPack(t *testing.T) {
	base := []byte("hello world, this is a longer base for delta testing\n")
	target := []byte("hello world, this is a longer base for delta TESTING!\n")
	baseObj := store.NewObject(store.TypeBlob, base)
	wantObj := store.NewObject(store.TypeBlob, target)

	delta := makeDelta(t, base, target)
	baseEntry := plainEntry(t, store.TypeBlob, base)

	// The delta entry starts right after the base entry, so its offset
	// back to the base is len(header) + len(baseEntry).
	deltaOffset := 12 + len(baseEntry)
	var deltaEntry []byte
	deltaEntry = append(deltaEntry, entryHeader(6, len(delta))...)
	deltaEntry = append(deltaEntry, encodeOfsOffset(deltaOffset-12)...)
	deltaEntry = append(deltaEntry, compress(t, delta)...)

	packed := buildPack(t, [][]byte{baseEntry, deltaEntry})
	got, err := Parse(packed, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Parse returned %d objects, want 2", len(got))
	}
	if got[0].OID() != baseObj.OID() {
		t.Fatalf("base OID = %s, want %s", got[0].ID(), baseObj.ID())
	}
	if got[1].OID() != wantObj.OID() {
		t.Fatalf("delta target OID = %s, want %s", got[1].ID(), wantObj.ID())
	}
	if !bytes.Equal(got[1].Data, target) {
		t.Fatalf("delta target data = %q, want %q", got[1].Data, target)
	}
}

func TestParseRefDeltaPack(t *testing.T) {
	base := []byte("the quick brown fox jumps over the lazy dog\n")
	target := []byte("the quick brown fox JUMPS over the lazy dog\n")
	baseObj := store.NewObject(store.TypeBlob, base)
	wantObj := store.NewObject(store.TypeBlob, target)

	delta := makeDelta(t, base, target)
	baseEntry := plainEntry(t, store.TypeBlob, base)

	var deltaEntry []byte
	deltaEntry = append(deltaEntry, entryHeader(7, len(delta))...)
	baseOID := baseObj.OID()
	deltaEntry = append(deltaEntry, baseOID[:]...)
	deltaEntry = append(deltaEntry, compress(t, delta)...)

	packed := buildPack(t, [][]byte{baseEntry, deltaEntry})
	got, err := Parse(packed, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 || got[1].OID() != wantObj.OID() {
		t.Fatalf("ref-delta target OID mismatch: got %v, want %s", got, wantObj.ID())
	}
}

func TestParseThinPack(t *testing.T) {
	base := []byte("thin pack base content, held only in the store\n")
	target := []byte("thin pack base content, held only in the store!\n")
	baseObj := store.NewObject(store.TypeBlob, base)
	wantObj := store.NewObject(store.TypeBlob, target)

	delta := makeDelta(t, base, target)
	var deltaEntry []byte
	deltaEntry = append(deltaEntry, entryHeader(7, len(delta))...)
	baseOID := baseObj.OID()
	deltaEntry = append(deltaEntry, baseOID[:]...)
	deltaEntry = append(deltaEntry, compress(t, delta)...)
	packed := buildPack(t, [][]byte{deltaEntry})

	st := store.New()
	st.Put(baseObj)
	got, err := Parse(packed, func(oid store.OID) (*store.Object, bool) { return st.Get(oid) })
	if err != nil {
		t.Fatalf("Parse thin pack: %v", err)
	}
	if len(got) != 1 || got[0].OID() != wantObj.OID() {
		t.Fatalf("thin pack target OID mismatch: got %v, want %s", got, wantObj.ID())
	}
}

// --- corrupt input must error, never panic -------------------------------

func TestParseCorrupt(t *testing.T) {
	blob := store.NewObject(store.TypeBlob, []byte("hello\n"))
	good, err := Write([]*store.Object{blob})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"truncated":   good[:len(good)-30],
		"bad magic":   append([]byte("XACK"), good[4:]...),
		"bad trailer": append(append([]byte{}, good[:len(good)-1]...), good[len(good)-1]^0xff),
		"bad version": append(append([]byte{}, good[:4]...), append([]byte{0, 0, 0, 9}, good[8:]...)...),
	}
	// Wrong declared count: the parser runs into the trailer mid-entry.
	wrongCount := append([]byte{}, good...)
	wrongCount[11] = 99
	cases["wrong count"] = wrongCount

	for name, data := range cases {
		if _, err := Parse(data, nil); err == nil {
			t.Errorf("Parse(%s): got no error, want an error", name)
		}
	}
}

func TestApplyDeltaCorrupt(t *testing.T) {
	base := []byte("hello\n")
	cases := map[string][]byte{
		"reserved 0x00":      {0x06, 0x06, 0x00},
		"wrong source size":  {0x07, 0x06, 0x06, 'x', 'x', 'x', 'x', 'x', 'x'},
		"copy past the base": {0x06, 0x0b, 0x91, 0x05, 0x06},
		"truncated insert":   {0x06, 0x0c, 0x06, 'a', 'b'},
		"wrong target size":  {0x06, 0x05, 0x90, 0x05, 0x06, ' ', 'w', 'o', 'r', 'l', 'd', 0x91, 0x05, 0x01},
	}
	for name, delta := range cases {
		if _, err := ApplyDelta(base, delta); err == nil {
			t.Errorf("ApplyDelta(%s): got no error, want an error", name)
		}
	}
}

func TestParseDeltaBadBase(t *testing.T) {
	// An ofs-delta whose base offset points at no entry.
	delta := makeDelta(t, []byte("hello\n"), []byte("hello!\n"))
	var entry []byte
	entry = append(entry, entryHeader(6, len(delta))...)
	entry = append(entry, encodeOfsOffset(11)...) // 11 back from offset 12 lands before the pack body
	entry = append(entry, compress(t, delta)...)
	packed := buildPack(t, [][]byte{entry})
	if _, err := Parse(packed, nil); err == nil {
		t.Error("Parse(ofs-delta with missing base): got no error, want an error")
	}

	// A ref-delta whose base is nowhere.
	unknown, _ := store.OIDFromHex("1111111111111111111111111111111111111111")
	var refEntry []byte
	refEntry = append(refEntry, entryHeader(7, len(delta))...)
	refEntry = append(refEntry, unknown[:]...)
	refEntry = append(refEntry, compress(t, delta)...)
	packed = buildPack(t, [][]byte{refEntry})
	if _, err := Parse(packed, nil); err == nil {
		t.Error("Parse(ref-delta with missing base): got no error, want an error")
	}
}

// --- loose objects -------------------------------------------------------

func TestLooseRoundTrip(t *testing.T) {
	obj := store.NewObject(store.TypeBlob, []byte("loose content\n"))
	enc, err := WriteLoose(obj)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadLoose(enc)
	if err != nil {
		t.Fatalf("ReadLoose: %v", err)
	}
	if back.OID() != obj.OID() || !bytes.Equal(back.Data, obj.Data) {
		t.Fatalf("loose round trip mismatch: got %s %q", back.ID(), back.Data)
	}
	if _, err := ReadLoose([]byte("not a zlib stream")); err == nil {
		t.Error("ReadLoose(garbage): got no error, want an error")
	}
}

// --- interop with real git ----------------------------------------------
// These tests run only when a git binary is on PATH, and they cross-check
// this package against git itself in both directions.

func gitAvailable(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not on PATH")
	}
	return path
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.example",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.example",
		"GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700000000 +0000")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func TestInteropParseGitPack(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "initial")
	runGit(t, dir, "gc", "-q")

	packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("no packfile after git gc: packs=%v err=%v", packs, err)
	}
	raw, err := os.ReadFile(packs[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw, nil)
	if err != nil {
		t.Fatalf("Parse(git pack): %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Parse(git pack): no objects")
	}
	// Every object git knows must be in the parsed result, and vice versa.
	check := runGit(t, dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(check), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 3 {
			t.Fatalf("unexpected batch-check line %q", line)
		}
		want[parts[0]] = parts[1]
	}
	if len(got) != len(want) {
		t.Fatalf("Parse(git pack) returned %d objects, git lists %d", len(got), len(want))
	}
	for _, obj := range got {
		typ, ok := want[obj.ID()]
		if !ok {
			t.Fatalf("parsed object %s is unknown to git", obj.ID())
		}
		if typ != obj.Type.Name() {
			t.Fatalf("object %s type = %s, git says %s", obj.ID(), obj.Type.Name(), typ)
		}
	}
}

func TestInteropGitReadsOurPack(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")

	blob := store.NewObject(store.TypeBlob, []byte("written by forge\n"))
	var tree bytes.Buffer
	tree.WriteString("100644 f.txt\x00")
	blobOID := blob.OID()
	tree.Write(blobOID[:])
	treeObj := store.NewObject(store.TypeTree, tree.Bytes())
	commitText := "tree " + treeObj.ID() + "\n" +
		"author Test <t@t.example> 1700000000 +0000\n" +
		"committer Test <t@t.example> 1700000000 +0000\n\nforge commit\n"
	commitObj := store.NewObject(store.TypeCommit, []byte(commitText))

	packed, err := Write([]*store.Object{commitObj, treeObj, blob})
	if err != nil {
		t.Fatal(err)
	}
	// Hand the pack to git: index-pack must accept it, then cat-file must
	// return the exact content back.
	cmd := exec.Command("git", "index-pack", "--stdin", "--keep")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(packed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git index-pack rejected our pack: %v\n%s", err, out)
	}
	out := runGit(t, dir, "cat-file", "-p", commitObj.ID())
	if !strings.Contains(out, "forge commit") {
		t.Fatalf("git cat-file returned %q, want the forge commit", out)
	}
	out = runGit(t, dir, "cat-file", "-p", blob.ID())
	if out != "written by forge\n" {
		t.Fatalf("git cat-file blob returned %q", out)
	}
	out = runGit(t, dir, "cat-file", "-p", treeObj.ID())
	if !strings.Contains(out, "f.txt") {
		t.Fatalf("git cat-file tree returned %q", out)
	}
}
