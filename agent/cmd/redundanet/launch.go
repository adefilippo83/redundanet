package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// `launch` is what the macOS service starts instead of the agent itself: a
// copy of this binary that never changes. macOS keys the permissions a
// program is granted (Documents, Desktop, Downloads, Full Disk Access) to
// the program's code identity, and an ad-hoc signed binary's identity is
// the hash of the file, so every self-update would be a new program to
// macOS and the grant would lapse. The launcher starts the agent as its
// child, restarts it when it exits (after an update, say) and stops it when
// launchd stops the service; macOS attributes a child's file access to the
// process launchd started, so the grant given once to the launcher covers
// every version of the agent underneath. `service install` makes the copy
// and points launchd at it (service.go).

// launchBackoff is the pause before the program is started again; a
// program that died within launchQuick of starting waits five times that.
var (
	launchBackoff = 2 * time.Second
	launchQuick   = 10 * time.Second
)

func runLaunch(args []string) error {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return errors.New("usage: launch -- EXECUTABLE ARGS...")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return launchLoop(ctx, rest[0], rest[1:], os.Stderr)
}

// launchLoop runs the program until ctx ends, starting it again whenever
// it exits, and passes the stop on to it.
func launchLoop(ctx context.Context, exe string, args []string, logw io.Writer) error {
	for {
		started := time.Now()
		cmd := exec.Command(exe, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(logw, "launcher: %s did not start: %v\n", exe, err)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					fmt.Fprintf(logw, "launcher: %s exited; starting it again\n", exe)
				} else {
					fmt.Fprintf(logw, "launcher: %s exited (%v); starting it again\n", exe, err)
				}
			case <-ctx.Done():
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					_ = cmd.Process.Kill()
					<-done
				}
				return nil
			}
		}
		wait := launchBackoff
		if time.Since(started) < launchQuick {
			wait = 5 * launchBackoff
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
