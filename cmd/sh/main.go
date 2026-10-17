// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Command sh is the image's /bin/sh: blairham/sh's gated shell, so that
// `kubectl exec <pod> -- /bin/sh -c '<script>'` can read /proc and
// /sys/fs/cgroup in a distroless container. It cannot exec, signal or write,
// and refuses /proc/*/environ. See
// https://github.com/blairham/sh/blob/main/gated/README.md.
//
// Only install it at /bin/sh in an image that has no other shell or userland.
// Beside busybox the gate protects nothing: `ls`, `wget` and the rest are
// still there to exec from anything that is not this binary.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/blairham/sh/gated"
)

func main() {
	os.Exit(run())
}

// run is gated's ExampleMain with the exit moved out, so the deferred stop
// runs before the process ends.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return gated.Main(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv("HOSTNAME"))
}
