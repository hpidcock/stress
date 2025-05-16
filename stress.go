// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || windows

// The stress utility is intended for catching sporadic failures.
// It runs a given process in parallel in a loop and collects any failures.
// Usage:
//
//	$ stress ./fmt.test -test.run=TestSometing -test.cpu=10
//
// You can also specify a number of parallel processes with -p flag;
// instruct the utility to not kill hanged processes for gdb attach;
// or specify the failure output you are looking for (if you want to
// ignore some other sporadic failures).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	flagCount    = flag.Int("count", 0, "stop after `N` runs (default never stop)")
	flagDuration = flag.Duration("duration", 0, "minimum duration of stress (count is ignored until this duration)")
	flagFailure  = flag.String("failure", "", "fail only if output matches `regexp`")
	flagFast     = flag.Bool("fast", false, "fail fast on first error")
	flagIgnore   = flag.String("ignore", "", "ignore failure if output matches `regexp`")
	flagKill     = flag.Bool("kill", true, "kill timed out processes if true, otherwise just print pid (to attach with gdb)")
	flagOutput   = flag.String("o", defaultPrefix(), "output failure logs to `path` plus a unique suffix")
	flagP        = flag.Int("p", runtime.NumCPU(), "run `N` processes in parallel")
	flagTimeout  = flag.Duration("timeout", 10*time.Minute, "timeout each process after `duration`")
)

func init() {
	flag.Usage = func() {
		os.Stderr.WriteString(`The stress utility is intended for catching sporadic failures.
It runs a given process in parallel in a loop and collects any failures.
Usage:

	$ stress ./fmt.test -test.run=TestSometing -test.cpu=10

`)
		flag.PrintDefaults()
	}
}

func defaultPrefix() string {
	date := time.Now().Format("go-stress-20060102T150405-")
	return filepath.Join(os.TempDir(), date)
}

func main() {
	flag.Parse()
	if *flagP <= 0 || *flagTimeout <= 0 || len(flag.Args()) == 0 {
		flag.Usage()
		os.Exit(1)
	}
	var failureRe, ignoreRe *regexp.Regexp
	if *flagFailure != "" {
		var err error
		if failureRe, err = regexp.Compile(*flagFailure); err != nil {
			fmt.Println("bad failure regexp:", err)
			os.Exit(1)
		}
	}
	if *flagIgnore != "" {
		var err error
		if ignoreRe, err = regexp.Compile(*flagIgnore); err != nil {
			fmt.Println("bad ignore regexp:", err)
			os.Exit(1)
		}
	}
	res := make(chan []byte)
	start := time.Now()
	var started atomic.Int64
	var wg sync.WaitGroup
	dying := make(chan struct{})
	for range *flagP {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				passedMinDuration := time.Since(start) > *flagDuration
				// Note: Must started.Add(1) even if not using -count,
				// because it enables the '%d active' print below.
				if started.Add(1) > int64(*flagCount) && *flagCount > 0 && passedMinDuration {
					break
				}
				cmd := exec.Command(flag.Args()[0], flag.Args()[1:]...)
				var buf bytes.Buffer
				cmd.Stdout = &buf
				cmd.Stderr = &buf
				err := cmd.Start() // make cmd.Process valid for timeout goroutine
				done := make(chan bool)
				if err == nil && *flagTimeout > 0 {
					go func() {
						select {
						case <-done:
							return
						case <-time.After(*flagTimeout):
						case <-dying:
						}
						if !*flagKill {
							fmt.Printf("process %v timed out\n", cmd.Process.Pid)
							return
						}
						cmd.Process.Signal(syscall.SIGABRT)
						select {
						case <-done:
							return
						case <-time.After(10 * time.Second):
						}
						cmd.Process.Kill()
					}()
				}
				if err == nil {
					err = cmd.Wait()
				}
				out := buf.Bytes()
				close(done)
				if err != nil && (failureRe == nil || failureRe.Match(out)) && (ignoreRe == nil || !ignoreRe.Match(out)) {
					out = append(out, fmt.Sprintf("\n\nERROR: %v\n", err)...)
				} else {
					out = []byte{}
				}
				res <- out
			}
		}()
	}
	runs, fails := 0, 0
	ticker := time.NewTicker(5 * time.Second).C
	status := func(context string) {
		elapsed := time.Since(start).Truncate(time.Second)
		var pct string
		if fails > 0 {
			pct = fmt.Sprintf(" (%0.2f%%)", 100.0*float64(fails)/float64(runs))
		}
		var active string
		n := started.Load() - int64(runs)
		if *flagDuration == 0 && *flagCount > 0 {
			// started counts past *flagCount at end; do not count those
			// TODO: n = min(n, int64(*flagCount-runs))
			if x := int64(*flagCount - runs); n > x {
				n = x
			}
		}
		if n > 0 {
			active = fmt.Sprintf(", %d active", n)
		}
		fmt.Printf("%v: %v runs %s, %v failures%s%s\n", elapsed, runs, context, fails, pct, active)
	}
	runResult := func(out []byte, ignoreErr bool) {
		runs++
		if !ignoreErr && len(out) > 0 {
			fails++
			dir, path := filepath.Split(*flagOutput)
			f, err := os.CreateTemp(dir, path)
			if err != nil {
				fmt.Printf("failed to create temp file: %v\n", err)
				os.Exit(1)
			}
			f.Write(out)
			f.Close()
			if len(out) > 2<<10 {
				out := out[:2<<10]
				fmt.Printf("\n%s\n%s\n…\n", f.Name(), out)
			} else {
				fmt.Printf("\n%s\n%s\n", f.Name(), out)
			}
		}
	}
	waitExit := func() {
		dead := make(chan struct{})
		go func() {
			defer close(dead)
			wg.Wait()
		}()
		close(dying)
		for {
			select {
			case out := <-res:
				runResult(out, true)
			case <-dead:
				return
			}
		}
	}
	for {
		select {
		case out := <-res:
			runResult(out, false)
			if *flagFast && fails > 0 {
				waitExit()
				status("total")
				os.Exit(1)
			}
			if *flagDuration != 0 && time.Since(start) < *flagDuration {
				continue
			} else if *flagCount > 0 && runs >= *flagCount {
				waitExit()
				status("total")
				if fails > 0 {
					os.Exit(1)
				}
				os.Exit(0)
			}
		case <-ticker:
			status("so far")
		}
	}
}
