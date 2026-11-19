// Receive-pack: the v0 push endpoint.
//
// The request is a command list of pkt-lines (`<old> <new> <ref>`, the first
// carrying capabilities after a NUL byte), a flush, then the pack. The
// server unpacks first: a corrupt pack, or a pack whose new tips are not
// fully connected in the store, fails the whole push with an `unpack` line
// and no ref moves. Then each command is judged on its own: the old object
// id must match the ref, a delete needs the ref to exist, and an update must
// be a fast-forward. Accepted commands move their ref; refused ones are
// reported per ref with `ng <ref> <reason>`, and a non-fast-forward refusal
// always carries the exact reason `non-fast-forward`.
package gitproto

import (
	"fmt"
	"strings"

	"forge/internal/pack"
	"forge/internal/store"
)

// PushCommand is one receive-pack ref update.
type PushCommand struct {
	OldOID store.OID
	NewOID store.OID
	Ref    string
}

// PushRefReport is one ref's line in the report-status answer.
type PushRefReport struct {
	Ref    string
	OK     bool
	Reason string
}

// PushReport is the whole report-status answer: the unpack verdict plus one
// line per command.
type PushReport struct {
	UnpackOK     bool
	UnpackDetail string
	Refs         []PushRefReport
}

// HandleReceivePack answers a receive-pack request body and returns the
// sideband-framed report-status response.
func HandleReceivePack(st *store.Store, body []byte) ([]byte, error) {
	commands, packBytes, err := parsePushCommands(body)
	if err != nil {
		return nil, err
	}

	report := &PushReport{}
	if err := unpackPush(st, commands, packBytes); err != nil {
		report.UnpackOK = false
		report.UnpackDetail = err.Error()
		return buildPushReport(report)
	}
	report.UnpackOK = true
	report.UnpackDetail = "ok"

	for _, command := range commands {
		report.Refs = append(report.Refs, applyPushCommand(st, command))
	}
	return buildPushReport(report)
}

// parsePushCommands splits a receive-pack body into its command list and
// its pack bytes. The command list ends at the first flush packet.
func parsePushCommands(body []byte) ([]PushCommand, []byte, error) {
	packets, rest, err := SplitPktLines(body)
	if err != nil {
		return nil, nil, err
	}
	var commands []PushCommand
	for index, packet := range packets {
		if packet.Kind != PacketData {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		if index == 0 {
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				line = line[:nul]
			}
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, nil, fmt.Errorf("gitproto: push command %q has %d fields, want 3", line, len(fields))
		}
		oldOID, err := store.OIDFromHex(fields[0])
		if err != nil {
			return nil, nil, fmt.Errorf("gitproto: push command has a bad old id %q", fields[0])
		}
		newOID, err := store.OIDFromHex(fields[1])
		if err != nil {
			return nil, nil, fmt.Errorf("gitproto: push command has a bad new id %q", fields[1])
		}
		commands = append(commands, PushCommand{OldOID: oldOID, NewOID: newOID, Ref: fields[2]})
	}
	if len(commands) == 0 {
		return nil, nil, fmt.Errorf("gitproto: push names no commands")
	}
	return commands, rest, nil
}

// unpackPush validates and stores a push's pack. An update command needs
// pack bytes; a delete-only push needs none. Every new tip must be fully
// connected in the store once the pack's objects are in.
func unpackPush(st *store.Store, commands []PushCommand, packBytes []byte) error {
	needsPack := false
	for _, command := range commands {
		if !command.NewOID.IsZero() {
			needsPack = true
			break
		}
	}
	if !needsPack {
		return nil
	}
	if len(packBytes) == 0 {
		return fmt.Errorf("no pack data for an update")
	}
	objects, err := pack.Parse(packBytes, st.Get)
	if err != nil {
		return fmt.Errorf("unpack failed: %w", err)
	}
	// The objects this pack brought, by identity, so the connectivity
	// walk can resolve them before they are stored.
	packObjs := make(map[store.OID]*store.Object, len(objects))
	for _, obj := range objects {
		packObjs[obj.OID()] = obj
	}
	// Verify connectivity before storing anything: a failed push must
	// not leave orphan objects behind, or a later push could build on
	// a tip whose closure was never verified.
	for _, command := range commands {
		if command.NewOID.IsZero() {
			continue
		}
		if err := checkConnected(st, command.NewOID, packObjs); err != nil {
			return fmt.Errorf("unpack failed: %w", err)
		}
	}
	for _, obj := range objects {
		st.Put(obj)
	}
	return nil
}

