package rawhttp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// echoHandler answers with the request's method, path, query, one header
// and the body, so tests can verify what the shim presented to the handler.
func echoHandler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
	fmt.Fprintf(w, "%s|%s|%s|%s", r.Method, r.URL.Path, r.URL.Query().Get("q"), string(body))
}

func dialServe(t *testing.T, handler http.Handler) (net.Conn, *bufio.Reader) {
	t.Helper()
	server, client := net.Pipe()
	go ServeConn(server, handler)
	return client, bufio.NewReader(client)
}

func readResponse(t *testing.T, rd *bufio.Reader) (status string, headers map[string]string, body string) {
	t.Helper()
	resp, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	headers = make(map[string]string)
	for k, v := range resp.Header {
		headers[k] = strings.Join(v, ",")
	}
	return resp.Status, headers, string(text)
}

func TestKeepAliveTwoRequests(t *testing.T) {
	client, rd := dialServe(t, http.HandlerFunc(echoHandler))
	defer client.Close()
	fmt.Fprint(client, "GET /x?q=1 HTTP/1.1\r\nHost: h\r\nAuthorization: Bearer tok\r\n\r\n")
	status, headers, body := readResponse(t, rd)
	if status != "200 OK" || body != "GET|/x|1|" {
		t.Fatalf("first: status=%q body=%q", status, body)
	}
	if headers["X-Echo-Auth"] != "Bearer tok" {
		t.Fatalf("auth header not passed: %v", headers)
	}
	fmt.Fprint(client, "POST /y?q=2 HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\nhello")
	status, _, body = readResponse(t, rd)
	if status != "200 OK" || body != "POST|/y|2|hello" {
		t.Fatalf("second: status=%q body=%q", status, body)
	}
}

func TestPipelinedRequests(t *testing.T) {
	client, rd := dialServe(t, http.HandlerFunc(echoHandler))
	defer client.Close()
	fmt.Fprint(client, "GET /a HTTP/1.1\r\nHost: h\r\n\r\nGET /b HTTP/1.1\r\nHost: h\r\n\r\n")
	_, _, body := readResponse(t, rd)
	if body != "GET|/a||" {
		t.Fatalf("first pipelined: %q", body)
	}
	_, _, body = readResponse(t, rd)
	if body != "GET|/b||" {
		t.Fatalf("second pipelined: %q", body)
	}
}

func TestChunkedBody(t *testing.T) {
	client, rd := dialServe(t, http.HandlerFunc(echoHandler))
	defer client.Close()
	fmt.Fprint(client, "POST /c HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n")
	_, _, body := readResponse(t, rd)
	if body != "POST|/c||hello" {
		t.Fatalf("chunked: %q", body)
	}
}

func TestConnectionClose(t *testing.T) {
	client, rd := dialServe(t, http.HandlerFunc(echoHandler))
	defer client.Close()
	fmt.Fprint(client, "GET /d HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")
	_, _, body := readResponse(t, rd)
	if body != "GET|/d||" {
		t.Fatalf("close: %q", body)
	}
	// The server must have closed the connection after the reply.
	client.SetReadDeadline(time.Now().Add(time.Second))
	_, err := rd.ReadByte()
	if err == nil {
		t.Fatalf("expected connection close, got more data")
	}
}

func TestSplitReads(t *testing.T) {
	client, rd := dialServe(t, http.HandlerFunc(echoHandler))
	defer client.Close()
	// Send the request one byte at a time: the parser must handle partial
	// reads without losing or corrupting data.
	req := "GET /e?q=9 HTTP/1.1\r\nHost: h\r\n\r\n"
	for i := 0; i < len(req); i++ {
		if _, err := client.Write([]byte{req[i]}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_, _, body := readResponse(t, rd)
	if body != "GET|/e|9|" {
		t.Fatalf("split: %q", body)
	}
}
