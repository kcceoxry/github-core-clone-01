// Command forge-server is the forge's HTTP server: the git smart-HTTP
// surface, the health probe, and the repository creation stub. It binds an
// ephemeral port (or the port in FORGE_PORT / --port), prints the port it
// bound on stdout for the Node launcher to read, and serves until it is
// killed. It writes nothing to disk and shells out to nothing: the trial
// pod has no git binary, and this server never needs one.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"forge/internal/gitproto"
	"forge/internal/server"
)

func main() {
	portFlag := flag.Int("port", 0, "TCP port to listen on (0 picks a free port)")
	flag.Parse()

	port := *portFlag
	if port == 0 {
		if text := os.Getenv("FORGE_PORT"); text != "" {
			if parsed, err := strconv.Atoi(text); err == nil {
				port = parsed
			}
		}
	}

	forge := gitproto.NewForge()
	if owner := os.Getenv("FORGE_TOKEN_OWNER"); owner != "" {
		forge.TokenOwner = owner
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", actualPort)

	server := &http.Server{Handler: server.New(forge)}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	_ = server.Close()
}
