package fleet

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

const pod = "vllm-0"

var far = time.Now().Add(time.Hour)

type outcome struct {
	slot *Slot
	err  error
}

// acquireAsync runs Acquire in a goroutine and returns the channel its
// result lands on.
func acquireAsync(q *Queue, prio decide.Priority, deadline time.Time) <-chan outcome {
	return acquireCtx(context.Background(), q, prio, deadline)
}

func acquireCtx(ctx context.Context, q *Queue, prio decide.Priority, deadline time.Time) <-chan outcome {
	done := make(chan outcome, 1)
	go func() {
		s, err := q.Acquire(ctx, pod, prio, deadline)
		done <- outcome{s, err}
	}()
	return done
}

// waitDepth polls until Depth(pod) equals (inFlight, interactive, batch), so
// a test can be sure a goroutine has reached its lane before the next step.
func waitDepth(t *testing.T, q *Queue, inFlight, interactive, batch int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		i, a, b := q.Depth(pod)
		if i == inFlight && a == interactive && b == batch {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	i, a, b := q.Depth(pod)
	t.Fatalf("Depth = (%d, %d, %d), want (%d, %d, %d)", i, a, b, inFlight, interactive, batch)
}

func assertDepth(t *testing.T, q *Queue, inFlight, interactive, batch int) {
	t.Helper()
	i, a, b := q.Depth(pod)
	if i != inFlight || a != interactive || b != batch {
		t.Fatalf("Depth = (%d, %d, %d), want (%d, %d, %d)", i, a, b, inFlight, interactive, batch)
	}
}

func mustSlot(t *testing.T, done <-chan outcome) *Slot {
	t.Helper()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("Acquire error = %v, want a slot", o.err)
		}
		return o.slot
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return a slot in time")
	}
	return nil
}

func mustRefusal(t *testing.T, done <-chan outcome, want decide.Reason) {
	t.Helper()
	select {
	case o := <-done:
		assertRefusal(t, o, want)
	case <-time.After(2 * time.Second):
		t.Fatalf("Acquire did not return %q in time", want)
	}
}

func assertRefusal(t *testing.T, o outcome, want decide.Reason) {
	t.Helper()
	var r *Refusal
	if !errors.As(o.err, &r) {
		t.Fatalf("Acquire = (%v, %v), want *Refusal{%q}", o.slot, o.err, want)
	}
	if r.Reason != want {
		t.Fatalf("Refusal reason = %q, want %q", r.Reason, want)
	}
	if o.slot != nil {
		t.Fatal("Acquire returned a slot alongside a refusal")
	}
}

