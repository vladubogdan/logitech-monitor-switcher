//go:build !darwin

package main

import "fmt"

func probeDDCRaw() { fmt.Println("ddcraw is macOS-only") }
