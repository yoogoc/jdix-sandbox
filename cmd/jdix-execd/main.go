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
		dataAddr     = flag.String("data-addr", ":8080", "data plane listen address")
		controlAddr  = flag.String("control-addr", ":8081", "cluster-internal control plane listen address")
		bwrapPath    = flag.String("bwrap", "/opt/jdix/bin/bwrap", "path to the bubblewrap binary")
		volumesFlag  = flag.String("volumes", "", "template-declared volumes as name=path,name=path")
		tierOverride = flag.String("tier", "", "skip detection and assert this tier (testing only)")
		tokenFile    = flag.String("internal-token-file", "", "file holding the control-plane bearer token")
		logLevel     = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()

	log := newLogger(*logLevel).With("component", "jdix-execd")

	layout := bwrap.DefaultLayout()
	if err := prepare(layout); err != nil {
		log.Error("preparing layout", "err", err)
		os.Exit(1)
	}

	probe := isolation.Detect(context.Background(), *bwrapPath)
	if *tierOverride != "" {
		probe.Tier = bwrap.Tier(*tierOverride)
		probe.Reason = "overridden on the command line"
	}
	log.Info("isolation measured", "tier", probe.Tier, "reason", probe.Reason)

	token, err := readToken(*tokenFile)
	if err != nil {
		log.Error("reading control-plane token", "err", err)
		os.Exit(1)
	}

	srv := execd.New(log, execd.Config{
		Layout:   layout,
		Tier:     probe.Tier,
		Volumes:  parseVolumes(*volumesFlag),
		Launcher: execd.BwrapLauncher(*bwrapPath),
		UID:      1000,
		GID:      1000,
	}, probe, token)

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

func readToken(path string) (string, error) {
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