func assertStillWaiting(t *testing.T, done <-chan outcome) {
	t.Helper()
	select {
	case o := <-done:
		t.Fatalf("Acquire returned (%v, %v) while the worker was full", o.slot, o.err)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestAcquireGrantsFreeSlotsThenWaitsForRelease(t *testing.T) {
	q := NewQueue([]string{pod}, 2, 32)

	s1 := mustSlot(t, acquireAsync(q, decide.Batch, far))
	assertDepth(t, q, 1, 0, 0)
	s2 := mustSlot(t, acquireAsync(q, decide.Batch, far))
	assertDepth(t, q, 2, 0, 0)

	third := acquireAsync(q, decide.Interactive, far)
	waitDepth(t, q, 2, 1, 0)
	assertStillWaiting(t, third)

	s1.Release()
	s3 := mustSlot(t, third)
	assertDepth(t, q, 2, 0, 0)

	s2.Release()
	s3.Release()
	assertDepth(t, q, 0, 0, 0)
}

func TestReleaseGrantsInteractiveLaneFirstThenFIFO(t *testing.T) {
	q := NewQueue([]string{pod}, 1, 32)
	holder := mustSlot(t, acquireAsync(q, decide.Batch, far))

	order := make(chan string, 4)
	enqueue := func(name string, prio decide.Priority, interactive, batch int) {
		go func() {
			s, err := q.Acquire(context.Background(), pod, prio, far)
			if err != nil {
				t.Errorf("%s: Acquire error = %v", name, err)
				return
			}
			order <- name
			s.Release()
		}()
		waitDepth(t, q, 1, interactive, batch)
	}
	enqueue("B1", decide.Batch, 0, 1)
	enqueue("I1", decide.Interactive, 1, 1)
	enqueue("B2", decide.Batch, 1, 2)
	enqueue("I2", decide.Interactive, 2, 2)

	holder.Release()

	want := []string{"I1", "I2", "B1", "B2"}
	for i, name := range want {
		select {
		case got := <-order:
			if got != name {
				t.Fatalf("grant %d = %q, want %q", i+1, got, name)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("grant %d (%q) never happened", i+1, name)
		}
	}
	waitDepth(t, q, 0, 0, 0)
}

func TestQueueFullRefusesBatchAndDisplacesNewestBatchForInteractive(t *testing.T) {
	q := NewQueue([]string{pod}, 1, 2)
	holder := mustSlot(t, acquireAsync(q, decide.Batch, far))

	b1 := acquireAsync(q, decide.Batch, far)
	waitDepth(t, q, 1, 0, 1)
	b2 := acquireAsync(q, decide.Batch, far)
	waitDepth(t, q, 1, 0, 2)

	b3 := acquireAsync(q, decide.Batch, far)
	mustRefusal(t, b3, decide.ReasonQueueFull)
	assertDepth(t, q, 1, 0, 2)

	i1 := acquireAsync(q, decide.Interactive, far)
	mustRefusal(t, b2, decide.ReasonQueueFull)
	waitDepth(t, q, 1, 1, 1)
	assertStillWaiting(t, b1)

	holder.Release()
	s := mustSlot(t, i1)
	assertDepth(t, q, 1, 0, 1)

	s.Release()
	mustSlot(t, b1).Release()
	assertDepth(t, q, 0, 0, 0)
}

func TestQueueFullWithNoBatchWaitersRefusesInteractive(t *testing.T) {
	q := NewQueue([]string{pod}, 1, 1)
	holder := mustSlot(t, acquireAsync(q, decide.Batch, far))

	i1 := acquireAsync(q, decide.Interactive, far)
	waitDepth(t, q, 1, 1, 0)
	mustRefusal(t, acquireAsync(q, decide.Interactive, far), decide.ReasonQueueFull)

	holder.Release()
	mustSlot(t, i1).Release()
	assertDepth(t, q, 0, 0, 0)
}

func TestDeadlineInQueueReturnsTimeoutQueue(t *testing.T) {
	q := NewQueue([]string{pod}, 1, 32)
	holder := mustSlot(t, acquireAsync(q, decide.Batch, far))

	late := acquireAsync(q, decide.Batch, time.Now().Add(15*time.Millisecond))
	mustRefusal(t, late, decide.ReasonTimeoutQueue)
	assertDepth(t, q, 1, 0, 0)

	s, err := q.Acquire(context.Background(), pod, decide.Interactive, time.Now().Add(-time.Second))
	assertRefusal(t, outcome{s, err}, decide.ReasonTimeoutQueue)
	assertDepth(t, q, 1, 0, 0)

	holder.Release()
	s, err = q.Acquire(context.Background(), pod, decide.Interactive, time.Now().Add(-time.Second))
	if err != nil || s == nil {
		t.Fatalf("Acquire with a past deadline and a free slot = (%v, %v), want a slot", s, err)
	}
	assertDepth(t, q, 1, 0, 0)
	s.Release()
	assertDepth(t, q, 0, 0, 0)
}

func TestContextCancelRemovesWaiter(t *testing.T) {
	q := NewQueue([]string{pod}, 1, 32)
	holder := mustSlot(t, acquireAsync(q, decide.Batch, far))

	ctx, cancel := context.WithCancel(context.Background())
	waiting := acquireCtx(ctx, q, decide.Interactive, far)
	waitDepth(t, q, 1, 1, 0)
	cancel()

	select {
	case o := <-waiting:
		if !errors.Is(o.err, context.Canceled) || o.slot != nil {
			t.Fatalf("Acquire = (%v, %v), want (nil, context.Canceled)", o.slot, o.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return after cancel")
	}
	assertDepth(t, q, 1, 0, 0)

	holder.Release()
	assertDepth(t, q, 0, 0, 0)
}

func TestReleaseIsIdempotent(t *testing.T) {
	q := NewQueue([]string{pod}, 1, 32)
	holder := mustSlot(t, acquireAsync(q, decide.Batch, far))

	w1 := acquireAsync(q, decide.Batch, far)
	waitDepth(t, q, 1, 0, 1)
	w2 := acquireAsync(q, decide.Batch, far)
	waitDepth(t, q, 1, 0, 2)

	holder.Release()
	holder.Release()
	s1 := mustSlot(t, w1)
	assertDepth(t, q, 1, 0, 1)
	assertStillWaiting(t, w2)

	s1.Release()
	s1.Release()
	s2 := mustSlot(t, w2)
	assertDepth(t, q, 1, 0, 0)

	s2.Release()
	s2.Release()
	holder.Release()
	assertDepth(t, q, 0, 0, 0)
}

func TestNoSlotLeakUnderRace(t *testing.T) {
	q := NewQueue([]string{pod}, 4, 32)
	rng := rand.New(rand.NewSource(7))
	const n = 200
	waits := make([]time.Duration, n)
	for i := range waits {
		waits[i] = time.Duration(rng.Intn(8000)) * time.Microsecond
	}

	var mu sync.Mutex
	var grants, releases, refusals int
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			prio := decide.Batch
			if i%3 == 0 {
				prio = decide.Interactive
			}
			s, err := q.Acquire(context.Background(), pod, prio, time.Now().Add(waits[i]))
			if err != nil {
				var r *Refusal
				if !errors.As(err, &r) {
					t.Errorf("Acquire error = %v, want *Refusal", err)
					return
				}
				if r.Reason != decide.ReasonTimeoutQueue && r.Reason != decide.ReasonQueueFull {
					t.Errorf("Refusal reason = %q, want timeout_queue or queue_full", r.Reason)
				}
				mu.Lock()
				refusals++
				mu.Unlock()
				return
			}
			mu.Lock()
			grants++
			mu.Unlock()
			time.Sleep(time.Millisecond)
			s.Release()
			mu.Lock()
			releases++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if grants != releases {
		t.Fatalf("grants = %d, releases = %d, want equal", grants, releases)
	}
	if grants+refusals != n {
		t.Fatalf("grants + refusals = %d, want %d", grants+refusals, n)
	}
	if grants < 4 {
		t.Fatalf("grants = %d, want at least the 4 free slots", grants)
	}
	assertDepth(t, q, 0, 0, 0)
}

func TestUnknownPodReturnsNoEligiblePod(t *testing.T) {
	q := NewQueue([]string{pod}, 16, 32)
	s, err := q.Acquire(context.Background(), "vllm-9", decide.Interactive, far)
	assertRefusal(t, outcome{s, err}, decide.ReasonNoEligiblePod)
	if i, a, b := q.Depth("vllm-9"); i != 0 || a != 0 || b != 0 {
		t.Fatalf("Depth(unknown) = (%d, %d, %d), want (0, 0, 0)", i, a, b)
	}
}

func TestRefusalError(t *testing.T) {
	err := error(&Refusal{Reason: decide.ReasonQueueFull})
	if got, want := err.Error(), "gateway queue: queue_full"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
