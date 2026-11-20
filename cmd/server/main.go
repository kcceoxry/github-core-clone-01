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
	"runtime"
	"runtime/pprof"
	"strconv"
	"syscall"

	"forge/internal/gitproto"
	"forge/internal/rawhttp"
	"forge/internal/server"
)

func main() {
	portFlag := flag.Int("port", 0, "TCP port to listen on (0 picks a free port)")
	transportFlag := flag.String("transport", "raw", "HTTP transport: raw (hand-rolled HTTP/1.1) or std (net/http)")
	profileFlag := flag.String("cpuprofile", "", "write a CPU profile to this file (for PGO)")
	flag.Parse()

	// The trial pod has 2 CPUs; pin the scheduler to match it.
	runtime.GOMAXPROCS(2)

	if *profileFlag != "" {
		f, err := os.Create(*profileFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[forge] cpuprofile: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		_ = pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	port := *portFlag
	if port == 0 {
		if text := os.Getenv("FORGE_PORT"); text != "" {
			if parsed, err := strconv.Atoi(text); err == nil {
				port = parsed
			}
		}
	}

	forge := gitproto.NewForge()

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", actualPort)

	handler := server.New(forge)
	serveErr := make(chan error, 1)
	go func() {
		switch *transportFlag {
		case "std":
			serveErr <- http.Serve(listener, handler)
		default:
			serveErr <- rawhttp.Serve(listener, handler)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
		_ = listener.Close()
	case err := <-serveErr:
		if err != nil {
			fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
			os.Exit(1)
		}
	}
}
