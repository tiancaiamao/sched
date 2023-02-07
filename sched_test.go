package sched

import (
	"context"
	"sync"
	"testing"
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
