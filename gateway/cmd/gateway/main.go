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

	"github.com/cduggn/ariadne/gateway/internal/decide"
	"github.com/cduggn/ariadne/gateway/internal/fleet"
	"github.com/cduggn/ariadne/gateway/internal/hop"
	"github.com/cduggn/ariadne/gateway/internal/metrics"
	"github.com/cduggn/ariadne/gateway/internal/overflow"
	"github.com/cduggn/ariadne/gateway/internal/serve"
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
	hopCfg := hop.DefaultConfig
	hopOn := flag.Bool("hop", envBool("hop", false), "copy a moved run's KV to its new worker through vLLM's MooncakeConnector (workers must run it)")
	flag.IntVar(&hopCfg.MinTokens, "hop-min-tokens", envInt("hop-min-tokens", hopCfg.MinTokens), "shortest run history worth a hop, in tokens")
	flag.IntVar(&hopCfg.KVBytesPerToken, "hop-kv-bytes-per-token", envInt("hop-kv-bytes-per-token", hopCfg.KVBytesPerToken), "the served model's KV bytes per token")
	flag.Float64Var(&hopCfg.TransferBytesPerS, "hop-transfer-bytes-per-s", envFloat("hop-transfer-bytes-per-s", hopCfg.TransferBytesPerS), "measured KV copy bandwidth between two workers")
	flag.Float64Var(&hopCfg.PrefillTokensPerS, "hop-prefill-tokens-per-s", envFloat("hop-prefill-tokens-per-s", hopCfg.PrefillTokensPerS), "measured uncached prefill rate of one worker")
	flag.DurationVar(&hopCfg.Overhead, "hop-overhead", envDuration("hop-overhead", hopCfg.Overhead), "fixed cost of one hop")
	flag.DurationVar(&hopCfg.Timeout, "hop-timeout", envDuration("hop-timeout", hopCfg.Timeout), "longest wait for the old worker before recomputing instead")
	flag.DurationVar(&hopCfg.ReceiveTimeout, "hop-receive-timeout", envDuration("hop-receive-timeout", hopCfg.ReceiveTimeout), "longest wait for the new worker to answer a hopped request before sending it again without the hop")
	flag.IntVar(&hopCfg.MaxInflight, "hop-max-inflight", envInt("hop-max-inflight", hopCfg.MaxInflight), "hops running at once; past this a moved run recomputes")
	hopPort := flag.Int("hop-bootstrap-port", envInt("hop-bootstrap-port", hop.DefaultBootstrapPort), "workers' Mooncake bootstrap port (VLLM_MOONCAKE_BOOTSTRAP_PORT)")
	ovCfg := overflow.DefaultConfig
	ovOn := flag.Bool("overflow", envBool("overflow", false), "send a 503 that may leave the box to the overflow backend instead of refusing it (restricted requests never leave)")
	flag.StringVar(&ovCfg.BaseURL, "overflow-base-url", env("overflow-base-url", ovCfg.BaseURL), "overflow backend origin, without a path")
	flag.StringVar(&ovCfg.ChatPath, "overflow-chat-path", env("overflow-chat-path", ovCfg.ChatPath), "chat-completions path on the overflow backend")
	flag.StringVar(&ovCfg.ProbePath, "overflow-probe-path", env("overflow-probe-path", ovCfg.ProbePath), "path the health probe fetches on the overflow backend; any 2xx passes")
	flag.StringVar(&ovCfg.Model, "overflow-model", env("overflow-model", ""), "model name sent to the overflow backend; empty forwards the client's")
	ovKeyEnv := flag.String("overflow-api-key-env", env("overflow-api-key-env", "GW_OVERFLOW_API_KEY"), "environment variable holding the overflow backend's bearer key")
	flag.DurationVar(&ovCfg.Timeout, "overflow-timeout", envDuration("overflow-timeout", ovCfg.Timeout), "longest wait for the overflow backend before answering the 503")
	flag.DurationVar(&ovCfg.ProbeInterval, "overflow-probe-interval", envDuration("overflow-probe-interval", ovCfg.ProbeInterval), "how often the overflow backend is re-probed")
	flag.IntVar(&ovCfg.MaxInflight, "overflow-max-inflight", envInt("overflow-max-inflight", ovCfg.MaxInflight), "overflow calls running at once; past this a 503 stays a 503")
	flag.Parse()
	hopCfg.SharedPrefixTokens = *sharedPrefix

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
	opts := serve.Options{Log: log, OnRequest: m.Observe, Metrics: m.Handler()}
	if *hopOn {
		hopper, err := newHopper(hopCfg, urls, *hopPort)
		if err != nil {
			fatal(err.Error())
		}
		opts.Hop = hopper
		workersLoop.OnNotReady(hopper.Forget) // a worker that left Ready or restarted has a new engine id
		log.Info("kv hop on", "min_tokens", hopCfg.MinTokens, "kv_bytes_per_token", hopCfg.KVBytesPerToken,
			"transfer_bytes_per_s", hopCfg.TransferBytesPerS, "prefill_tokens_per_s", hopCfg.PrefillTokensPerS,
			"overhead", hopCfg.Overhead.String(), "timeout", hopCfg.Timeout.String(),
			"receive_timeout", hopCfg.ReceiveTimeout.String(), "max_inflight", hopCfg.MaxInflight,
			"bootstrap_port", *hopPort)
	}
	var ov *overflow.Overflow
	if *ovOn {
		key := os.Getenv(*ovKeyEnv) // never logged
		backend, err := overflow.NewSuperlinked(ovCfg, key, &http.Client{})
		if err != nil {
			fatal(err.Error())
		}
		ov = overflow.New(backend, ovCfg.ProbeInterval, ovCfg.MaxInflight, time.Now, log)
		opts.Overflow = ov
		m.WatchOverflow(ov)
		if key == "" {
			log.Warn("overflow key is empty; the backend stays unavailable until a probe passes", "key_env", *ovKeyEnv)
		}
		log.Info("overflow on", "backend", ov.Name(), "base_url", ovCfg.BaseURL, "chat_path", ovCfg.ChatPath,
			"probe_path", ovCfg.ProbePath, "model", ovCfg.Model, "timeout", ovCfg.Timeout.String(),
			"probe_interval", ovCfg.ProbeInterval.String(), "max_inflight", ovCfg.MaxInflight,
			"key_env", *ovKeyEnv, "key_set", key != "")
	}
	server := serve.New(gate, workersLoop, urls, opts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fleetDone := make(chan struct{})
	go func() {
		defer close(fleetDone)
		workersLoop.Run(ctx)
	}()
	overflowDone := make(chan struct{})
	go func() {
		defer close(overflowDone)
		if ov != nil {
			ov.Run(ctx) // probes at start and every interval; a failed probe never stops the gateway
		}
	}()

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      125 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second) // ctx is already done; keep its values, not its cancellation
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Warn("shutdown", "err", err)
		}
	}()

	log.Info("gateway listening", "addr", *listen, "pods", pods, "policy", string(pick),
		"max_inflight", *maxInflight, "max_queued", *maxQueued, "pool", *pool, "shared_prefix_tokens", *sharedPrefix)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fatal("serve: " + err.Error())
	}
	stop()
	<-fleetDone
	<-overflowDone
	log.Info("gateway stopped")
}

