// Package rawhttp serves an http.Handler over hand-rolled HTTP/1.1 on raw
// TCP. It still uses Go's netpoller for accept/read/write readiness — no
// raw syscalls and no epoll bypass — but it replaces the net/http server
// machinery (bufio, per-request header maps, chunked response encoding,
// Date header) with a single read buffer and a single write per request
// on a keep-alive connection.
//
// The handlers never see the difference: each request is presented as a
// real *http.Request (method, URL path and query, headers, body) and each
// reply goes through a real http.ResponseWriter. The only observable
// differences from net/http are the absence of the automatic Date header
// and of Transfer-Encoding: chunked on replies (replies are always
// Content-Length framed, because the whole body is buffered before the
// single write).
package rawhttp

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// maxRequestBody is the largest request body the server reads, matching the
// git surface's cap.
const maxRequestBody = 64 << 20

// Serve accepts connections on ln and serves handler on each of them until
// the listener closes.
func Serve(ln net.Listener, handler http.Handler) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go ServeConn(conn, handler)
	}
}

// ServeConn serves handler on a single connection until it closes or the
// client sends a request that ends the keep-alive session.
func ServeConn(conn net.Conn, handler http.Handler) {
	defer conn.Close()
	st := &connState{
		conn:       conn,
		handler:    handler,
		buf:        make([]byte, 32768),
		reqHeader:  make(http.Header, 8),
		respHeader: make(http.Header, 8),
	}
	st.resp.header = st.respHeader
	for {
		req, keepAlive, err := st.readRequest()
		if err != nil {
			return
		}
		st.dispatch(req)
		if !keepAlive {
			return
		}
	}
}

type connState struct {
	conn   net.Conn
	handler http.Handler
	// read buffer: holds bytes not yet consumed by the parser. Grows as
	// needed for large bodies and shrinks back only when idle.
	buf  []byte
	used int // valid bytes in buf[:used]
	// per-request scratch, reused across keep-alive requests on this
	// connection to avoid per-request allocation. The request and
	// response headers are separate maps: dispatch clears the response
	// map, never the request map.
	req        http.Request
	reqHeader  http.Header
	respHeader http.Header
	body       sliceReader
	resp       responseWriter
	// write buffer: the whole reply (headers + body) is built here and
	// flushed with one Write.
	wbuf []byte
}

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

type responseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        []byte
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.body = append(w.body, p...)
	return len(p), nil
}


// readRequest parses one request from the connection. It returns the
// request, whether the connection stays open afterwards, and an error when
// the connection is dead or the request is malformed.
func (st *connState) readRequest() (*http.Request, bool, error) {
	// Find the end of the header block.
	for {
		if idx := bytes.Index(st.buf[:st.used], []byte("\r\n\r\n")); idx >= 0 {
			return st.parseRequest(idx)
		}
		if st.used == len(st.buf) {
			if len(st.buf) >= maxRequestBody+65536 {
				return nil, false, io.ErrUnexpectedEOF
			}
			next := make([]byte, len(st.buf)*2)
			copy(next, st.buf[:st.used])
			st.buf = next
		}
		n, err := st.conn.Read(st.buf[st.used:])
		if err != nil {
			return nil, false, err
		}
		st.used += n
	}
}

