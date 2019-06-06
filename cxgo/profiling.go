package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"time"
)

var profile bool
var profiles map[string]int64 = map[string]int64{}

func StartProfile(name string) {
	if profile {
		profiles[name] = time.Now().UnixNano()
	}
}

func StopProfile(name string) {
	if profile {
		t := time.Now().UnixNano()
		deltaTime := t - profiles[name]
		fmt.Printf("%s : %dms\n", name, deltaTime/(int64(time.Millisecond)/int64(time.Nanosecond)))
	}
}

func StartCPUProfile() *os.File {
	if profile {
		f, err := os.Create(os.Args[0] + "_cpu.pprof")
		if err != nil {
			fmt.Println("Failed to create CPU profile: ", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Println("Failed to start CPU profile: ", err)
		}
		return f
	}
	return nil
}

func StopCPUProfile(f *os.File) {
	if profile {
		if f != nil {
			defer f.Close()
		}
		defer pprof.StopCPUProfile()
	}
}

func DumpMEMProfile() {
	if profile {
		f, err := os.Create(os.Args[0] + "_mem.pprof")
		if err != nil {
			fmt.Println("Failed to create MEM profile: ", err)
		}
		defer f.Close()
		runtime.GC()
		if err := pprof.WriteHeapProfile(f); err != nil {
			fmt.Println("Failed to write MEM profile: ", err)
		}
	}
}