// checkConnected walks from a tip and reports the first object it cannot
// find in the store or the pack being unpacked. Gitlinks are skipped:
// they name commits the repository does not hold.
//
// The walk descends only into objects the pack brought (packObjs). An
// object the store already held is checked for existence and not
// descended into: every object in the store arrived together with its
// whole closure (a pack's objects are checked here before they are put,
// a contents write creates blob, tree and commit together, and a merge
// commit's tree is built from trees already in the store), so a held
// object's closure is present by construction. Without this boundary a
// push onto a 200-commit history re-walked all 200 commits, which made
// the push workload quadratic in the history length.
func checkConnected(st *store.Store, tip store.OID, packObjs map[store.OID]*store.Object) error {
	seen := make(map[store.OID]bool)
	queue := []store.OID{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := packObjs[oid]
		if !ok {
			obj, ok = st.Get(oid)
		}
		if !ok {
			return fmt.Errorf("object %s is missing", oid.Hex())
		}
		if _, inPack := packObjs[oid]; !inPack {
			continue
		}
		switch obj.Type {
		case store.TypeCommit:
			commit, err := ParseCommit(obj.Data)
			if err != nil {
				return err
			}
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case store.TypeTag:
			tag, err := ParseTag(obj.Data)
			if err != nil {
				return err
			}
			queue = append(queue, tag.Object)
		case store.TypeTree:
			entries, err := ParseTree(obj.Data)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.IsGitlink() {
					continue
				}
				queue = append(queue, entry.OID)
			}
		}
	}
	return nil
}

// applyPushCommand judges one command against the store's current state and
// applies it when it is accepted. A refusal never moves the ref.
func applyPushCommand(st *store.Store, command PushCommand) PushRefReport {
	ref := command.Ref
	if !strings.HasPrefix(ref, "refs/") {
		return PushRefReport{Ref: ref, Reason: "invalid ref name"}
	}
	current, exists := st.GetRef(ref)
	if command.OldOID != current {
		return PushRefReport{Ref: ref, Reason: "old oid mismatch"}
	}
	if command.NewOID.IsZero() {
		if !exists {
			return PushRefReport{Ref: ref, Reason: "ref does not exist"}
		}
		st.DeleteRef(ref)
		return PushRefReport{Ref: ref, OK: true}
	}
	if exists {
		ancestor, err := isAncestor(st, current, command.NewOID)
		if err != nil {
			return PushRefReport{Ref: ref, Reason: err.Error()}
		}
		if !ancestor {
			return PushRefReport{Ref: ref, Reason: "non-fast-forward"}
		}
	}
	st.SetRef(ref, command.NewOID)
	return PushRefReport{Ref: ref, OK: true}
}

// isAncestor reports whether ancestor is reachable from descendant by
// following parent links (or is the descendant itself).
func isAncestor(st *store.Store, ancestor, descendant store.OID) (bool, error) {
	if ancestor == descendant {
		return true, nil
	}
	seen := make(map[store.OID]bool)
	queue := []store.OID{descendant}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := st.Get(oid)
		if !ok {
			return false, fmt.Errorf("object %s is missing", oid.Hex())
		}
		if obj.Type != store.TypeCommit {
			continue
		}
		commit, err := ParseCommit(obj.Data)
		if err != nil {
			return false, err
		}
		for _, parent := range commit.Parents {
			if parent == ancestor {
				return true, nil
			}
			queue = append(queue, parent)
		}
	}
	return false, nil
}

// buildPushReport frames a report-status answer: the unpack line and one
// line per ref, pkt-lined inside sideband band 1, then a final flush.
func buildPushReport(report *PushReport) ([]byte, error) {
	var items []any
	if report.UnpackOK {
		items = append(items, "unpack ok\n")
	} else {
		items = append(items, fmt.Sprintf("unpack %s\n", report.UnpackDetail))
	}
	for _, ref := range report.Refs {
		if ref.OK {
			items = append(items, fmt.Sprintf("ok %s\n", ref.Ref))
		} else {
			items = append(items, fmt.Sprintf("ng %s %s\n", ref.Ref, ref.Reason))
		}
	}
	items = append(items, "flush")
	inner, err := EncodePktLines(items)
	if err != nil {
		return nil, err
	}
	chunks, err := SidebandChunks(inner)
	if err != nil {
		return nil, err
	}
	out := append(chunks, []byte("0000")...)
	return out, nil
}
