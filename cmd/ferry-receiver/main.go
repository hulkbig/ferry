// Command ferry-receiver is the only command authorized by the dedicated
// production SSH key. It rejects interactive use, validates the repository-
// bound envelope, and executes fixed Go/argv operations without a shell.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"ferry/internal/buildinfo"
	"ferry/internal/receiver"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-version-json" {
		_ = json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
		return
	}
	abortPrepared := len(os.Args) == 4 && os.Args[1] == "abort-prepared"
	cleanup := len(os.Args) >= 2 && os.Args[1] == "cleanup"
	backupNow := len(os.Args) == 3 && os.Args[1] == "scheduled-backup" && os.Args[2] == "--now"
	if !cleanup && !abortPrepared && !backupNow && (len(os.Args) != 2 || (os.Args[1] != "forced" && os.Args[1] != "scheduled-backup")) {
		fmt.Fprintln(os.Stderr, "ferry receiver requires forced, scheduled-backup [--now], cleanup [--apply], or abort-prepared <release-sha> <operation-id>")
		os.Exit(2)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "ferry receiver must run through the root forced-command key")
		os.Exit(1)
	}
	originalCommand := os.Getenv("SSH_ORIGINAL_COMMAND")
	os.Clearenv()
	_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	_ = os.Setenv("LANG", "C.UTF-8")
	_ = os.Setenv("SSH_ORIGINAL_COMMAND", originalCommand)
	// A dropped Actions SSH transport must not strand a half-applied release.
	// The receiver continues its local commit, whose error path rolls back.
	signal.Ignore(syscall.SIGHUP)
	environment := receiver.DefaultEnvironment(os.Stdin, os.Stdout)
	if cleanup {
		if originalCommand != "" {
			fmt.Fprintln(os.Stderr, "cleanup rejects SSH command context")
			os.Exit(1)
		}
		options, err := environment.CleanupDefaults()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cleanup policy:", err)
			os.Exit(1)
		}
		flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
		flags.BoolVar(&options.Apply, "apply", false, "apply the freshly computed plan; default is preview")
		flags.DurationVar(&options.MinAge, "min-age", options.MinAge, "minimum age, at least 1h")
		flags.DurationVar(&options.MaxAge, "max-age", options.MaxAge, "maximum age for unreferenced temporary copies")
		flags.Int64Var(&options.MaxBytes, "max-bytes", options.MaxBytes, "allocated-byte budget; protected copies can exceed it")
		if err = flags.Parse(os.Args[2:]); err != nil || flags.NArg() != 0 {
			os.Exit(2)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		report, err := environment.Cleanup(ctx, options)
		if report != nil {
			_ = json.NewEncoder(os.Stdout).Encode(report)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "cleanup:", err)
			os.Exit(1)
		}
		return
	}
	if abortPrepared {
		if originalCommand != "" {
			fmt.Fprintln(os.Stderr, "abort-prepared rejects SSH command context")
			os.Exit(1)
		}
		if err := environment.AbortPrepared(context.Background(), os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, "abort prepared:", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "scheduled-backup" {
		if originalCommand != "" {
			fmt.Fprintln(os.Stderr, "scheduled backup rejects SSH command context")
			os.Exit(1)
		}
		var err error
		if backupNow {
			err = environment.BackupNow(context.Background())
		} else {
			err = environment.ScheduledBackup(context.Background())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "scheduled backup:", err)
			os.Exit(1)
		}
		return
	}
	request, err := receiver.ParseOriginalCommand(originalCommand)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request rejected:", err)
		os.Exit(1)
	}
	if err := environment.Execute(context.Background(), request); err != nil {
		fmt.Fprintln(os.Stderr, "receiver:", err)
		os.Exit(1)
	}
}
