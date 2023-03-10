//go:build linux
// +build linux

package sched

import (
	// "fmt"
	"container/heap"
	"context"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "go.uber.org/automaxprocs"
)

type TaskGroup struct {
	cpuTime   int64
	startTime time.Time
	weight    float64

	// All task context (goroutine) in this task group is yield and maintained in a link list
	mu struct {
		sync.RWMutex
		tasks *TaskContext
	}
	version uint64
}

// SetWeight can set the weight of this task group if the scheduler is configured to weighted.
func (tg *TaskGroup) SetWeight(w float64) {
	tg.weight = w
}

type TaskContext struct {
	tg        *TaskGroup
	wg        sync.WaitGroup
	lastCheck int64
	next      *TaskContext
	version   uint64
}

// type startTimeHistory struct {
// 	queue [5]time.Time
// 	pos int
// 	len int
// }

// func (sth *startTimeHistory) startTime() time.Time {
// 	last := (sth.pos + len(sth.queue) - 1) % len(sth.queue)
// 	return sth.queue[last]
// }

// func (sth *startTimeHistory) add(t time.Time) {
// 	sth.queue[sth.pos] = t
// 	sth.pos = (sth.pos + 1) % len(sth.queue)
// 	sth.len++
// 	if sth.len >= len(sth.queue) {
// 		sth.len = len(sth.queue)
// 	}
// }

// func (sth *startTimeHistory) usage() float64 {
// 	if sth.len < 2 {
// 		dur := time.Now().Sub(sth.startTime())
// 		return float64(s.cfg.timeSlice) / float64(dur)
// 	}
// 	firstPos := (sth.pos + len(sth.queue) - sth.len) % len(sth.queue)
// 	oldest := sth.queue[firstPos]
// 	dur := sth.startTime().Sub(oldest)
// 	return float64(sth.len-1) * float64(s.cfg.timeSlice) / float64(dur)
// }

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

// NewTaskGroup returns a new context with TaskGroup attached using context.WithValue.
// A task group is a scheduling unit consist of a group of goroutines.
// The necessary information for scheduling is embeded in the context.Context.
func NewTaskGroup(ctx context.Context) (context.Context, *TaskGroup) {
	tg := &TaskGroup{}
	tg.weight = 1.0
	tg.startTime = time.Now()
	return context.WithValue(ctx, key, &TaskContext{
		tg: tg,
	}), tg
}

// WithSchedInfo returns a new context with scheduling information attached.
// When creating running a new goroutine from a task group, use this:
//
//	go f(sched.WithSchedInfo(ctx))
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