// parseRequest parses the request whose headers end at headerEnd (the index
// of "\r\n\r\n" in st.buf) and reads its body.
func (st *connState) parseRequest(headerEnd int) (*http.Request, bool, error) {
	head := st.buf[:headerEnd]
	// Request line: METHOD SP REQUEST-TARGET SP HTTP/x.y
	lineEnd := bytes.IndexByte(head, '\n')
	if lineEnd < 0 {
		return nil, false, errMalformed
	}
	line := head[:lineEnd]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	sp1 := bytes.IndexByte(line, ' ')
	sp2 := bytes.LastIndexByte(line, ' ')
	if sp1 < 0 || sp2 <= sp1 {
		return nil, false, errMalformed
	}
	method := methodString(line[:sp1])
	target := line[sp1+1 : sp2]
	version := line[sp2+1:]
	keepAlive := true
	if bytes.Equal(version, []byte("HTTP/1.0")) {
		keepAlive = false
	} else if !bytes.Equal(version, []byte("HTTP/1.1")) {
		return nil, false, errMalformed
	}

	// Split the target into path and query. These slices alias the read
	// buffer, so materialise them as strings NOW: the buffer is compacted
	// below, which would corrupt any slice still pointing into it.
	path := target
	var rawQuery []byte
	if q := bytes.IndexByte(target, '?'); q >= 0 {
		path = target[:q]
		rawQuery = target[q+1:]
	}
	pathStr := decodePath(path)
	rawQueryStr := string(rawQuery)

	// Headers: one line each, "Name: value". Only the headers the
	// handlers read are kept, plus every other header verbatim, so no
	// handler can observe a difference from net/http.
	clear(st.reqHeader)
	rest := head[lineEnd+1:]
	contentLength := -1
	chunked := false
	expectContinue := false
	for len(rest) > 0 {
		// The last header line has no trailing newline inside head: head
		// stops at the "\r\n\r\n" boundary, so treat a missing newline as
		// the end of the final line instead of dropping it.
		eol := bytes.IndexByte(rest, '\n')
		var hline []byte
		if eol < 0 {
			hline = rest
			rest = nil
		} else {
			hline = rest[:eol]
			rest = rest[eol+1:]
		}
		if len(hline) > 0 && hline[len(hline)-1] == '\r' {
			hline = hline[:len(hline)-1]
		}
		if len(hline) == 0 {
			continue
		}
		colon := bytes.IndexByte(hline, ':')
		if colon <= 0 {
			continue
		}
		name := canonicalHeaderName(hline[:colon])
		value := bytes.TrimLeft(hline[colon+1:], " \t")
		st.reqHeader[name] = append(st.reqHeader[name], string(value))
		switch name {
		case "Content-Length":
			if n, err := strconv.Atoi(string(value)); err == nil {
				contentLength = n
			}
		case "Transfer-Encoding":
			if bytes.EqualFold(value, []byte("chunked")) {
				chunked = true
			}
		case "Expect":
			if bytes.EqualFold(value, []byte("100-continue")) {
				expectContinue = true
			}
		case "Connection":
			if bytes.EqualFold(value, []byte("close")) {
				keepAlive = false
			} else if bytes.EqualFold(value, []byte("keep-alive")) {
				keepAlive = true
			}
		}
	}

	// Consume the header block from the buffer.
	consumed := headerEnd + 4
	copy(st.buf, st.buf[consumed:st.used])
	st.used -= consumed

	if expectContinue {
		_, _ = st.conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}

	// Read the body.
	var body []byte
	if chunked {
		var err error
		body, err = st.readChunkedBody()
		if err != nil {
			return nil, false, err
		}
	} else if contentLength > 0 {
		if contentLength > maxRequestBody {
			return nil, false, errTooLarge
		}
		body = make([]byte, contentLength)
		got := copy(body, st.buf[:st.used])
		copy(st.buf, st.buf[got:st.used])
		st.used -= got
		for got < contentLength {
			n, err := st.conn.Read(body[got:])
			if err != nil {
				return nil, false, err
			}
			got += n
		}
	} else {
		body = nil
	}

	// Build the *http.Request the handlers expect.
	st.req = http.Request{}
	st.req.Method = method
	if debugRawHTTP {
		fmt.Fprintf(os.Stderr, "[rawhttp-debug] method=%q path=%q rawQuery=%q body=%q\n", method, pathStr, rawQueryStr, string(body))
	}
	st.req.Proto = "HTTP/1.1"
	st.req.ProtoMajor = 1
	st.req.ProtoMinor = 1
	st.req.Header = st.reqHeader
	st.req.Host = st.reqHeader.Get("Host")
	st.req.URL = &url.URL{Path: pathStr, RawQuery: rawQueryStr}
	st.req.ContentLength = int64(len(body))
	st.body.b = body
	st.body.i = 0
	st.req.Body = io.NopCloser(&st.body)
	return &st.req, keepAlive, nil
}

// readChunkedBody decodes a chunked request body. The buffer already holds
// whatever the connection delivered after the headers.
func (st *connState) readChunkedBody() ([]byte, error) {
	var out []byte
	for {
		line, err := st.readLine()
		if err != nil {
			return nil, err
		}
		// Chunk size line: hex, optionally followed by ";extension".
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size, err := strconv.ParseInt(string(bytes.TrimSpace(line)), 16, 64)
		if err != nil || size < 0 {
			return nil, errMalformed
		}
		if size == 0 {
			// Consume the trailer section (lines until an empty line).
			for {
				trailer, err := st.readLine()
				if err != nil {
					return nil, err
				}
				if len(trailer) == 0 {
					break
				}
			}
			return out, nil
		}
		if int64(len(out))+size > maxRequestBody {
			return nil, errTooLarge
		}
		chunk := make([]byte, size)
		got := copy(chunk, st.buf[:st.used])
		copy(st.buf, st.buf[got:st.used])
		st.used -= got
		for got < int(size) {
			n, err := st.conn.Read(chunk[got:])
			if err != nil {
				return nil, err
			}
			got += n
		}
		out = append(out, chunk...)
		// Consume the CRLF after the chunk.
		if _, err := st.readLine(); err != nil {
			return nil, err
		}
	}
}

