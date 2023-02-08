## Why?

Go runtime has no priority-based scheduling (barely FIFO), the scheduling latency is a big problem for some real-time task.

See a blog post (Chinese) https://www.zenlife.tk/go-manual-scheduler.md

## Prerequisites 

Need to patch the Go https://github.com/golang/go/pull/51347

## Watch the Demo

[![asciicast](https://asciinema.org/a/558157.svg)](https://asciinema.org/a/558157?t=02:25&speed=2)

## Run it yourself

Run the [demo/main.go](demo/main.go).

```
cd demo
go build
GOMAXPROCS=9 ./demo
```

```bash
# feel free to tune the parameters below if you like
# you may need to change the `GOMAXPROCS=9` and `-c 10` with machine's CPU < 10 to see the effect.

# cmd 1
while true; do sleep 1;ab -s10000000 -c 10 -n 60 http://127.0.0.1:8080/delay1ms; done

# cmd 2
while true; do sleep 1;ab -s10000000 -c 10 -n 60 http://127.0.0.1:8080/checksumWithoutScheduling; done

# cmd 3
while true; do sleep 1;ab -s10000000 -c 10 -n 60 http://127.0.0.1:8080/checksumWithScheduling; done

# cmd 4
while true; do sleep 1;ab -s10000000 -c 10 -n 60 http://127.0.0.1:8080/checksumSmallTaskWithScheduling; done
```

Step 1: Killall already existing cmd `x`, then run the cmd 1.

Step 2: Killall already existing cmd `x`, then run the cmd 1 and cmd 2 simultaneously.

Step 3: Killall already existing cmd `x`, then run the cmd 1 and cmd 3 simultaneously.

Step 4: Killall already existing cmd `x`, then run the cmd 1, cmd 3 and cmd 4 simultaneously.

Please watch the latency which cmd 1 and cmd 4 yields carefully at every step and then you would catch the difference :-D

**The point is, with scheduling, the CPU dense task would not affect the latency of the others**

## Acknowledgments 

* Inspired by [cpuworker](https://github.com/hnes/cpuworker) with better API and improving usability.
* Inspired by cockroachdb's awesome [blog](https://www.cockroachlabs.com/blog/rubbing-control-theory/)

Thanks to their pioneering work, I am convinced that manually scheduling over the Go runtime is possible.