// newHopper builds the KV hop over the workers, each reached for Mooncake on
// its own host at the bootstrap port.
func newHopper(cfg hop.Config, urls map[string]string, port int) (*hop.Hopper, error) {
	endpoints := make(map[string]hop.Endpoint, len(urls))
	for pod, base := range urls {
		bootstrap, err := hop.BootstrapURL(base, port)
		if err != nil {
			return nil, fmt.Errorf("hop endpoint for %s: %w", pod, err)
		}
		endpoints[pod] = hop.Endpoint{BaseURL: strings.TrimSuffix(base, "/"), BootstrapURL: bootstrap}
	}
	return hop.New(cfg, endpoints, &http.Client{}, time.Now)
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

// envBool is env for a boolean flag, accepting what strconv.ParseBool does.
func envBool(flagName string, def bool) bool {
	v, ok := os.LookupEnv(envName(flagName))
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		fatal(envName(flagName) + "=" + strconv.Quote(v) + " is not a boolean")
	}
	return b
}

// envFloat is env for a float flag.
func envFloat(flagName string, def float64) float64 {
	v, ok := os.LookupEnv(envName(flagName))
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		fatal(envName(flagName) + "=" + strconv.Quote(v) + " is not a number")
	}
	return f
}

// envDuration is env for a duration flag, in time.ParseDuration's form.
func envDuration(flagName string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(envName(flagName))
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fatal(envName(flagName) + "=" + strconv.Quote(v) + " is not a duration")
	}
	return d
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "gateway: "+msg)
	os.Exit(2)
}
