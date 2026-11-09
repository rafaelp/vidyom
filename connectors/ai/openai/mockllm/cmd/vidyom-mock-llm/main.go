// Command vidyom-mock-llm serves the MOCK OpenAI-compatible endpoint of package mockllm
// on a loopback port, for the demo and E2E tests (I12, I217). It understands nothing.
// Configure it as a local-class endpoint with baseUrl http://127.0.0.1:<port>/v1.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/rafaelp/vidyom/connectors/ai/openai/mockllm"
)

func main() {
	address := flag.String("listen", "127.0.0.1:0", "loopback address to listen on")
	anyInterface := flag.Bool("any-interface", false, "allow a non-loopback address, for a private Compose network only")
	flag.Parse()

	host, _, err := net.SplitHostPort(*address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vidyom-mock-llm: invalid --listen address")
		os.Exit(2)
	}
	if ip := net.ParseIP(host); !*anyInterface && (ip == nil || !ip.IsLoopback()) {
		fmt.Fprintln(os.Stderr, "vidyom-mock-llm: --listen must be a loopback IP address")
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vidyom-mock-llm:", err)
		os.Exit(1)
	}
	fmt.Printf("vidyom-mock-llm: MOCK LLM listening on http://%s/v1\n", listener.Addr())
	server := &http.Server{Handler: &mockllm.Server{}, ReadHeaderTimeout: 10 * time.Second}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "vidyom-mock-llm:", err)
		os.Exit(1)
	}
}