// CheckPoint should be called manually to check whether the goroutine (task group)
// run out of its time slice. If so, the goroutine is yielded for scheduling.
// The scheduler will wake up it according to the scheduling algorithm.
func CheckPoint(ctx context.Context) {
	if !supported() || s.disabled.Load() == true {
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

	if !scheduling {
		runningTime := grunningnanos()
		elapse := runningTime - tc.lastCheck
		tc.lastCheck = runningTime
		cpuTime := atomic.AddInt64(&tg.cpuTime, elapse)
		if time.Duration(cpuTime) < s.cfg.timeSlice {
			return
		}
	}

	tc.wg.Add(1)
	s.ch <- tc
	tc.wg.Wait()
}

type config struct {
	timeSlice time.Duration
	capacity  time.Duration
	load      float64
	weighted  bool
}

type sched struct {
	cfg        config
	ch         chan *TaskContext
	feedbackCh chan feedbackData
	pq         *PriorityQueue
	disabled   atomic.Bool
}

var s = sched{
	ch:         make(chan *TaskContext, 8192),
	feedbackCh: make(chan feedbackData, 100),
	pq:         newPriorityQueue(1024),
	cfg: config{
		timeSlice: 10 * time.Millisecond,
		load:      1,
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
	if !supported() {
		return
	}

	numCPU := runtime.GOMAXPROCS(0)
	for _, opt := range opts {
		opt(&s.cfg)
	}
	timeSlice := s.cfg.timeSlice
	capacity := s.cfg.capacity
	if capacity == 0 {
		capacity = timeSlice * time.Duration(numCPU)
	}

	var output time.Duration
	// monitor the current CPU load to adjust the rate.
	initRate := s.cfg.load * float64(numCPU)
	go cpuLoadFeedback(s.feedbackCh, initRate)
	rate := initRate
	fb := feedback{
		numCPU:   numCPU,
		initRate: initRate,
	}
	tokens := capacity
	lastTime := time.Now()
	for i := 0; ; {
		select {
		case tc := <-s.ch:
			tg := tc.tg
			tg.mu.Lock()
			if tg.mu.tasks == nil {
				if tc.version < tg.version {
					tc.version = tg.version
					tc.wg.Done()
					tg.mu.Unlock()
					continue
				}
				s.pq.Enqueue(tg)
			}
			tc.next = tg.mu.tasks
			tg.mu.tasks = tc
			tg.mu.Unlock()
		case data := <-s.feedbackCh:
			// Adjust the rate according to the feedback algorithm.
			rate = feedbackAlgorithm(&fb, rate, data)

			if i%100 == 0 {
				// fmt.Println("rate:", rate, "output:", output)
				output = 0
			}
			i++
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
		refill := time.Duration(rate * float64(elapse))
		tokens += refill
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
			if tokens < timeSlice {
				// fmt.Println("queue len==", s.pq.Len(), "rate:", rate)
				break
			}

			tg := s.pq.Dequeue()
			tg.version++

			// Should it be `tokens - tg.cpuTime` here?
			// tg.cpuTime may go beyond timeSlice, because the goroutines of the task group would not
			// stop simultaneously, when one goroutine detected the timeSlice threshold,
			// the others goroutines could still be running and consuming CPU until a CheckPoint is reached.
			tokens -= timeSlice
			tg.startTime = time.Now()
			output += timeSlice

			atomic.StoreInt64(&tg.cpuTime, 0)
			tg.mu.Lock()
			for tg.mu.tasks != nil {
				tc := tg.mu.tasks
				tc.version = tg.version
				tg.mu.tasks = tg.mu.tasks.next
				tc.next = nil
				tc.wg.Done()
			}
			tg.mu.Unlock()
		}
	}
}

type feedbackData struct {
	elapse  time.Duration
	cpuTime time.Duration
}

type feedbackPhase int64

const (
	Stable feedbackPhase = iota
	Idle
	Busy
	GC
)

type feedback struct {
	numCPU   int
	initRate float64
	phase    feedbackPhase
	count    int
	rateAVG  float64
}

func feedbackAlgorithm(f *feedback, rate float64, fb feedbackData) float64 {
	// The feedback algorithm works mainly by switching between two phases:
	//     1. feedback by the load -- Idle
	//     2. decrease the rate a bit when the load is saturated -- Busy
	// There are yet another two phases: Stable and GC
	// The rate will be adjusted between Idle and Busy to make it located in the Stable phase.
	// In Stable phase, the rate is not adjusted.
	// When Go do the garbage collection, the workload is totally different, so there is a GC phase.
	usage := float64(fb.cpuTime) / (float64(fb.elapse) * float64(f.numCPU))
	// The ticker default to 10ms, when the program is too busy, the ticker become unstable.
	tickerStable := fb.elapse > 8*time.Millisecond && fb.elapse < 12*time.Millisecond

	if usage < 0.8 && tickerStable { // Idle phase
		// The expected cpu time = numCPU * elapsed wall clock
		expectCPUTime := f.initRate * float64(fb.elapse)
		// delta is the difference between expected cpu time and actual cpu time
		delta := expectCPUTime - float64(fb.cpuTime)
		// delta / the period duration is used as feedback for the rate.
		rateDelta := float64(delta) / float64(fb.elapse+10*time.Millisecond)
		rate += rateDelta
		// fmt.Println("rate:", rate, "usage:", usage, "elapse:", fb.elapse)
		f.phase = Idle
	} else if usage > 0.96 { // Busy phase
		// When the load is saturated, feedback the rate by the load does not work well.
		// Just turn down the rate a bit, to see does the load turn down as well.
		// If if become Idle again, the Idle phase feedback would increase the rate.
		// The ultimate goal is make rate to the Stable phase.
		if rate > f.rateAVG*0.7 {
			// fmt.Println("irate:", rate, "after:", rate * 0.96, "usage:", usage, "elapse:", fb.elapse)
			rate = rate * 0.96
			f.phase = Busy
		} else {
			// There is a **fake** busy condition: when Go GC, the load is high
			// If we turn down the rate, the Go runtime would take the idle CPU resource to assist GC
			// This is not what I want, so just keep the rate to 70% of the average when Go is not GC
			rate = f.rateAVG * 0.7
			f.phase = GC
		}
	} else { // Stable phase
		// Calculate the average rate when switching from Idle -> Stable phase
		// This value can be seen as the comfort zone for the rate when Go is not GCing.
		if f.phase == Idle {
			if f.count == 0 {
				f.rateAVG = rate
			} else {
				f.rateAVG = f.rateAVG*float64(f.count)/float64(f.count+1) +
					rate/float64(f.count+1)
			}
			f.count++
			// fmt.Println("keep rate?", rate, "usage:", usage, "elapse:", fb.elapse, "rateAVG:", f.rateAVG)
		}
		f.phase = Stable
	}

	// set a upper/lower bound for the rate
	// If there is no upper bound for the rate, it goes to +INF when the workload can't
	// make the CPU utilization full. And +INF for rate does not make sense in that case.
	if rate > f.initRate {
		rate = f.initRate
	}
	if rate < 0 {
		rate = 0
	}
	// if rate < f.rateAVG * 0.7 {
	// 	fmt.Println("low rate?", rate, "usage:", usage, "elapse:", fb.elapse, "rateAVG:", f.rateAVG)
	// }
	return rate
}

// cpuLoadFeedback monitor the current CPU utilization as feedback for the mutator's rate.
func cpuLoadFeedback(feedbackCh chan<- feedbackData, initRate float64) {
	// linux kernel ticker work at 100Hz by default, so the same value (10ms) is choosed here.
	// If the values is too large, the CPU load result for a period is accurate, but the rate feedback become dull.
	// If the values is too small, I doubt the getProcessCPUTime() to be accurate. Because the implementation might
	// choose reading /proc/{pid}/stat, and the later count the kernel's clock ticks.
	ticker := time.NewTicker(10 * time.Millisecond)

	lastSys, lastUsr := getProcessCPUTime()
	lastWallClock := time.Now()
	for i := 0; ; i++ {
		now := <-ticker.C

		// Get the expected CPU utilization, it's 'elapsed wall clock' * 'expected rate'
		elapse := now.Sub(lastWallClock)
		lastWallClock = now

		// Get the actual CPU utilization
		sys, usr := getProcessCPUTime()
		s := sys.Sub(lastSys)
		u := usr.Sub(lastUsr)
		lastSys = sys
		lastUsr = usr
		cpuTime := s + u

		feedbackCh <- feedbackData{elapse, cpuTime}
	}
}

func getProcessCPUTime() (time.Time, time.Time) {
	// TODO: Support more platforms? Getrusage() is a linux API.
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

// LoadOption controls the `load` of the CPU usage, default to 1
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

// WeightedOption make the scheduling support weighted task group, rather than regarding as all equal.
func WeightedOption(v bool) Option {
	return func(c *config) {
		c.weighted = v
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
	now := time.Now()
	if s.cfg.weighted {
		return (float64(s.cfg.timeSlice)/float64(now.Sub(di.startTime)))*di.weight <
			(float64(s.cfg.timeSlice)/float64(now.Sub(dj.startTime)))*dj.weight
	}
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
