// Ref advertisement: the v0 ref list and the v2 capability list the kit
// client parses, plus the v2 ls-refs command.
//
// The two protocol versions answer the same info/refs URL, told apart by the
// Git-Protocol request header: version=2 gets the v2 capability list, anything
// else gets the v0 ref list. Receive-pack is v0-only, whatever the header
// says: there is no v2 push.
package gitproto

import (
	"fmt"
	"strings"

	"forge/internal/store"
)

// AgentName is the agent string this server advertises where the client
// expects an agent capability.
const AgentName = "forge/1.0"

// DefaultBranch is the branch HEAD points at in every repository.
const DefaultBranch = "refs/heads/main"

// uploadPackV0Caps are the capabilities on a v0 upload-pack advertisement's
// first ref line. symref names HEAD's target; side-band-64k is required by
// the client's v0 fetch; multi_ack_detailed, thin-pack, ofs-delta, shallow
// and filter name the fetch features the server speaks.
const uploadPackV0Caps = "symref=HEAD:refs/heads/main side-band-64k multi_ack_detailed thin-pack ofs-delta shallow filter agent=forge/1.0"

// receivePackV0Caps are the capabilities on a v0 receive-pack advertisement.
// report-status, side-band-64k and delete-refs are required by the checks;
// report-status-v2 is the same report with option lines, which this server
// never needs to send.
const receivePackV0Caps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=forge/1.0"

// refLine is one ref for an advertisement: its name, its tip, and, for an
// annotated tag, the commit it peels to.
type refLine struct {
	name   string
	oid    store.OID
	peeled store.OID
	hasPeeled bool
}

