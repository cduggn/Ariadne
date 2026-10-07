package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Phase is where a worker is in its life as the fleet sees it. A worker
// starts Down, becomes Warming on its first successful scrape, Ready once
// warm-up passes, and Down again after DownAfter without a scrape. Only
// the Gate's ready flag follows it, which is set on Ready and cleared on
// Down.
type Phase uint8

const (
	Down Phase = iota
	Warming
	Ready
)

var phaseNames = [...]string{"down", "warming", "ready"}

func (p Phase) String() string {
	if int(p) < len(phaseNames) {
		return phaseNames[p]
	}
	return "phase(" + strconv.Itoa(int(p)) + ")"
}

// event is what moved a worker. scraped is a successful scrape, warmed is
// WarmPasses consecutive probe passes, and stale is DownAfter without a
// successful scrape.
type event uint8

const (
	scraped event = iota
	warmed
	stale
)

// step is the worker's transition table. Every pair not listed keeps its
// phase, so a stale Down worker or a scraped Ready worker is a no-op.
func step(p Phase, e event) Phase {
	switch {
	case p == Down && e == scraped:
		return Warming
	case p == Warming && e == warmed:
		return Ready
	case p != Down && e == stale:
		return Down
	}
	return p
}

// WorkerConfig shapes every per-pod loop. Interval is the scrape period and
// ScrapeTimeout bounds one scrape. DownAfter is how long a worker may go
// without a successful scrape before it is Down. Window is how far back the
// TTFT quantiles look, and RisingOver how far back WaitingRising compares.
// WarmBody is the recorded step-1 request a warm-up probe replays. A probe
// passes when it answers 200 under WarmUnder, and WarmPasses consecutive
// passes make the worker Ready. ProbeTimeout bounds one probe.
type WorkerConfig struct {
	Interval      time.Duration
	ScrapeTimeout time.Duration
	DownAfter     time.Duration
	Window        time.Duration
	RisingOver    time.Duration
	WarmBody      []byte
	WarmUnder     time.Duration
	WarmPasses    int
	ProbeTimeout  time.Duration
}

// DefaultWorkerConfig is the shipped loop shape. It scrapes twice a second,
// marks a worker Down after 10s of silence, quotes TTFT over the last 30s
// and asks two probes under 1s before a worker is Ready.
func DefaultWorkerConfig(warmBody []byte) WorkerConfig {
	return WorkerConfig{
		Interval:      500 * time.Millisecond,
		ScrapeTimeout: time.Second,
		DownAfter:     10 * time.Second,
		Window:        30 * time.Second,
		RisingOver:    5 * time.Second,
		WarmBody:      warmBody,
		WarmUnder:     time.Second,
		WarmPasses:    2,
		ProbeTimeout:  30 * time.Second,
	}
}

// WorkerStatus is one worker as the fleet last saw it, for metrics and
// /debug. LastProbeS is the latest warm-up probe's latency in seconds.
// Restarts counts Down to Warming transitions, including the first one at
// startup.
type WorkerStatus struct {
	Pod        string
	Phase      Phase
	LastScrape time.Time
	LastProbeS float64
	Probes     int
	Restarts   int
}

// timed is one parsed scrape and when it was read.
type timed struct {
	at time.Time
	s  Sample
}

// member is one pod's loop state. st is written only by the pod's own
// goroutine, under mu so Status can read it. ring holds the scrapes of the
// last Window, oldest first, and is reset on every restart because vLLM's
// counters start over. passes counts consecutive probe passes.
type member struct {
	name string
	base string

	mu sync.Mutex
	st WorkerStatus

	ring   []timed
	passes int
}

// Fleet keeps the Gate fed. It runs one loop per pod that scrapes the
// pod's /metrics, feeds each scrape to the Gate and drives the pod through
// the warm-up gate. The Gate never calls back into the Fleet.
type Fleet struct {
	g       *Gate
	cfg     WorkerConfig
	client  *http.Client
	now     func() time.Time
	members []*member
}

// NewFleet builds a Fleet over urls, which maps each pod to its base URL.
// Status reports pods in name order. A nil client uses http.DefaultClient
// and a nil now uses time.Now.
func NewFleet(g *Gate, urls map[string]string, cfg WorkerConfig, client *http.Client, now func() time.Time) *Fleet {
	if client == nil {
		client = http.DefaultClient
	}
	if now == nil {
		now = time.Now
	}
	f := &Fleet{g: g, cfg: cfg, client: client, now: now}
	for name, base := range urls {
		f.members = append(f.members, &member{
			name: name,
			base: strings.TrimSuffix(base, "/"),
			st:   WorkerStatus{Pod: name},
		})
	}
	sort.Slice(f.members, func(i, j int) bool { return f.members[i].name < f.members[j].name })
	return f
}

// Run starts one loop per pod and returns once ctx ends and every loop
// has stopped.
func (f *Fleet) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, m := range f.members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.watch(ctx, m)
		}()
	}
	wg.Wait()
}

// Status returns every worker's status in pod name order.
func (f *Fleet) Status() []WorkerStatus {
	out := make([]WorkerStatus, 0, len(f.members))
	for _, m := range f.members {
		m.mu.Lock()
		out = append(out, m.st)
		m.mu.Unlock()
	}
	return out
}

// RawMetrics fetches a pod's /metrics body as the pod sent it, capped at
// maxMetricsBytes. An unknown pod is an error.
func (f *Fleet) RawMetrics(ctx context.Context, pod string) ([]byte, error) {
	for _, m := range f.members {
		if m.name == pod {
			scrapeCtx, cancel := context.WithTimeout(ctx, f.cfg.ScrapeTimeout)
			defer cancel()
			return f.get(scrapeCtx, m.base+"/metrics")
		}
	}
	return nil, fmt.Errorf("fleet: unknown pod %q", pod)
}

