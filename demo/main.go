// Copyright 2023 The sched Authors
// Copyright 2021 The cpuworker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/rand"
	"fmt"
	"hash/crc32"
	"log"
	_ "net/http/pprof"

	mathrand "math/rand"
	"net/http"
	"context"
	"time"

	"github.com/tiancaiamao/sched"
)

var glCrc32bs = make([]byte, 1024*256)

func cpuIntensiveTask(amt int) uint32 {
	var ck uint32
	for range make([]struct{}, amt) {
		ck = crc32.ChecksumIEEE(glCrc32bs)
	}
	return ck
}

func cpuIntensiveTaskWithCheckpoint(ctx context.Context, amt int) uint32 {
	var ck uint32
	for range make([]struct{}, amt) {
		ck = crc32.ChecksumIEEE(glCrc32bs)
		sched.CheckPoint(ctx)
	}
	return ck
}

func handleChecksumWithoutScheduling(w http.ResponseWriter, _ *http.Request) {
	ts := time.Now()
	ck := cpuIntensiveTask(10000 + mathrand.Intn(10000))
	w.Write([]byte(fmt.Sprintln("crc32 (without scheduling):", ck, "time cost:", time.Now().Sub(ts))))
}

func handleChecksumWithScheduling(w http.ResponseWriter, _ *http.Request) {
	ts := time.Now()
	var ck uint32
	ctx, _ := sched.NewTaskGroup(context.Background())
	ck = cpuIntensiveTaskWithCheckpoint(ctx, 10000+mathrand.Intn(10000))
	w.Write([]byte(fmt.Sprintln("crc32 (with scheduling and checkpoint):", ck, "time cost:", time.Now().Sub(ts))))
}

func handleChecksumSmallTaskWithScheduling(w http.ResponseWriter, _ *http.Request) {
	ts := time.Now()
	var ck uint32
	ck = cpuIntensiveTask(10)
	w.Write([]byte(fmt.Sprintln("crc32 (with scheduilng and small task):", ck, "time cost:", time.Now().Sub(ts))))
}

func handleDelay(w http.ResponseWriter, _ *http.Request) {
	t0 := time.Now()
	wCh := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond)
		wCh <- struct{}{}
	}()
	<-wCh
	w.Write([]byte(fmt.Sprintf("delayed 1ms, time cost %s :)\n", time.Now().Sub(t0))))
}

func handleDelayLoop(w http.ResponseWriter, _ *http.Request) {
	t0 := time.Now()
	for idx := range make([]byte, 10) {
		t0 := time.Now()
		wCh := make(chan struct{})
		go func() {
			time.Sleep(time.Millisecond)
			wCh <- struct{}{}
		}()
		<-wCh
		w.Write([]byte(fmt.Sprintf("delayed 1ms loop, idx:%d , time cost %s :)\n", idx, time.Now().Sub(t0))))
	}
	w.Write([]byte(fmt.Sprintf("delayed 1ms loop, final , total time cost %s :)\n", time.Now().Sub(t0))))
}

func handleDelayLoopWithScheduling(w http.ResponseWriter, _ *http.Request) {
	for _ = range make([]byte, 10) {
		// t0 := time.Now()
		wCh := make(chan struct{})
		go func() {
			time.Sleep(time.Millisecond)
			wCh <- struct{}{}
		}()
	}
}

func main() {
	go sched.Scheduler()
	rand.Read(glCrc32bs)

	http.HandleFunc("/checksumWithScheduling", handleChecksumWithScheduling)
	http.HandleFunc("/checksumSmallTaskWithScheduling", handleChecksumSmallTaskWithScheduling)
	http.HandleFunc("/checksumWithoutScheduling", handleChecksumWithoutScheduling)
	http.HandleFunc("/delay1ms", handleDelay)
	http.HandleFunc("/delay1msLoop", handleDelayLoop)
	http.HandleFunc("/delay1msLoopWithScheduling", handleDelayLoopWithScheduling)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
