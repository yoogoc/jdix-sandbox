// Command jdix-execd runs as the sandbox container's PID 1, outside the
// bubblewrap namespace.
//
// It measures what the node can enforce, waits to be bound to a sandbox, builds
// the namespace when that happens, and proxies the data plane into it.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"jdix.io/sandbox/pkg/bwrap"
	"jdix.io/sandbox/pkg/execd"
	"jdix.io/sandbox/pkg/isolation"
)

func main() {
	var (
		filesystemMode = flag.String("filesystem-isolation", "", "bwrap directory view in the container PID namespace")
		probeChild     = flag.Bool("filesystem-probe-child", false, "internal: validate a completed filesystem view")
		dataAddr       = flag.String("data-addr", ":8080", "data plane listen address")
		controlAddr    = flag.String("control-addr", ":8081", "cluster-internal control plane listen address")
		bwrapPath      = flag.String("bwrap", "/opt/jdix/bin/bwrap", "path to the bubblewrap binary")
		volumesFlag    = flag.String("volumes", "", "template-declared volumes as name=path,name=path")
		tierOverride   = flag.String("tier", "", "skip detection and assert this tier (testing only)")
		tokenEnv       = flag.String("internal-token-env", "JDIX_CONTROL_TOKEN",
			"environment variable holding this Pod's control-plane token; the controller sets it per Pod")
		tokenFile = flag.String("internal-token-file", "", "file holding the control-plane token, as an alternative to the environment")
		allowOpen = flag.Bool("allow-unauthenticated-control", false,
			"serve the control plane with no token at all — for local experiments only")
		logLevel = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()
	if *probeChild {
		if err := isolation.CheckFilesystemView(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("filesystem-ok")
		return
	}
	if *filesystemMode != "" && *filesystemMode != "bwrap" {
		fmt.Fprintln(os.Stderr, "unknown filesystem isolation mode")
		os.Exit(1)
	}
	if *filesystemMode != "" {
		if *tierOverride != "" {
			fmt.Fprintln(os.Stderr, "filesystem mode cannot override capability detection")
			os.Exit(1)
		}
		if err := isolation.ProtectSupervisor(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	log := newLogger(*logLevel).With("component", "jdix-execd")

	layout := bwrap.DefaultLayout()
	if err := prepare(layout); err != nil {
		log.Error("preparing layout", "err", err)
		os.Exit(1)
	}

	var probe isolation.Result
	if *filesystemMode == "bwrap" {
		probe = isolation.DetectFilesystem(context.Background(), *bwrapPath, parseVolumes(*volumesFlag))
		if probe.Tier != bwrap.TierFilesystem {
			log.Error("filesystem isolation unavailable", "reason", probe.Reason)
			os.Exit(1)
		}
	} else {
		probe = isolation.Detect(context.Background(), *bwrapPath)
	}
	if *tierOverride != "" {
		probe.Tier = bwrap.Tier(*tierOverride)
		probe.Reason = "overridden on the command line"
	}
	log.Info("isolation measured", "tier", probe.Tier, "reason", probe.Reason)

	token, err := readToken(*tokenEnv, *tokenFile)
	if err != nil {
		log.Error("reading the control-plane token", "err", err)
		os.Exit(1)
	}
	if token == "" && !*allowOpen {
		// Refuse rather than serve an open control plane. Anything that reaches
		// :8081 can bind a sandbox to itself — that is a shell inside this Pod —
		// so a missing token has to be loud. It is the kind of misconfiguration
		// that otherwise works perfectly until someone notices.
		log.Error("no control-plane token",
			"env", *tokenEnv,
			"hint", "the controller sets this per Pod; pass --internal-token-file, or --allow-unauthenticated-control if you really mean it")
		os.Exit(1)
	}
	if token == "" {
		log.Warn("control plane is unauthenticated", "reason", "--allow-unauthenticated-control")
	}

	srv := execd.New(log, execd.Config{
		Layout:   layout,
		Tier:     probe.Tier,
		Volumes:  parseVolumes(*volumesFlag),
		Launcher: execd.BwrapLauncher(*bwrapPath),
		UID:      1000,
		GID:      1000,
	}, probe, token)

	// In filesystem mode the sandbox shares this container's PID namespace, so
	// execd is the PID 1 that inherits whatever jdix-init does not adopt. It
	// has to reap, or a sandbox that never expires fills the process table and
	// nothing in it can fork any more (DESIGN §06).
	if r := srv.Reaper(); r != nil {
		stopReaper := make(chan struct{})
		go r.Run(stopReaper)
		defer close(stopReaper)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// A sandbox that outlives its TTL is the platform's number one leak, so the
	// Pod terminates itself rather than waiting to be collected. The controller
	// still sweeps as a second line of defence.
	shutdown := make(chan string, 1)
	srv.SetOnExpire(func(reason string) {
		select {
		case shutdown <- reason:
		default:
		}
	})

	data := &http.Server{Addr: *dataAddr, Handler: srv.DataHandler(), ReadHeaderTimeout: 15 * time.Second}
	control := &http.Server{Addr: *controlAddr, Handler: srv.ControlHandler(), ReadHeaderTimeout: 15 * time.Second}

	errCh := make(chan error, 2)
	go func() { errCh <- serve(data, "data", *dataAddr, log) }()
	go func() { errCh <- serve(control, "control", *controlAddr, log) }()

	var reason string
	select {
	case err := <-errCh:
		if err != nil {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
		reason = "server closed"
	case <-ctx.Done():
		reason = "signal"
	case reason = <-shutdown:
	}

	log.Info("shutting down", "reason", reason)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = data.Shutdown(shutdownCtx)
	_ = control.Shutdown(shutdownCtx)
}

func serve(s *http.Server, name, addr string, log *slog.Logger) error {
	log.Info("listening", "surface", name, "addr", addr)
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s plane: %w", name, err)
	}
	return nil
}

// prepare creates the directories bwrap will bind and writes the /etc skeleton
// the sandbox sees. Doing it here rather than in the initContainer keeps the
// two in step: the code that reads these files also writes them.
func prepare(l bwrap.Layout) error {
	for _, d := range []string{l.WorkspaceDir, l.VolumeRoot, l.IPCDir, l.SkelDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// The IPC directory holds the socket that reaches into the namespace.
	if err := os.Chmod(l.IPCDir, 0o700); err != nil {
		return err
	}
	// The sandbox runs as uid 1000 and must be able to write its workspace.
	_ = os.Chown(l.WorkspaceDir, 1000, 1000)

	skel := map[string]string{
		"passwd": "root:x:0:0:root:/root:/bin/sh\nsandbox:x:1000:1000:sandbox:/home:/bin/sh\n",
		"group":  "root:x:0:\nsandbox:x:1000:\n",
	}
	for name, content := range skel {
		p := filepath.Join(l.SkelDir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	// resolv.conf is copied from the Pod so the sandbox resolves names the same
	// way the container does; a hand-written file would drift from the cluster's
	// DNS configuration.
	dst := filepath.Join(l.SkelDir, "resolv.conf")
	if _, err := os.Stat(dst); err != nil {
		b, err := os.ReadFile("/etc/resolv.conf")
		if err != nil {
			b = []byte("nameserver 127.0.0.53\n")
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func parseVolumes(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, path, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(path)
	}
	return out
}

// readToken prefers the environment, because that is how the controller hands
// each Pod its own credential. A file is still accepted for deployments that
// mount one.
func readToken(envName, path string) (string, error) {
	if envName != "" {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			return v, nil
		}
	}
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
