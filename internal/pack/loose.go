package pack

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"

	"forge/internal/store"
)

// WriteLoose encodes an object the way git stores it loose on disk: the
// zlib stream of "<type> <len>\0<content>".
func WriteLoose(obj *store.Object) ([]byte, error) {
	if obj == nil {
		return nil, fmt.Errorf("pack: cannot write a nil loose object")
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(obj.Header()); err != nil {
		return nil, fmt.Errorf("pack: compressing loose header for object %s: %w", obj.ID(), err)
	}
	if _, err := zw.Write(obj.Data); err != nil {
		return nil, fmt.Errorf("pack: compressing loose content for object %s: %w", obj.ID(), err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("pack: closing loose stream for object %s: %w", obj.ID(), err)
	}
	return buf.Bytes(), nil
}

// ReadLoose decodes a loose object: inflate, split the "<type> <len>\0"
// header from the content, check the length, and return the object with its
// identity computed from what was actually read.
func ReadLoose(data []byte) (*store.Object, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("pack: loose object did not open: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("pack: loose object did not inflate: %w", err)
	}
	nul := bytes.IndexByte(raw, 0)
	if nul < 0 {
		return nil, fmt.Errorf("pack: loose object has no header terminator")
	}
	var typName string
	var size int
	if _, err := fmt.Sscanf(string(raw[:nul]), "%s %d", &typName, &size); err != nil {
		return nil, fmt.Errorf("pack: loose object header %q did not parse: %w", string(raw[:nul]), err)
	}
	typ, err := store.ParseObjectType(typName)
	if err != nil {
		return nil, fmt.Errorf("pack: %w", err)
	}
	content := raw[nul+1:]
	if len(content) != size {
		return nil, fmt.Errorf("pack: loose object header declares %d bytes but holds %d", size, len(content))
	}
	return store.NewObject(typ, content), nil
}
