package sched

import (
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"
)

type TaskGroup struct {
	cpuTime   int64
	startTime time.Time

	// All tasks in this task group is yield and maintained in a link list
	mu struct {
		sync.RWMutex
		tasks *TaskContext
	}
}

type TaskContext struct {
	tg *TaskGroup
	wg sync.WaitGroup

	lastCheck int64
	next      *TaskContext
}

type key int

const Key key = 0

func NewTaskGroup() *TaskGroup {
	return &TaskGroup{
		startTime: time.Now(),
	}
}

func fromContext(ctx context.Context) *TaskContext {
	val := ctx.Value(Key)
	if val == nil {
		return nil
	}
	ret, ok := val.(*TaskContext)
	if !ok {
		return nil
	}
	return ret
}

func NewContext(ctx context.Context, tg *TaskGroup) context.Context {
	return context.WithValue(ctx, Key, &TaskContext{
		tg: tg,
	})
}

func ContextWithSchedInfo(ctx context.Context) context.Context {
	tc := fromContext(ctx)
	if tc == nil {
		panic("no TaskContext found in current context!")
	}
	return NewContext(ctx, tc.tg)
}

func CheckPoint(ctx context.Context) {
	tc := fromContext(ctx)
	if tc == nil {
		return
	}

	tg := tc.tg
	tg.mu.RLock()
	scheduling := tg.mu.tasks != nil
	tg.mu.RUnlock()

	if !scheduling {
		runningTime := grunningnanos()
		elapse := runningTime - tc.lastCheck
		tc.lastCheck = runningTime
		cpuTime := atomic.AddInt64(&tg.cpuTime, elapse)
		if time.Duration(cpuTime) < 20*time.Millisecond {
			return
		}
	}

	tc.wg.Add(1)
	s.ch <- tc
	tc.wg.Wait()
}

type sched struct {
	ch chan *TaskContext
	pq *PriorityQueue
}

var s = sched{
	ch: make(chan *TaskContext, 4096),
	pq: newPriorityQueue(200),
}

func Scheduler() {
	lastTime := time.Now()
	const rate = 10
	capacity := 200 * time.Millisecond
	tokens := capacity
	for {
		select {
		case cp := <-s.ch:
			tg := cp.tg
			tg.mu.Lock()
			if tg.mu.tasks == nil {
				s.pq.Enqueue(tg)
			}
			cp.next = tg.mu.tasks
			tg.mu.tasks = cp
			tg.mu.Unlock()
		}

		// A token bucket algorithm to limit the total cpu usage.
		//
		// When a task group enqueue, it means a 20ms cpu time elapsed,
		// assume that the elapsed wall time is 100ms, it means the CPU usage is 20ms/100ms = 20%
		// assume that the elapsed wall time is 20ms, it means the CPU usage is 20ms/20ms = 100%
		// assume that the elapsed wall time is 5ms, it means the CPU usage is 20ms/5ms = 400%
		// The last case can happen in a multi-core environment.
		//
		// == How to decide the token generate rate? ==
		// Say, we want to control the CPU usage at 80%, there are 10 cores.
		// GC takes 25% of CPU resource, reserve that for it, we have actually 750% available in total.
		// 750% * 80% = 600%
		// CPU usage = cpu time / wall time, so the rate is: every 100ms, we can have 600ms cpu time,
		// 600ms / 20ms = 30, or we can say: every 100ms, we can have 30 token generated.

		now := time.Now()
		elapse := now.Sub(lastTime)
		lastTime = now

		// refill tokens
		tokens += rate * elapse
		if tokens > capacity {
			tokens = capacity
		}

		// == How to decide the priority of a task group? ==
		// If a task group A is swap-in to the queue, and its last running time is [startA, endA], the corresponding cpu time is 20ms.
		// It means the CPU resource usage rate for A is: 20ms / [now - startA], because [endA, now] does not take any CPU resource.
		//
		// If a task group B is swap-in to the queue, and its last running time is [startB, endB], the corresponding cpu time is 20ms.
		// It means the CPU resource usage rate for A is: 20ms / [now - startB], because [endB, now] does not take any CPU resource.
		//
		// Note that the 20ms cpu time is fixed for all the swaped-in task groups.
		// If startA < startB, task group A takes less CPU resource than task group B.
		// So we reach a conclusion that the smaller the start running time of a task group, the less CPU resource it uses, thus the higher priority.

		for !s.pq.Empty() {
			if tokens < 20*time.Millisecond {
				// Not enough tokens, rate limiter take effect.
				break
			}

			tg := s.pq.Dequeue()
			tokens -= 20 * time.Millisecond
			tg.startTime = now
			atomic.StoreInt64(&tg.cpuTime, 0)
			tg.mu.Lock()
			for tg.mu.tasks != nil {
				tc := tg.mu.tasks
				tg.mu.tasks = tg.mu.tasks.next
				tc.next = nil
				tc.wg.Done()
			}
			tg.mu.Unlock()
		}
	}
}

type PriorityQueue struct {
	data []*TaskGroup
	size int
}

func newPriorityQueue(size int) *PriorityQueue {
	data := make([]*TaskGroup, 0, size)
	return &PriorityQueue{
		data: data,
		size: size,
	}
}

func (pq *PriorityQueue) Len() int {
	return len(pq.data)
}

func (pq *PriorityQueue) Less(i, j int) bool {
	di := pq.data[i]
	dj := pq.data[j]
	return di.startTime.Before(dj.startTime)
}

func (pq *PriorityQueue) Swap(i, j int) {
	pq.data[i], pq.data[j] = pq.data[j], pq.data[i]
}

func (pq *PriorityQueue) Push(x interface{}) {
	pq.data = append(pq.data, x.(*TaskGroup))
}

func (pq *PriorityQueue) Pop() interface{} {
	n := len(pq.data)
	ret := pq.data[n-1]
	pq.data = pq.data[: n-1 : cap(pq.data)]
	return ret
}

func (pq *PriorityQueue) Enqueue(x *TaskGroup) {
	heap.Push(pq, x)
}

func (pq *PriorityQueue) Dequeue() *TaskGroup {
	return heap.Pop(pq).(*TaskGroup)
}

func (pq *PriorityQueue) Full() bool {
	return len(pq.data) == pq.size
}

func (pq *PriorityQueue) Empty() bool {
	return len(pq.data) == 0
}

//go:linkname grunningnanos runtime.grunningnanos
func grunningnanos() int64
