// Command vidyom is the Vidyom client. See internal/cli.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rafaelp/vidyom/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first signal cancels the context, which disconnects streams and stops
	// uploads at a checkpoint. Restoring the default action lets a second Ctrl+C end
	// the process at once (I174).
	go func() {
		<-ctx.Done()
		stop()
	}()
	return cli.Execute(ctx, os.Args[1:])
}