// readLine reads one CRLF-terminated line from the buffer, refilling from
// the connection as needed. The returned line excludes the CRLF. It is a
// copy: the buffer is compacted as the line is consumed, which would
// corrupt a slice still pointing into it.
func (st *connState) readLine() ([]byte, error) {
	for {
		if idx := bytes.Index(st.buf[:st.used], []byte("\r\n")); idx >= 0 {
			line := append([]byte(nil), st.buf[:idx]...)
			copy(st.buf, st.buf[idx+2:st.used])
			st.used -= idx + 2
			return line, nil
		}
		if st.used == len(st.buf) {
			if len(st.buf) >= maxRequestBody+65536 {
				return nil, errTooLarge
			}
			next := make([]byte, len(st.buf)*2)
			copy(next, st.buf[:st.used])
			st.buf = next
		}
		n, err := st.conn.Read(st.buf[st.used:])
		if err != nil {
			return nil, err
		}
		st.used += n
	}
}

// dispatch runs the handler for one request and writes the reply with a
// single Write call.
func (st *connState) dispatch(req *http.Request) {
	// Reset the response state, reusing the response header map and body
	// buffer. The request header map is untouched.
	clear(st.respHeader)
	st.resp.header = st.respHeader
	st.resp.status = 0
	st.resp.wroteHeader = false
	st.resp.body = st.resp.body[:0]

	st.handler.ServeHTTP(&st.resp, req)

	status := st.resp.status
	if status == 0 {
		status = http.StatusOK
	}
	// Build the reply: status line, headers, blank line, body.
	wbuf := st.wbuf[:0]
	wbuf = append(wbuf, "HTTP/1.1 "...)
	wbuf = append(wbuf, strconv.Itoa(status)...)
	wbuf = append(wbuf, ' ')
	wbuf = append(wbuf, reasonPhrase(status)...)
	wbuf = append(wbuf, "\r\n"...)
	lengthSet := false
	for name, values := range st.resp.header {
		if strings.EqualFold(name, "Content-Length") {
			lengthSet = true
		}
		for _, v := range values {
			wbuf = append(wbuf, name...)
			wbuf = append(wbuf, ": "...)
			wbuf = append(wbuf, v...)
			wbuf = append(wbuf, "\r\n"...)
		}
	}
	if !lengthSet {
		wbuf = append(wbuf, "Content-Length: "...)
		wbuf = append(wbuf, strconv.Itoa(len(st.resp.body))...)
		wbuf = append(wbuf, "\r\n"...)
	}
	wbuf = append(wbuf, "\r\n"...)
	wbuf = append(wbuf, st.resp.body...)
	st.wbuf = wbuf
	_, _ = st.conn.Write(wbuf)
}

// methodString interns the methods the suite uses so no string is
// allocated for them.
func methodString(b []byte) string {
	switch string(b) {
	case "GET":
		return "GET"
	case "POST":
		return "POST"
	case "PUT":
		return "PUT"
	case "DELETE":
		return "DELETE"
	case "PATCH":
		return "PATCH"
	case "HEAD":
		return "HEAD"
	default:
		return string(b)
	}
}

// canonicalHeaderName canonicalises a header name, fast-pathing the ones
// this server sees.
func canonicalHeaderName(b []byte) string {
	switch {
	case bytes.EqualFold(b, []byte("authorization")):
		return "Authorization"
	case bytes.EqualFold(b, []byte("content-type")):
		return "Content-Type"
	case bytes.EqualFold(b, []byte("content-length")):
		return "Content-Length"
	case bytes.EqualFold(b, []byte("git-protocol")):
		return "Git-Protocol"
	case bytes.EqualFold(b, []byte("host")):
		return "Host"
	case bytes.EqualFold(b, []byte("connection")):
		return "Connection"
	case bytes.EqualFold(b, []byte("transfer-encoding")):
		return "Transfer-Encoding"
	case bytes.EqualFold(b, []byte("expect")):
		return "Expect"
	case bytes.EqualFold(b, []byte("accept")):
		return "Accept"
	case bytes.EqualFold(b, []byte("user-agent")):
		return "User-Agent"
	case bytes.EqualFold(b, []byte("link")):
		return "Link"
	default:
		return textproto.CanonicalMIMEHeaderKey(string(b))
	}
}

// decodePath percent-decodes a request path. It allocates only when the
// path actually contains a '%' escape.
func decodePath(path []byte) string {
	if bytes.IndexByte(path, '%') < 0 {
		return string(path)
	}
	decoded, err := url.PathUnescape(string(path))
	if err != nil {
		return string(path)
	}
	return decoded
}

func reasonPhrase(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	default:
		return ""
	}
}

var (
	errMalformed = &httpError{"malformed request"}
	errTooLarge  = &httpError{"request too large"}
)

// debugRawHTTP enables stderr logging of parsed requests. It is only set
// by tests via the RAWHTTP_DEBUG environment variable.
var debugRawHTTP = os.Getenv("RAWHTTP_DEBUG") != ""

type httpError struct{ msg string }

func (e *httpError) Error() string { return e.msg }