// repoRefs returns a repository's refs in advertisement order: HEAD first is
// handled by the caller; these are the refs/ lines, sorted by name, each
// followed by its peeled line when it names an annotated tag.
func repoRefs(st *store.Store) []refLine {
	var lines []refLine
	for _, name := range st.RefNames() {
		oid, ok := st.GetRef(name)
		if !ok {
			continue
		}
		line := refLine{name: name, oid: oid}
		if strings.HasPrefix(name, "refs/tags/") {
			if peeled, _, err := PeelTag(st, oid); err == nil && peeled != oid {
				line.peeled = peeled
				line.hasPeeled = true
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// headOID returns the object HEAD points at: the default branch's tip, or
// any branch tip when the default branch does not exist, or zero when the
// repository has no refs at all.
func headOID(st *store.Store) store.OID {
	if oid, ok := st.GetRef(DefaultBranch); ok {
		return oid
	}
	for _, name := range st.RefNames() {
		if strings.HasPrefix(name, "refs/heads/") {
			oid, _ := st.GetRef(name)
			return oid
		}
	}
	return store.ZeroOID
}

// AdvertiseV0UploadPack builds a v0 upload-pack advertisement: the service
// preamble, a flush, then the refs. The flush after the preamble is part of
// the wire format: real git servers send it, and real git clients fail
// without it. The first line carries the capabilities after a NUL byte; an
// empty repository advertises only the capabilities^{} pseudo-ref at the
// zero object id.
func AdvertiseV0UploadPack(st *store.Store) ([]byte, error) {
	items := []any{"# service=git-upload-pack\n", "flush"}
	refs := repoRefs(st)
	if len(refs) == 0 {
		items = append(items, fmt.Sprintf("%s capabilities^{}\x00%s\n", store.ZeroOID.Hex(), uploadPackV0Caps))
	} else {
		head := headOID(st)
		items = append(items, fmt.Sprintf("%s HEAD\x00%s\n", head.Hex(), uploadPackV0Caps))
		for _, ref := range refs {
			items = append(items, fmt.Sprintf("%s %s\n", ref.oid.Hex(), ref.name))
			if ref.hasPeeled {
				items = append(items, fmt.Sprintf("%s %s^{}\n", ref.peeled.Hex(), ref.name))
			}
		}
	}
	items = append(items, "flush")
	return EncodePktLines(items)
}

// AdvertiseV0ReceivePack builds a v0 receive-pack advertisement: the same
// shape as the upload-pack one, with the receive-pack service name and
// capabilities. The checks require report-status, side-band-64k and
// delete-refs, and the advertised refs must include the pushed tip.
func AdvertiseV0ReceivePack(st *store.Store) ([]byte, error) {
	items := []any{"# service=git-receive-pack\n", "flush"}
	refs := repoRefs(st)
	if len(refs) == 0 {
		items = append(items, fmt.Sprintf("%s capabilities^{}\x00%s\n", store.ZeroOID.Hex(), receivePackV0Caps))
	} else {
		head := headOID(st)
		items = append(items, fmt.Sprintf("%s HEAD\x00%s\n", head.Hex(), receivePackV0Caps))
		for _, ref := range refs {
			items = append(items, fmt.Sprintf("%s %s\n", ref.oid.Hex(), ref.name))
			if ref.hasPeeled {
				items = append(items, fmt.Sprintf("%s %s^{}\n", ref.peeled.Hex(), ref.name))
			}
		}
	}
	items = append(items, "flush")
	return EncodePktLines(items)
}

// AdvertiseV2UploadPack builds a v2 upload-pack advertisement: the service
// preamble, a flush, then `version 2` and one capability per line. The
// flush after the preamble is part of the wire format: real git servers
// send it, and real git clients fail without it. fetch names shallow and
// filter, because the client throws before fetching when either is missing;
// ls-refs names unborn, because the client asks for it.
func AdvertiseV2UploadPack(st *store.Store) ([]byte, error) {
	items := []any{
		"# service=git-upload-pack\n",
		"flush",
		"version 2\n",
		"agent=forge/1.0\n",
		"ls-refs=unborn\n",
		"fetch=shallow filter\n",
		"object-format=sha1\n",
		"flush",
	}
	return EncodePktLines(items)
}

// LsRefsOptions carries a v2 ls-refs request's arguments.
type LsRefsOptions struct {
	Peel     bool
	Symrefs  bool
	Unborn   bool
	Prefixes []string
}

// LsRefs builds a v2 ls-refs answer: HEAD first (as a symref line, or an
// unborn line when the repository is empty), then the refs in sorted order.
// A ref is listed only when it matches a requested prefix; HEAD is listed
// only when no prefix was requested. Peeled tags carry their peeled commit
// only when peel was requested, and the symref target only when symrefs was.
func LsRefs(st *store.Store, opts LsRefsOptions) ([]byte, error) {
	matches := func(name string) bool {
		if len(opts.Prefixes) == 0 {
			return true
		}
		for _, prefix := range opts.Prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}

	var items []any
	refs := repoRefs(st)
	if len(refs) == 0 {
		// The unborn HEAD line is a ref named HEAD, so it obeys the same
		// prefix filter as every other ref: it is only sent when a
		// prefix matches "HEAD".
		if opts.Unborn && matches("HEAD") {
			items = append(items, "unborn HEAD symref-target:refs/heads/main\n")
		}
	} else {
		head := headOID(st)
		if matches("HEAD") {
			if opts.Symrefs {
				items = append(items, fmt.Sprintf("%s HEAD symref-target:%s\n", head.Hex(), DefaultBranch))
			} else {
				items = append(items, fmt.Sprintf("%s HEAD\n", head.Hex()))
			}
		}
		for _, ref := range refs {
			if !matches(ref.name) {
				continue
			}
			line := fmt.Sprintf("%s %s", ref.oid.Hex(), ref.name)
			if ref.hasPeeled && opts.Peel {
				line += fmt.Sprintf(" peeled:%s", ref.peeled.Hex())
			}
			items = append(items, line+"\n")
		}
	}
	items = append(items, "flush")
	return EncodePktLines(items)
}
