package sched

import (
	"fmt"
	"syscall"
	"runtime"
	"runtime/debug"
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"

	_ "go.uber.org/automaxprocs"
)

type TaskGroup struct {
	cpuTime   int64
	startTime time.Time

	// All task context (goroutine) in this task group is yield and maintained in a link list
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

type keyType int

const key keyType = 0

var Debug = "schedTest"

func fromContext(ctx context.Context) *TaskContext {
	val := ctx.Value(key)
	if val == nil {
		return nil
	}
	ret, ok := val.(*TaskContext)
	if !ok {
		return nil
	}
	return ret
}

func FromContext(ctx context.Context) *TaskContext {
	return fromContext(ctx)
}

// NewTaskGroup returns a new context with TaskGroup attached using context.WithValue.
// A task group is a scheduling unit consist of a group of goroutines.
// The necessary information for scheduling is embeded in the context.Context.
func NewTaskGroup(ctx context.Context) (context.Context, *TaskGroup) {
	tg := &TaskGroup{
		startTime: time.Now(),
	}
	return context.WithValue(ctx, key, &TaskContext{
		tg: tg,
	}), tg
}

// WithSchedInfo returns a new context with scheduling information attached.
// When creating running a new goroutine from a task group, use this:
//     ctx := WithSchedInfo(ctx)
//     go subTask(ctx)
func WithSchedInfo(ctx context.Context) context.Context {
	tc := fromContext(ctx)
	if tc == nil {
		if x := ctx.Value(Debug); x != nil {
			debug.PrintStack()
		}
		return ctx
	}
	return context.WithValue(ctx, key, &TaskContext{
		tg: tc.tg,
	})
}

// Go runs f in a separate goroutine. It's the same with:
//     ctx := WithSchedInfo(ctx)
//     go f(ctx)
// It's recommended to use this API rather than do above steps manually,
// because one may forget to call WithSchedInfo.
func Go(ctx context.Context, f func(ctx context.Context)) {
	ctx = WithSchedInfo(ctx)
	go f(ctx)
}

// CheckPoint should be called manually to check whether the goroutine (task group)
// run out of its time slice. If so, the goroutine is yielded for scheduling.
// The scheduler will wake up it according to the scheduling algorithm.
func CheckPoint(ctx context.Context) {
	if s.disabled.Load() == true {
		return
	}

	tc := fromContext(ctx)
	if tc == nil {
		if x := ctx.Value(Debug); x != nil {
			debug.PrintStack()
		}
		return
	}

	tg := tc.tg
	tg.mu.RLock()
	scheduling := tg.mu.tasks != nil
	tg.mu.RUnlock()

	runningTime := grunningnanos()
	elapse := runningTime - tc.lastCheck
	tc.lastCheck = runningTime
	cpuTime := atomic.AddInt64(&tg.cpuTime, elapse)

	if !scheduling && time.Duration(cpuTime) < s.cfg.timeSlice {
		return
	}

	tc.wg.Add(1)
	s.ch <- tc
	tc.wg.Wait()
}

type config struct {
	timeSlice time.Duration
	load float64
	capacity time.Duration
}

type sched struct {
	cfg config
	ch chan *TaskContext
	refillCh chan time.Duration
	pq *PriorityQueue
	disabled    atomic.Bool
}

var s = sched {
	ch: make(chan *TaskContext, 8192),
	refillCh: make(chan time.Duration, 100),
	pq: newPriorityQueue(1024),
	cfg: config{
		timeSlice: 20 * time.Millisecond,
		load: 0.8,
	},
}

// Enable is provided for debugging.
func Enable() {
	s.disabled.Store(false)
}

// Disable is provided for debugging.
func Disable() {
	s.disabled.Store(true)
}

// Option overrides the default scheduling parameters.
type Option func(*config)

// Scheduler should run in a separate goroutine, and typically be called at the beginning of the process.
func Scheduler(opts ...Option) {
	numCPU := runtime.GOMAXPROCS(0)
	for _, opt := range opts {
		opt(&s.cfg)
	}
	timeSlice := s.cfg.timeSlice
	capacity := s.cfg.capacity
	if capacity == 0 {
		capacity = timeSlice * time.Duration(numCPU)
		// capacity = time.Duration(float64(timeSlice) * rate)
	}

	fmt.Println("numCPU=", numCPU, "load=", s.cfg.load, "capacity=", capacity, "timeSlice=", timeSlice)
	go refillTokensByLoad(s.refillCh, timeSlice, s.cfg.load * float64(numCPU))

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
		case refill := <-s.refillCh:
			tokens += refill
			if tokens > capacity {
				tokens = capacity
			}
			if tokens < 0 {
				tokens = 0
			}
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
			if tokens < timeSlice {
				// fmt.Println("sched really take effect", s.pq.Len(), "token:", tokens)
				break
			}

			tg := s.pq.Dequeue()
			cpuTime := time.Duration(atomic.LoadInt64(&tg.cpuTime))
			tokens -= cpuTime
			tg.startTime = time.Now()
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

func refillTokensByLoad(refillCh chan<- time.Duration, timeSlice time.Duration, load float64) {
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

	lastWallClock := time.Now()
	ticker := time.NewTicker(10 * time.Millisecond)
	lastSys, lastUsr := getProcessCPUTime()
	totalReal, totalExpect := time.Duration(0), time.Duration(0)
	expect := 10 * time.Millisecond
	capacity := 10 * time.Duration(float64(time.Millisecond) * load)
	for i := 0; ; i++ {
		now := <-ticker.C
		sys,usr := getProcessCPUTime()
		s := sys.Sub(lastSys)
		u := usr.Sub(lastUsr)
		lastSys = sys
		lastUsr = usr
		real := s + u

		elapse := now.Sub(lastWallClock)
		lastWallClock = now

		max := time.Duration(float64(elapse) * load)
		// expect := time.Duration(float64(elapse) * load)

		totalReal += real
		totalExpect += max
		
		delta := max - real
		_ = delta
		// refillCh <- delta
		// refillCh <- time.Duration(float64(delta) * 0.5) + time.Duration(float64(expect) * 0.5)
		// refillCh <- time.Duration(float64(delta) * 0.4) + time.Duration(float64(expect) * 0.6)
		// refillCh <- time.Duration(float64(delta) * 0.25) + time.Duration(float64(expect) * 0.75)
		// refillCh <- expect

		if delta < 0 {
			// expect = time.Duration(1.8 * float64(delta)) + expect
			// refillCh <- time.Duration(1.8 * float64(delta)) + expect
			// expect = time.Duration(1.5 * float64(delta)) + expect
			// expect = time.Duration(1.2 * float64(delta)) + expect
			expect += delta
		} else {
			expect += delta
		}
		if expect >= capacity {
			expect = capacity
		}

		if expect < 0 {
			fmt.Println("expect:", expect, "delta:", delta, "max:", max, "real:", real, "sys:", s, "usr:", u)
		}

		refillCh  <- expect

		if i % 100 == 0 {
			fmt.Println("totalReal:", totalReal, "totalExpect:", totalExpect)
			totalReal = 0
			totalExpect = 0
		}
	}
}

func getProcessCPUTime() (time.Time, time.Time) {
	var r syscall.Rusage
	err := syscall.Getrusage(syscall.RUSAGE_SELF, &r)
	if err != nil {
		panic(err)
	}
	sys := time.Unix(r.Stime.Sec, r.Stime.Usec*1000)
	usr := time.Unix(r.Utime.Sec, r.Utime.Usec*1000)
	return sys, usr
}

// TimeSliceOption controls the time-slice of a task group.
// It's default to 20ms
func TimeSliceOption(v time.Duration) Option {
	return func(c *config) {
		c.timeSlice = v
	}
}

// LoadOption controls the `load` of the CPU usage, default to 0.8 (80%)
func LoadOption(v float64) Option {
	return func(c *config) {
		c.load = v
	}
}

// CapacityOption controls the token bucket capacity, which controls the burst behaviour.
// Default value is GOMAXPROCS * time slice, DONOT SET IT TO 0
func CapacityOption(v time.Duration) Option {
	return func(c *config) {
		c.capacity = v
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

func (pq *PriorityQueue) Peek() *TaskGroup {
	n := len(pq.data)
	return pq.data[n-1]
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

