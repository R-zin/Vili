// Command vili is the Vili terminal chat client. It is a thin wrapper over
// internal/cli: it wires os stdin/stdout and a cancellable context (SIGINT)
// into cli.Run and translates the returned error into an exit code.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/R-zin/vili/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := cli.Run(ctx, os.Stdout, os.Stdin, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vili:", err)
		os.Exit(1)
	}
}
