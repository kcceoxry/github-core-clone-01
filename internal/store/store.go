// Package store holds the forge's in-memory object database.
//
// A git object is content addressed: its identity is the SHA-1 of the header
// "<type> <len>\0" followed by its raw content. The store keeps objects in a
// map keyed by that identity and refs in a map keyed by ref name. Every
// operation is safe for concurrent use, so the HTTP, git and MCP surfaces can
// share one store without extra locking at the call site.
package store

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// ObjectType is a git object type, using git's own numeric codes so a pack
// entry header can be written from the type without a translation table.
type ObjectType uint8

const (
	TypeCommit ObjectType = 1
	TypeTree   ObjectType = 2
	TypeBlob   ObjectType = 3
	TypeTag    ObjectType = 4
)

// Name returns the git name of the type ("commit", "tree", "blob", "tag").
func (t ObjectType) Name() string {
	switch t {
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	}
	return fmt.Sprintf("unknown(%d)", uint8(t))
}

// ParseObjectType parses a git type name into its code.
func ParseObjectType(name string) (ObjectType, error) {
	switch name {
	case "commit":
		return TypeCommit, nil
	case "tree":
		return TypeTree, nil
	case "blob":
		return TypeBlob, nil
	case "tag":
		return TypeTag, nil
	}
	return 0, fmt.Errorf("store: unknown object type %q", name)
}

// OID is a git object identity: the 20 raw SHA-1 bytes.
type OID [20]byte

// ZeroOID is the all-zero identity git uses for "no object".
var ZeroOID OID

// OIDFromHex parses a 40 character lowercase hex identity.
func OIDFromHex(s string) (OID, error) {
	var oid OID
	if len(s) != 40 {
		return oid, fmt.Errorf("store: oid %q is %d characters, want 40", s, len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return oid, fmt.Errorf("store: oid %q is not hex: %w", s, err)
	}
	copy(oid[:], b)
	return oid, nil
}

// Hex returns the 40 character lowercase hex form of the identity.
func (o OID) Hex() string { return hex.EncodeToString(o[:]) }

// String returns the hex form, so an OID prints readably in logs and errors.
func (o OID) String() string { return o.Hex() }

// IsZero reports whether the identity is the all-zero identity.
func (o OID) IsZero() bool { return o == ZeroOID }

// Object is one git object: its type, its raw content (without the
// "<type> <len>\0" header), and its identity, computed once at creation.
type Object struct {
	Type ObjectType
	Data []byte
	oid  OID
}

// NewObject builds an object and computes its identity from type and content.
func NewObject(typ ObjectType, data []byte) *Object {
	o := &Object{Type: typ, Data: data}
	o.oid = ComputeOID(typ, data)
	return o
}

// ComputeOID returns the identity of an object with the given type and
// content, without building the object: SHA-1 of "<type> <len>\0<content>".
func ComputeOID(typ ObjectType, data []byte) OID {
	h := sha1.New()
	h.Write([]byte(typ.Name()))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	var oid OID
	copy(oid[:], h.Sum(nil))
	return oid
}

// OID returns the object's identity.
func (o *Object) OID() OID { return o.oid }

// ID returns the object's identity in hex.
func (o *Object) ID() string { return o.oid.Hex() }

// Header returns the "<type> <len>\0" prefix git hashes and stores in front
// of the raw content in loose objects.
func (o *Object) Header() []byte {
	h := make([]byte, 0, len(o.Type.Name())+1+10+1)
	h = append(h, o.Type.Name()...)
	h = append(h, ' ')
	h = strconv.AppendInt(h, int64(len(o.Data)), 10)
	h = append(h, 0)
	return h
}

// Store is the in-memory object database: objects by identity, refs by name.
type Store struct {
	mu   sync.RWMutex
	objs map[OID]*Object
	refs map[string]OID
}

// New returns an empty store.
func New() *Store {
	return &Store{
		objs: make(map[OID]*Object),
		refs: make(map[string]OID),
	}
}

// Put stores an object under its identity. Storing the same identity twice
// keeps the first object, because content addressing makes them identical.
func (s *Store) Put(obj *Object) {
	if obj == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objs[obj.oid]; !ok {
		s.objs[obj.oid] = obj
	}
}

// Get returns the object with the given identity, if the store holds it.
func (s *Store) Get(oid OID) (*Object, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objs[oid]
	return obj, ok
}

// GetHex parses a hex identity and returns the object, if the store holds it.
func (s *Store) GetHex(hexID string) (*Object, bool) {
	oid, err := OIDFromHex(hexID)
	if err != nil {
		return nil, false
	}
	return s.Get(oid)
}

// Has reports whether the store holds an object with the given identity.
func (s *Store) Has(oid OID) bool {
	_, ok := s.Get(oid)
	return ok
}

// Count returns how many objects the store holds.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.objs)
}

// SetRef points a ref name at an identity.
func (s *Store) SetRef(name string, oid OID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[name] = oid
}

// GetRef returns the identity a ref name points at, if the ref exists.
func (s *Store) GetRef(name string) (OID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	oid, ok := s.refs[name]
	return oid, ok
}

// DeleteRef removes a ref name.
func (s *Store) DeleteRef(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refs, name)
}

// RefNames returns the ref names in sorted order, so a state export built
// from them is deterministic.
func (s *Store) RefNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.refs))
	for name := range s.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All returns every object the store holds, in no particular order. Callers
// that need a deterministic order must sort by identity themselves.
func (s *Store) All() []*Object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Object, 0, len(s.objs))
	for _, obj := range s.objs {
		out = append(out, obj)
	}
	return out
}
