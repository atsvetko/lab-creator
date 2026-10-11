//go:build !windows

package main

// pauseIfDoubleClicked is a no-op off Windows.
func pauseIfDoubleClicked() {}
