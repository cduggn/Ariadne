// Command gateway is cluster-doctor's inference gateway. It guards, admits,
// places and queues chat completions in front of a fixed set of vLLM
// workers and forwards each body unchanged. Every flag reads its default
// from the environment variable GW_<NAME> with the name upper-cased and
// dashes turned into underscores.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
	"github.com/cduggn/cluster-doctor/gateway/internal/fleet"
	"github.com/cduggn/cluster-doctor/gateway/internal/metrics"
	"github.com/cduggn/cluster-doctor/gateway/internal/serve"
)

const defaultWorkers = "vllm-0=http://vllm-0.vllm.default.svc.cluster.local:8000," +
	"vllm-1=http://vllm-1.vllm.default.svc.cluster.local:8000"

func main() {
	listen := flag.String("listen", env("listen", ":8000"), "address to serve on")
	workers := flag.String("workers", env("workers", defaultWorkers), "comma-separated pod=url list of vLLM workers")
	policy := flag.String("policy", env("policy", string(decide.PolicyPrefixThenLoad)), "pick policy: prefix_then_load, least_loaded or p2c")
	maxInflight := flag.Int("max-inflight", envInt("max-inflight", 16), "in-flight requests per worker")
	maxQueued := flag.Int("max-queued", envInt("max-queued", 32), "queued requests per worker")
	warmBody := flag.String("warm-body", env("warm-body", ""), "path to the recorded step-1 request JSON that warm-up probes replay (required)")
	pool := flag.String("pool", env("pool", "sliced"), "pool label on the replica metrics")
	sharedPrefix := flag.Int("shared-prefix-tokens", envInt("shared-prefix-tokens", 3899), "tokens every prompt shares, which split cached tokens into shared_hit and run_hit")
	logLevel := flag.String("log-level", env("log-level", "info"), "log level: debug, info, warn or error")
	flag.Parse()

	urls, err := parseWorkers(*workers)
	if err != nil {
		fatal(err.Error())
	}
	pick, ok := decide.ParsePickPolicy(*policy)
	if !ok {
		fatal("unknown policy " + strconv.Quote(*policy))
	}
	if *warmBody == "" {
		fatal("--warm-body is required")
	}
	warm, err := os.ReadFile(*warmBody)
	if err != nil {
		fatal("read warm body: " + err.Error())
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("bad log level " + strconv.Quote(*logLevel))
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	pods := make([]string, 0, len(urls))
	for pod := range urls {
		pods = append(pods, pod)
	}
	sort.Strings(pods)
	cfg := fleet.DefaultConfig(pods)
	cfg.Policy = pick
	cfg.MaxInflight = *maxInflight
	cfg.MaxQueued = *maxQueued
	gate := fleet.NewGate(cfg, time.Now, rand.IntN)
	workersLoop := fleet.NewFleet(gate, urls, fleet.DefaultWorkerConfig(warm), nil, time.Now)
	m := metrics.New(gate, workersLoop, *pool, *sharedPrefix)
	server := serve.New(gate, workersLoop, urls, serve.Options{Log: log, OnRequest: m.Observe, Metrics: m.Handler()})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fleetDone := make(chan struct{})
	go func() {
		defer close(fleetDone)
		workersLoop.Run(ctx)
	}()

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      125 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	log.Info("gateway listening", "addr", *listen, "pods", pods, "policy", string(pick),
		"max_inflight", *maxInflight, "max_queued", *maxQueued, "pool", *pool, "shared_prefix_tokens", *sharedPrefix)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fatal("serve: " + err.Error())
	}
	stop()
	<-fleetDone
	log.Info("gateway stopped")
}

// parseWorkers reads "pod=url,pod=url" into a map. A missing "=" or a
// repeated pod is an error.
func parseWorkers(s string) (map[string]string, error) {
	urls := make(map[string]string)
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		pod, url, ok := strings.Cut(item, "=")
		if !ok || pod == "" || url == "" {
			return nil, fmt.Errorf("bad worker %q, want pod=url", item)
		}
		if _, dup := urls[pod]; dup {
			return nil, fmt.Errorf("worker %q listed twice", pod)
		}
		urls[pod] = url
	}
	if len(urls) == 0 {
		return nil, errors.New("no workers configured")
	}
	return urls, nil
}

// envName maps a flag name to its environment variable, GW_<NAME>.
func envName(flagName string) string {
	return "GW_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// env returns the flag's environment default, or def when it is unset.
func env(flagName, def string) string {
	if v, ok := os.LookupEnv(envName(flagName)); ok {
		return v
	}
	return def
}

// envInt is env for an integer flag. A set but unparsable value fails at
// startup rather than silently taking the default.
func envInt(flagName string, def int) int {
	v, ok := os.LookupEnv(envName(flagName))
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fatal(envName(flagName) + "=" + strconv.Quote(v) + " is not an integer")
	}
	return n
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "gateway: "+msg)
	os.Exit(2)
}
