package sched

import (
	"context"
	"fmt"
	"hash/crc32"
	"io/ioutil"
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestExample(t *testing.T) {
	go Scheduler()

	ctx, _ := NewTaskGroup(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	ctx1 := WithSchedInfo(ctx)
	go func(ctx context.Context) {
		for x := 0; x < 100; x++ {
			for i := 0; i < 10000000; i++ {
				f(i)
			}
			CheckPoint(ctx)
		}
		wg.Done()
	}(ctx1)

	CheckPoint(ctx)
	wg.Wait()
}

func f(i int) {
}

var glCrc32bs = make([]byte, 1024*256)

func cpuIntensiveTask(ctx context.Context) uint32 {
	amt := 10000 + rand.Intn(10000)
	var ck uint32
	for range make([]struct{}, amt) {
		ck = crc32.ChecksumIEEE(glCrc32bs)
		CheckPoint(ctx)
	}
	return ck
}

func allocIntensiveTask(m map[int][]byte, ctx context.Context) {
	// m := make(map[int][]byte)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 100000; i++ {
		idx := r.Intn(4000000)
		sz := 8 + r.Intn(248)
		m[idx] = make([]byte, sz)

		if i%20 == 0 {
			CheckPoint(ctx)
		}
	}
}

func TestT(t *testing.T) {
	// go Scheduler(LoadOption(0.50), TimeSliceOption(10*time.Millisecond))
	go Scheduler(LoadOption(0.8), TimeSliceOption(10*time.Millisecond))
	for i := 0; i < 12; i++ {
		ctx, _ := NewTaskGroup(context.Background())
		switch i % 3 {
		case 0:
			Go(ctx, func(ctx context.Context) {
				for {
					cpuIntensiveTask(ctx)
				}
			})
		case 1:
			Go(ctx, func(ctx context.Context) {
				m := make(map[int][]byte)
				for {
					allocIntensiveTask(m, ctx)
				}
			})
		case 2:
			Go(ctx, func(ctx context.Context) {
				for {
					m := make(map[int][]byte)
					allocIntensiveTask(m, ctx)
				}
			})
		}
	}
	go untracked()
	http.ListenAndServe(":10080", nil)
}

var g = 33

func untracked() {
	for {
		for i := 0; i < 1000; i++ {
			g += i
		}
	}
}

func TestX(t *testing.T) {
	var r syscall.Rusage

	for i := 0; i < 5; i++ {
		go func() {
			for {
				amt := 10000 + rand.Intn(10000)
				for range make([]struct{}, amt) {
					crc32.ChecksumIEEE(glCrc32bs)
				}
			}
		}()
	}

	sysTimeStart := time.Unix(r.Stime.Sec, r.Stime.Usec*1000)
	usrTimeStart := time.Unix(r.Utime.Sec, r.Utime.Usec*1000)

	for {
		time.Sleep(time.Second)
		err := syscall.Getrusage(syscall.RUSAGE_SELF, &r)
		if err != nil {
			panic(err)
		}
		sysTimeEnd := time.Unix(r.Stime.Sec, r.Stime.Usec*1000)
		usrTimeEnd := time.Unix(r.Utime.Sec, r.Utime.Usec*1000)

		fmt.Println("Sys time:", sysTimeEnd.Sub(sysTimeStart))
		fmt.Println("Usr time:", usrTimeEnd.Sub(usrTimeStart))
	}
}

// BenchmarkGetrsusage-16           3848857               313.1 ns/op             0 B/op          0 allocs/op
func BenchmarkGetrsusage(b *testing.B) {
	var r syscall.Rusage
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := syscall.Getrusage(syscall.RUSAGE_SELF, &r)
		if err != nil {
			panic(err)
		}
	}
}

// BenchmarkGetCPUTime-16            144364              8238 ns/op            2097 B/op          9 allocs/op
func BenchmarkGetCPUTime(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := getCPUTime()
		if err != nil {
			panic(err)
		}
	}
}

// BenchmarkReadFile-16              172177              6886 ns/op             848 B/op          5 allocs/op
func BenchmarkReadFile(b *testing.B) {
	pid := os.Getpid()
	path := fmt.Sprintf("/proc/%d/stat", pid)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ioutil.ReadFile(path)
		if err != nil {
			panic(err)
		}
	}
}

// getCPUTime returns the cumulative user/system time (in ms) since the process start.
func getCPUTime() (userTimeMillis, sysTimeMillis int64, err error) {
	pid := os.Getpid()
	path := fmt.Sprintf("/proc/%d/stat", pid)
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}

	fields := strings.Fields(string(contents))
	const systemTicks = 100

	user, _ := strtoull(fields[13])
	sys, _ := strtoull(fields[14])
	// convert to millis
	selfUser := user * (1000 / systemTicks)
	selfSys := sys * (1000 / systemTicks)
	// selfTotal := selfUser + selfSys

	// // convert to millis
	// self.StartTime, _ = strtoull(fields[21])
	// self.StartTime /= systemTicks
	// self.StartTime += system.btime
	// self.StartTime *= 1000

	return int64(selfUser), int64(selfSys), nil
}

func strtoull(val string) (uint64, error) {
	return strconv.ParseUint(val, 10, 64)
}
