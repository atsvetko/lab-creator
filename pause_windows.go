//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// pauseIfDoubleClicked keeps a double-clicked console window open so its output
// (the URL, or an error) is readable. When labctl is launched from an existing
// console (cmd/PowerShell) it returns at once, so scripts never block.
func pauseIfDoubleClicked() {
	if !ownsConsole() {
		return
	}
	fmt.Fprint(os.Stderr, "\nPress Enter to close this window…")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// ownsConsole reports whether this process is the only one attached to the
// console — the case when it was started by double-click, so the window will
// vanish the instant the process exits.
func ownsConsole() bool {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	var pids [4]uint32
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n <= 1
}
