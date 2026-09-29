// SPDX-License-Identifier: AGPL-3.0-only
// Command coverage refuses to turn partial test evidence into a release pass.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type event struct{ Action, Package, Test string }

func check(events, ledger string) error {
	f, err := os.Open(events)
	if err != nil {
		return err
	}
	defer f.Close()
	passed := map[string]bool{}
	unproven := map[string]bool{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 8<<20)
	for scan.Scan() {
		var e event
		if json.Unmarshal(scan.Bytes(), &e) == nil && e.Test != "" {
			name := e.Package + "/" + e.Test
			if e.Action == "run" {
				for old := range unproven {
					if old == name || strings.HasPrefix(old, name+"/") {
						delete(unproven, old)
					}
				}
				for old := range passed {
					if old == name || strings.HasPrefix(old, name+"/") {
						delete(passed, old)
					}
				}
			}
			if e.Action == "pass" {
				passed[name] = true
				delete(unproven, name)
			}
			if e.Action == "fail" || e.Action == "skip" {
				unproven[name] = true
			}
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	f2, err := os.Open(ledger)
	if err != nil {
		return err
	}
	defer f2.Close()
	scan = bufio.NewScanner(f2)
	missing := 0
	total := 0
	for scan.Scan() {
		line := scan.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			return fmt.Errorf("invalid coverage row %q", line)
		}
		total++
		complete := passed[fields[1]]
		for name := range unproven {
			if name == fields[1] || strings.HasPrefix(name, fields[1]+"/") {
				complete = false
			}
		}
		if !complete {
			fmt.Printf("UNPROVEN\t%s\t%s\n", fields[0], fields[1])
			missing++
		} else {
			fmt.Printf("EXECUTED\t%s\t%s\n", fields[0], fields[1])
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	if total == 0 || missing > 0 {
		return fmt.Errorf("release gate: %d/%d required cases lack passing execution evidence", missing, total)
	}
	return nil
}
func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: coverage go-test.json required.tsv")
		os.Exit(2)
	}
	if err := check(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
