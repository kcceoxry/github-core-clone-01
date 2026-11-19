// Object parsing for the fetch planner: commit headers, tag headers and
// tree entries, read straight from a store object's bytes. These parsers are
// strict about shape (a malformed object is an error, never a guess) and
// tolerant about content the planner does not use (extra commit headers are
// skipped, not rejected).
package gitproto

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"forge/internal/store"
)

// Commit holds the fields a fetch walk needs from a commit object.
type Commit struct {
	Tree      store.OID
	Parents   []store.OID
	Committer int64 // committer timestamp, Unix seconds; 0 when unparsable
}

// ParseCommit parses a commit object's headers: the tree line, zero or more
// parent lines, and the committer timestamp. The message is not needed here.
func ParseCommit(data []byte) (*Commit, error) {
	commit := &Commit{}
	blank := bytes.Index(data, []byte("\n\n"))
	headers := data
	if blank >= 0 {
		headers = data[:blank]
	}
	for _, line := range strings.Split(string(headers), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "tree":
			oid, err := store.OIDFromHex(value)
			if err != nil {
				return nil, fmt.Errorf("gitproto: commit has a bad tree id %q", value)
			}
			commit.Tree = oid
		case "parent":
			oid, err := store.OIDFromHex(value)
			if err != nil {
				return nil, fmt.Errorf("gitproto: commit has a bad parent id %q", value)
			}
			commit.Parents = append(commit.Parents, oid)
		case "committer":
			commit.Committer = parseIdentityTimestamp(value)
		}
	}
	if commit.Tree.IsZero() {
		return nil, fmt.Errorf("gitproto: commit has no tree header")
	}
	return commit, nil
}

// parseIdentityTimestamp pulls the Unix timestamp out of an author or
// committer identity line ("Name <mail> 1700000000 +0000"). A line without
// one yields 0, which the planner treats as "no time limit applies".
func parseIdentityTimestamp(value string) int64 {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return 0
	}
	ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

// Tag holds the fields a fetch walk needs from an annotated tag object.
type Tag struct {
	Object store.OID
	Type   store.ObjectType
	Name   string
}

// ParseTag parses a tag object's headers: object, type and tag.
func ParseTag(data []byte) (*Tag, error) {
	tag := &Tag{}
	blank := bytes.Index(data, []byte("\n\n"))
	headers := data
	if blank >= 0 {
		headers = data[:blank]
	}
	for _, line := range strings.Split(string(headers), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("gitproto: tag header %q has no value", line)
		}
		switch key {
		case "object":
			oid, err := store.OIDFromHex(value)
			if err != nil {
				return nil, fmt.Errorf("gitproto: tag has a bad object id %q", value)
			}
			tag.Object = oid
		case "type":
			typ, err := store.ParseObjectType(value)
			if err != nil {
				return nil, fmt.Errorf("gitproto: tag has a bad type %q", value)
			}
			tag.Type = typ
		case "tag":
			tag.Name = value
		}
	}
	if tag.Object.IsZero() {
		return nil, fmt.Errorf("gitproto: tag has no object header")
	}
	return tag, nil
}

// TreeEntry is one entry of a tree object: its mode, name and target.
type TreeEntry struct {
	Mode string
	Name string
	OID  store.OID
}

// ParseTree parses a tree object into its entries, in stored order. An entry
// is "<mode> <name>\0<20-byte oid>"; a truncated entry is an error.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			return nil, fmt.Errorf("gitproto: tree entry has no mode separator")
		}
		mode := string(data[offset : offset+space])
		offset += space + 1
		nul := bytes.IndexByte(data[offset:], 0)
		if nul < 0 {
			return nil, fmt.Errorf("gitproto: tree entry has no name terminator")
		}
		name := string(data[offset : offset+nul])
		offset += nul + 1
		if offset+20 > len(data) {
			return nil, fmt.Errorf("gitproto: tree entry %q has a truncated object id", name)
		}
		var oid store.OID
		copy(oid[:], data[offset:offset+20])
		offset += 20
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return entries, nil
}

// IsGitlink reports whether a tree entry is a submodule pointer (mode
// 160000). A gitlink names a commit the repository does not hold, so the
// walk skips it, exactly as the kit client does.
func (e TreeEntry) IsGitlink() bool { return e.Mode == "160000" }

// PeelTag follows an annotated tag to the object it points at, recursing
// through tag-of-tag chains. It returns the final object id and type.
func PeelTag(st *store.Store, oid store.OID) (store.OID, store.ObjectType, error) {
	for depth := 0; depth < 10; depth++ {
		obj, ok := st.Get(oid)
		if !ok {
			return store.OID{}, 0, fmt.Errorf("gitproto: peel target %s is not in the store", oid.Hex())
		}
		if obj.Type != store.TypeTag {
			return oid, obj.Type, nil
		}
		tag, err := ParseTag(obj.Data)
		if err != nil {
			return store.OID{}, 0, err
		}
		oid = tag.Object
	}
	return store.OID{}, 0, fmt.Errorf("gitproto: tag chain deeper than 10 at %s", oid.Hex())
}
