// Command fakevllm runs one fake vLLM worker for the laptop demo. It serves
// /metrics and /v1/chat/completions the way internal/fakevllm does in the
// tests, and answers every run with a few read-tool calls and then an
// inconclusive diagnosis.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/fakevllm"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18001", "address to serve on")
	steps := flag.Int("steps", 3, "read-tool calls per run before it submits")
	latency := flag.Duration("latency", 300*time.Millisecond, "delay before every chat answer")
	kv := flag.Float64("kv-usage", 0.1, "KV cache usage reported on /metrics, 0..1")
	flag.Parse()

	w := fakevllm.New()
	w.Set(func(s *fakevllm.Settings) {
		s.Steps, s.Latency, s.KVUsage = *steps, *latency, *kv
	})
	fmt.Fprintf(os.Stderr, "fakevllm on %s: %d steps, %s latency\n", *listen, *steps, *latency)
	srv := &http.Server{Addr: *listen, Handler: w.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