// watch runs one pod's loop until ctx ends. The first tick runs at once.
func (f *Fleet) watch(ctx context.Context, m *member) {
	ticker := time.NewTicker(f.cfg.Interval)
	defer ticker.Stop()
	for {
		f.tick(ctx, m)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick scrapes once and applies what it learned. A failed scrape only ages
// the worker, and a successful one feeds the Gate, then probes while the
// worker is Warming. A tick that fails because ctx ended changes nothing.
func (f *Fleet) tick(ctx context.Context, m *member) {
	s, err := f.scrape(ctx, m)
	now := f.now()
	if err != nil {
		if ctx.Err() == nil && now.Sub(m.st.LastScrape) >= f.cfg.DownAfter {
			f.move(m, stale)
		}
		return
	}

	f.move(m, scraped)
	m.ring = append(m.ring, timed{at: now, s: s})
	cutoff := now.Add(-f.cfg.Window)
	for len(m.ring) >= 2 && !m.ring[1].at.After(cutoff) {
		m.ring = m.ring[1:]
	}
	m.update(func(st *WorkerStatus) { st.LastScrape = now })
	f.g.Observe(m.name, f.scrapeOf(m, now))

	if m.st.Phase == Warming {
		f.probe(ctx, m)
		if m.passes >= f.cfg.WarmPasses {
			f.move(m, warmed)
		}
	}
}

// move applies one event to the worker and performs what entering the new
// phase owes the Gate. Entering Warming is a restart, so the ring and the
// pass count start over and the Gate drops the worker's bindings. Entering
// Ready and Down set and clear the Gate's ready flag.
func (f *Fleet) move(m *member, e event) {
	next := step(m.st.Phase, e)
	if next == m.st.Phase {
		return
	}
	m.update(func(st *WorkerStatus) {
		st.Phase = next
		if next == Warming {
			st.Restarts++
		}
	})
	switch next {
	case Warming:
		m.ring = nil
		m.passes = 0
		f.g.Restarted(m.name)
	case Ready:
		f.g.SetReady(m.name, true)
	case Down:
		f.g.SetReady(m.name, false)
	}
}

// scrapeOf builds the Gate's Scrape from the newest sample in the ring.
// The TTFT quantiles are taken over the delta between the oldest and the
// newest sample in the ring, so the first scrape after a restart quotes 0.
// WaitingRising compares the newest sample with the newest one at least
// RisingOver old, and is false while the ring is too short to tell.
func (f *Fleet) scrapeOf(m *member, now time.Time) Scrape {
	cur := m.ring[len(m.ring)-1].s
	prev := m.ring[0].s.TTFT
	rising := false
	if base, ok := m.sampleBefore(now.Add(-f.cfg.RisingOver)); ok {
		rising = cur.Waiting > base.Waiting
	}
	return Scrape{
		At:            now,
		KVPoolTokens:  cur.KVPoolTokens,
		KVUsage:       cur.KVUsage,
		TTFTp50S:      Quantile(cur.TTFT, prev, 0.5),
		TTFTp99S:      Quantile(cur.TTFT, prev, 0.99),
		WaitingRising: rising,
	}
}

// sampleBefore returns the newest sample read at or before t.
func (m *member) sampleBefore(t time.Time) (Sample, bool) {
	for i := len(m.ring) - 1; i >= 0; i-- {
		if !m.ring[i].at.After(t) {
			return m.ring[i].s, true
		}
	}
	return Sample{}, false
}

// scrape fetches and parses one /metrics body. A body without KV usage is
// a failed scrape, because the Gate cannot place on a worker whose load it
// cannot see.
func (f *Fleet) scrape(ctx context.Context, m *member) (Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.ScrapeTimeout)
	defer cancel()
	body, err := f.get(ctx, m.base+"/metrics")
	if err != nil {
		return Sample{}, err
	}
	s, err := ParseMetrics(bytes.NewReader(body))
	if err != nil {
		return Sample{}, err
	}
	if !s.OK {
		return Sample{}, errors.New("metrics body has no KV cache usage")
	}
	return s, nil
}

// get fetches url and returns its body, capped at maxMetricsBytes. Any
// status but 200 is an error.
func (f *Fleet) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxMetricsBytes))
}

// probe replays WarmBody against the worker once and scores it. A 200
// under WarmUnder extends the run of passes, and anything else, including
// a transport error, starts the run over. The request id carries the pod
// and probe number so vLLM's logs and the Gate's run table can tell probes
// from live traffic.
func (f *Fleet) probe(ctx context.Context, m *member) {
	m.update(func(st *WorkerStatus) { st.Probes++ })
	ctx, cancel := context.WithTimeout(ctx, f.cfg.ProbeTimeout)
	defer cancel()

	start := f.now()
	status, err := f.post(ctx, m.base+"/v1/chat/completions", "warmup-"+m.name+"-"+strconv.Itoa(m.st.Probes)+"-s1")
	elapsed := f.now().Sub(start)

	if err == nil && status == http.StatusOK && elapsed < f.cfg.WarmUnder {
		m.passes++
	} else {
		m.passes = 0
	}
	m.update(func(st *WorkerStatus) { st.LastProbeS = elapsed.Seconds() })
}

// post sends WarmBody to url and returns the status code. The response
// body is drained so the connection can be reused.
func (f *Fleet) post(ctx context.Context, url, requestID string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(f.cfg.WarmBody))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", requestID)
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetricsBytes)) // drained for connection reuse; only the status matters
	return resp.StatusCode, nil
}

// update edits the worker's status under its lock.
func (m *member) update(fn func(st *WorkerStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.st)
}
