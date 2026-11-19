package store

import (
	"sync"
	"testing"
)

func TestPutGet(t *testing.T) {
	s := New()
	obj := NewObject(TypeBlob, []byte("hello\n"))
	if s.Has(obj.OID()) {
		t.Fatal("Has before Put")
	}
	s.Put(obj)
	got, ok := s.Get(obj.OID())
	if !ok || got != obj {
		t.Fatal("Get after Put did not return the object")
	}
	if s.Count() != 1 {
		t.Fatalf("Count = %d, want 1", s.Count())
	}
	s.Put(obj)
	if s.Count() != 1 {
		t.Fatalf("Count after duplicate Put = %d, want 1", s.Count())
	}
	got, ok = s.GetHex(obj.ID())
	if !ok || got != obj {
		t.Fatal("GetHex did not return the object")
	}
	if _, ok := s.GetHex("not hex"); ok {
		t.Fatal("GetHex(bad hex) returned an object")
	}
	if _, ok := s.GetHex("00"); ok {
		t.Fatal("GetHex(short hex) returned an object")
	}
}

func TestRefs(t *testing.T) {
	s := New()
	oid, _ := OIDFromHex("ce013625030ba8dba906f756967f9e9ca394464a")
	s.SetRef("refs/heads/main", oid)
	got, ok := s.GetRef("refs/heads/main")
	if !ok || got != oid {
		t.Fatal("GetRef did not return the set identity")
	}
	names := s.RefNames()
	if len(names) != 1 || names[0] != "refs/heads/main" {
		t.Fatalf("RefNames = %v", names)
	}
	s.DeleteRef("refs/heads/main")
	if _, ok := s.GetRef("refs/heads/main"); ok {
		t.Fatal("GetRef after DeleteRef still returns the ref")
	}
}

func TestConcurrent(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				obj := NewObject(TypeBlob, []byte{byte(w), byte(i)})
				s.Put(obj)
				s.Get(obj.OID())
				s.SetRef("refs/heads/b", obj.OID())
				s.GetRef("refs/heads/b")
			}
		}(w)
	}
	wg.Wait()
	if s.Count() == 0 {
		t.Fatal("store is empty after concurrent puts")
	}
}
