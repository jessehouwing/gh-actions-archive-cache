package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/jessehouwing/gh-actions-archive-cache/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := app.NewCommand(os.Stdout, os.Stderr).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
