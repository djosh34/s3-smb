// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"log"
	"os"
)

func main() {
	var err error
	if len(os.Args) != 2 {
		err = errors.New("usage: fullsync <new file on the mounted share>")
	} else {
		err = writeFullSync(os.Args[1], fullSync)
	}
	if err != nil {
		log.New(os.Stderr, "fullsync: ", 0).Print(err)
		os.Exit(1)
	}
}
