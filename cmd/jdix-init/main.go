// Command jdix-init is PID 1 inside the bubblewrap namespace.
//
// bwrap execs it as the sandbox's first process. It reaps orphans, serves the
// data plane over a unix socket that only execd can reach, and — on the
// capadmin tier — drops privileges before any tenant code runs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"jdix.io/sandbox/pkg/initd"
	"jdix.io/sandbox/pkg/isolation"
)

func main() {
	var (
		subreaper = flag.Bool("subreaper", false, "adopt orphaned children in the container PID namespace")
		socket    = flag.String("socket", "/run/jdix/init.sock", "unix socket to serve the data plane on")
		workspace = flag.String("workspace", "/workspace", "default working directory for commands")
		dropTo    = flag.String("drop-to", "", "uid:gid to drop to before serving (capadmin tier only)")
		logLevel  = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()
	if *subreaper {
		if err := isolation.CheckFilesystemView(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := isolation.BecomeSubreaper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := isolation.ProtectSupervisor(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	log := newLogger(*logLevel).With("component", "jdix-init", "pid", os.Getpid())

	if err := run(log, *socket, *workspace, *dropTo); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, socket, workspace, dropTo string) error {
	srv := initd.New(log, workspace)

	// Reaping starts before anything can fork, so no exit status is missed.
	stopReaper := make(chan struct{})
	go srv.Reaper().Run(stopReaper)
	defer close(stopReaper)

	// The listener is created while still privileged; the socket's 0600 mode is
	// what keeps it to execd afterwards.
	ln, err := initd.Listen(socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socket, err)
	}
	defer ln.Close()

	if dropTo != "" {
		uid, gid, err := parseIDs(dropTo)
		if err != nil {
			return err
		}
		if err := initd.DropPrivileges(uid, gid); err != nil {
			// Continuing as root would silently give tenant code the privileges
			// the tier was supposed to remove, so this is fatal.
			return fmt.Errorf("drop privileges to %s: %w", dropTo, err)
		}
		log.Info("dropped privileges", "uid", uid, "gid", gid)
	}

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// No read timeout: exec streams and PTY sessions are long-lived by
		// design. Runaway commands are bounded by their own deadline instead.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()
	log.Info("serving", "socket", socket, "workspace", workspace)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func parseIDs(s string) (uid, gid int, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("--drop-to must look like uid:gid, got %q", s)
	}
	if uid, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, fmt.Errorf("--drop-to uid: %w", err)
	}
	if gid, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, fmt.Errorf("--drop-to gid: %w", err)
	}
	if uid == 0 || gid == 0 {
		return 0, 0, errors.New("--drop-to must not target root")
	}
	return uid, gid, nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
