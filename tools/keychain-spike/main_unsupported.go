//go:build !darwin || !cgo

package main

import "os"

func main() {
	os.Exit(2)
}
