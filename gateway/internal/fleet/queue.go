// Package fleet holds the gateway's stateful side. Queue is the per-worker
// in-flight cap and the two-lane wait list behind it, the only step of the
// pipeline that blocks.
package fleet

import (
	"context"
	"sync"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

// Refusal is Acquire's error when the queue turns a request away. Reason is
// the shed label the caller reports.
type Refusal struct {
	Reason decide.Reason
}

func (r *Refusal) Error() string {
	return "gateway queue: " + string(r.Reason)
}

// waiterState is where a waiter is in its life. A waiter starts waiting and
// moves exactly once, to granted or dropped, under the queue lock. Its grant
// channel closes at that move and never again.
type waiterState uint8

const (
	waiting waiterState = iota
	granted
	dropped
)

// waiter is one blocked Acquire. Every field but grant is guarded by the
// queue lock.
type waiter struct {
	grant      chan struct{}
	prio       decide.Priority
	state      waiterState
	dropReason decide.Reason
}

// worker is one pod's slot accounting. lanes is indexed by decide.Priority,
// so lanes[decide.Interactive] drains before lanes[decide.Batch]. Each lane
// is FIFO.
type worker struct {
	inFlight  int
	max       int
	maxQueued int
	lanes     [2][]*waiter
}

func (w *worker) queued() int {
	return len(w.lanes[decide.Interactive]) + len(w.lanes[decide.Batch])
}

// Queue is the gateway's per-worker admission queue. One mutex guards every
// worker, because grants on one worker are rare and short, and one lock keeps
// Depth consistent across lanes. The lock is never held while blocking, and
// the queue spawns no goroutines. Waiters are woken by the releaser.
type Queue struct {
	mu      sync.Mutex
	workers map[string]*worker
}

// NewQueue builds a Queue for pods, each with maxInflight slots and room for
// maxQueued waiters across both lanes.
func NewQueue(pods []string, maxInflight, maxQueued int) *Queue {
	q := &Queue{workers: make(map[string]*worker, len(pods))}
	for _, pod := range pods {
		q.workers[pod] = &worker{max: maxInflight, maxQueued: maxQueued}
	}
	return q
}

// Slot is one held in-flight slot on a worker. Release returns it. The zero
// value is not a slot, so only Acquire builds one.
type Slot struct {
	q        *Queue
	w        *worker
	released bool
}

// Acquire takes an in-flight slot on pod, or waits for one until deadline
// or ctx ends. A free slot is granted at once only when no one is already
// waiting, so a newcomer never passes the lanes. A full queue refuses a
// batch arrival with queue_full. An interactive arrival at a full queue
// displaces the newest batch waiter instead, which then returns queue_full.
// A deadline already in the past returns timeout_queue unless a slot is free.
// A waiter that times out or is cancelled after it was granted keeps the
// slot, so a granted slot is never leaked. An unknown pod returns
// no_eligible_pod.
func (q *Queue) Acquire(ctx context.Context, pod string, prio decide.Priority, deadline time.Time) (*Slot, error) {
	if prio != decide.Interactive {
		prio = decide.Batch
	}

	q.mu.Lock()
	w, ok := q.workers[pod]
	if !ok {
		q.mu.Unlock()
		return nil, &Refusal{Reason: decide.ReasonNoEligiblePod}
	}
	if w.inFlight < w.max && w.queued() == 0 {
		w.inFlight++
		q.mu.Unlock()
		return &Slot{q: q, w: w}, nil
	}
	if !deadline.After(time.Now()) {
		q.mu.Unlock()
		return nil, &Refusal{Reason: decide.ReasonTimeoutQueue}
	}
	if w.queued() >= w.maxQueued {
		batch := w.lanes[decide.Batch]
		if prio != decide.Interactive || len(batch) == 0 {
			q.mu.Unlock()
			return nil, &Refusal{Reason: decide.ReasonQueueFull}
		}
		newest := batch[len(batch)-1]
		w.lanes[decide.Batch] = batch[:len(batch)-1]
		newest.settle(dropped, decide.ReasonQueueFull)
	}
	me := &waiter{grant: make(chan struct{}), prio: prio}
	w.lanes[prio] = append(w.lanes[prio], me)
	q.mu.Unlock()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case <-me.grant:
		return q.resolve(w, me, nil)
	case <-timer.C:
		return q.resolve(w, me, &Refusal{Reason: decide.ReasonTimeoutQueue})
	case <-ctx.Done():
		return q.resolve(w, me, ctx.Err())
	}
}

// resolve turns a woken waiter into Acquire's result. A granted waiter gets
// its slot whatever woke it, because the releaser already counted it in
// flight. A dropped waiter returns its drop reason. A waiter still waiting
// was woken by its deadline or its context, so it leaves its lane and
// returns late.
func (q *Queue) resolve(w *worker, me *waiter, late error) (*Slot, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	switch me.state {
	case granted:
		return &Slot{q: q, w: w}, nil
	case dropped:
		return nil, &Refusal{Reason: me.dropReason}
	}
	w.remove(me)
	me.settle(dropped, decide.ReasonTimeoutQueue)
	return nil, late
}

// settle moves a waiting waiter to its final state and closes its grant
// channel. It must be called under the queue lock, and only on a waiter that
// is still waiting, so the channel closes exactly once.
func (wt *waiter) settle(to waiterState, reason decide.Reason) {
	wt.state = to
	wt.dropReason = reason
	close(wt.grant)
}

// remove takes wt out of its lane. It must be called under the queue lock.
func (w *worker) remove(wt *waiter) {
	lane := w.lanes[wt.prio]
	for i, cand := range lane {
		if cand == wt {
			w.lanes[wt.prio] = append(lane[:i], lane[i+1:]...)
			return
		}
	}
}

// Release returns the slot and hands free slots to waiters, interactive
// lane first, each lane in arrival order. A second call is a no-op, so a
// handler that releases on every exit path cannot over-grant.
func (s *Slot) Release() {
	s.q.mu.Lock()
	defer s.q.mu.Unlock()

	if s.released {
		return
	}
	s.released = true
	s.w.inFlight--
	s.w.grantWaiters()
}

// grantWaiters hands slots to waiters while the worker has room. It must be
// called under the queue lock. It counts each granted waiter in flight here,
// so a waiter that stops waiting before it runs still owns a slot and
// releases it itself.
func (w *worker) grantWaiters() {
	for w.inFlight < w.max {
		var lane *[]*waiter
		switch {
		case len(w.lanes[decide.Interactive]) > 0:
			lane = &w.lanes[decide.Interactive]
		case len(w.lanes[decide.Batch]) > 0:
			lane = &w.lanes[decide.Batch]
		default:
			return
		}
		head := (*lane)[0]
		n := copy(*lane, (*lane)[1:])
		(*lane)[n] = nil
		*lane = (*lane)[:n]
		w.inFlight++
		head.settle(granted, "")
	}
}

// Depth reports pod's in-flight count and the length of each lane, for
// metrics and WorkerView. An unknown pod reports zeros.
func (q *Queue) Depth(pod string) (inFlight, interactive, batch int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	w, ok := q.workers[pod]
	if !ok {
		return 0, 0, 0
	}
	return w.inFlight, len(w.lanes[decide.Interactive]), len(w.lanes[decide.Batch])
}
