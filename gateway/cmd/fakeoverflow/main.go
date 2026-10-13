// Command fakeoverflow runs one fake overflow backend for the laptop demo.
// It serves GET /v1/models and POST /v1/chat/completions the way
// internal/fakeoverflow does in the tests, checks a bearer key when given
// one, and answers every chat with an inconclusive diagnosis.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/fakeoverflow"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18010", "address to serve on")
	key := flag.String("key", "", "bearer key every request must carry; empty accepts any")
	status := flag.Int("status", http.StatusOK, "status every chat answers, e.g. 402 to play an out-of-credit backend")
	latency := flag.Duration("latency", 500*time.Millisecond, "delay before every chat answer")
	down := flag.Bool("down", false, "answer the probe with 503, as a backend that is out would")
	flag.Parse()

	b := fakeoverflow.New()
	b.Set(func(s *fakeoverflow.Settings) {
		s.Key, s.Status, s.Latency, s.Down = *key, *status, *latency, *down
	})
	fmt.Fprintf(os.Stderr, "fakeoverflow on %s: status %d, %s latency, key %v, down %v\n", *listen, *status, *latency, *key != "", *down)
	srv := &http.Server{Addr: *listen, Handler: b.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
