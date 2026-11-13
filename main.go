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
	"runtime/debug"
)

// version is the module version Go stamps into the binary: the tag
// (v0.1.0) for `go install ...@v0.1.0` or a build at a tagged commit, a
// pseudo-version past the last tag otherwise, with +dirty for uncommitted
// changes.
var version = buildVersion()

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	// Built outside version control, or with -buildvcs=false.
	return "devel"
}

const usage = `usage:
  oshirase serve [-listen addr] [-backend name]   run the daemon (host)
  oshirase send [flags] <title> [body]            show a notification (guest)
  oshirase withdraw -tag <tag> [flags]            take down tagged notifications
  oshirase auth google|fastmail|smtp [flags]      grant the email backend (host)
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
	case "withdraw":
		err = runWithdraw(rest)
	case "auth":
		err = runAuth(rest)
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
