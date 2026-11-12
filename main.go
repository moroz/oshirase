// oshirase (お知らせ, "notice") carries desktop notifications from a VM to
// the machine it runs on. `oshirase serve` runs on the host and shows them;
// `oshirase send` runs in the guest and asks for one.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage:
  oshirase serve [-listen addr] [-backend name]   run the daemon (host)
  oshirase send [flags] <title> [body]            show a notification (guest)
  oshirase version
`

func main() {
	log.SetFlags(log.LstdFlags)

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	command := os.Args[1]
	rest := os.Args[2:]

	var err error
	switch command {
	case "serve":
		err = runServe(rest)
	case "send":
		err = runSend(rest)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err == nil {
		return
	}
	if errors.Is(err, errUsage) || errors.Is(err, flag.ErrHelp) {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "oshirase:", err)
	os.Exit(1)
}
