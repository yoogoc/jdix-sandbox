package initd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"jdix.io/sandbox/pkg/api"
)

type harness struct {
	t    *testing.T
	srv  *Server
	http *httptest.Server
	ws   string // workspace dir
	ro   string // read-only root
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	ro := filepath.Join(base, "corpus")
	for _, d := range []string{ws, ro} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ro, "readme.txt"), []byte("corpus"), 0o644); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(log, ws)
	stop := make(chan struct{})
	go srv.Reaper().Run(stop)
	t.Cleanup(func() { close(stop) })

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	h := &harness{t: t, srv: srv, http: ts, ws: ws, ro: ro}
	h.configure(api.ConfigureRequest{
		SandboxID: "sbx_test",
		Env:       map[string]string{"RUN_ID": "42"},
		Secrets:   map[string]string{"TOKEN": "s3cr3t"},
		Workspace: ws,
		Roots:     []api.Root{{Path: ws}, {Path: ro, ReadOnly: true}},
	})
	return h
}

func (h *harness) configure(req api.ConfigureRequest) {
	h.t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(h.http.URL+"/configure", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("configure: %d %s", resp.StatusCode, b)
	}
}

func (h *harness) exec(req api.ExecRequest) api.ExecResult {
	h.t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(h.http.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("exec: %d %s", resp.StatusCode, b)
	}
	var out api.ExecResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func TestExecBasics(t *testing.T) {
	h := newHarness(t)

	if got := h.exec(api.ExecRequest{Cmd: "echo hello"}); strings.TrimSpace(got.Stdout) != "hello" || got.ExitCode != 0 {
		t.Fatalf("got %+v", got)
	}
	if got := h.exec(api.ExecRequest{Cmd: "exit 7"}); got.ExitCode != 7 {
		t.Fatalf("exit code: got %d want 7", got.ExitCode)
	}
	if got := h.exec(api.ExecRequest{Cmd: "echo oops >&2"}); strings.TrimSpace(got.Stderr) != "oops" {
		t.Fatalf("stderr: got %q", got.Stderr)
	}
	// argv form must not go through a shell, so shell metacharacters stay literal.
	got := h.exec(api.ExecRequest{Argv: []string{"/bin/echo", "a;b"}})
	if strings.TrimSpace(got.Stdout) != "a;b" {
		t.Fatalf("argv form: got %q", got.Stdout)
	}
}

func TestExecSeesEnvAndSecrets(t *testing.T) {
	h := newHarness(t)
	got := h.exec(api.ExecRequest{Cmd: "echo $RUN_ID-$TOKEN"})
	if strings.TrimSpace(got.Stdout) != "42-s3cr3t" {
		t.Fatalf("env/secret injection failed: %q", got.Stdout)
	}
	// A per-call value overrides the sandbox default.
	got = h.exec(api.ExecRequest{Cmd: "echo $RUN_ID", Env: map[string]string{"RUN_ID": "99"}})
	if strings.TrimSpace(got.Stdout) != "99" {
		t.Fatalf("per-call env should win: %q", got.Stdout)
	}
}

func TestExecRunsInWorkspaceByDefault(t *testing.T) {
	h := newHarness(t)
	got := h.exec(api.ExecRequest{Cmd: "pwd"})
	// macOS reports /private/var for /var, so compare the resolved forms.
	want, _ := filepath.EvalSymlinks(h.ws)
	gotPath, _ := filepath.EvalSymlinks(strings.TrimSpace(got.Stdout))
	if gotPath != want {
		t.Fatalf("cwd: got %q want %q", gotPath, want)
	}
}

func TestExecTimeoutKillsTheProcessGroup(t *testing.T) {
	h := newHarness(t)
	start := time.Now()
	got := h.exec(api.ExecRequest{Cmd: "sleep 30", TimeoutSeconds: 1})
	if elapsed := time.Since(start); elapsed > 12*time.Second {
		t.Fatalf("timeout did not fire promptly: %s", elapsed)
	}
	if got.ExitCode == 0 {
		t.Fatalf("a killed command must not report success: %+v", got)
	}
}

func TestExecTruncatesRunawayOutput(t *testing.T) {
	h := newHarness(t)
	got := h.exec(api.ExecRequest{
		Cmd:            "yes abcdefghij | head -c 100000",
		MaxOutputBytes: 1024,
		TimeoutSeconds: 30,
	})
	if !got.Truncated {
		t.Fatal("expected Truncated to be set")
	}
	if len(got.Stdout) > 1024 {
		t.Fatalf("output not capped: %d bytes", len(got.Stdout))
	}
}

func TestFileRoundTrip(t *testing.T) {
	h := newHarness(t)
	target := h.ws + "/sub/dir/main.py"

	req, _ := http.NewRequest(http.MethodPut, h.http.URL+"/v1/files?path="+target, strings.NewReader("print('hi')"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d", resp.StatusCode)
	}

	resp, err = http.Get(h.http.URL + "/v1/files?path=" + target)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "print('hi')" {
		t.Fatalf("get returned %q", body)
	}

	resp, err = http.Get(h.http.URL + "/v1/files/list?path=" + h.ws + "/sub/dir")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Entries []api.FileInfo `json:"entries"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Entries) != 1 || list.Entries[0].Name != "main.py" {
		t.Fatalf("list: %+v", list.Entries)
	}

	req, _ = http.NewRequest(http.MethodDelete, h.http.URL+"/v1/files?path="+target, nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(h.ws, "sub/dir/main.py")); !os.IsNotExist(err) {
		t.Fatal("file survived delete")
	}
}

func TestFileAPIRefusesPathsOutsideEveryRoot(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/etc/passwd", "/", h.ws + "/../../etc/passwd", "relative/path", ""} {
		resp, err := http.Get(h.http.URL + "/v1/files?path=" + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("path %q returned %d %s; expected refusal", p, resp.StatusCode, body)
		}
	}
}

func TestFileAPIRefusesSymlinkEscape(t *testing.T) {
	h := newHarness(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h.ws, "link")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(h.http.URL + "/v1/files?path=" + h.ws + "/link")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatalf("symlink escape succeeded and returned %q", body)
	}
	if bytes.Contains(body, []byte("SECRET")) {
		t.Fatal("response leaked the contents of a file outside the sandbox")
	}
}

func TestFileAPIHonoursReadOnlyRoots(t *testing.T) {
	h := newHarness(t)
	// Reading the read-only root works.
	resp, err := http.Get(h.http.URL + "/v1/files?path=" + h.ro + "/readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("read of a read-only root: %d", resp.StatusCode)
	}
	// Writing to it does not.
	req, _ := http.NewRequest(http.MethodPut, h.http.URL+"/v1/files?path="+h.ro+"/new.txt", strings.NewReader("x"))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write to a read-only root returned %d, want 403", resp.StatusCode)
	}
	// And neither does deleting from it.
	req, _ = http.NewRequest(http.MethodDelete, h.http.URL+"/v1/files?path="+h.ro+"/readme.txt", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete from a read-only root returned %d, want 403", resp.StatusCode)
	}
}

func TestExecStreamOverWebSocket(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(h.http.URL, "http") +
		"/v1/exec/stream?cmd=" + "for%20i%20in%201%202%203%3B%20do%20echo%20line%24i%3B%20done%3B%20exit%205"
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	var out strings.Builder
	var exit *api.Frame
	var lastSeq uint64
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		var f api.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("bad frame %q: %v", data, err)
		}
		switch f.T {
		case api.FrameStdout:
			if f.Seq <= lastSeq {
				t.Fatalf("seq must increase monotonically: got %d after %d", f.Seq, lastSeq)
			}
			lastSeq = f.Seq
			raw, err := base64.StdEncoding.DecodeString(f.D)
			if err != nil {
				t.Fatalf("payload is not base64: %v", err)
			}
			out.Write(raw)
		case api.FrameExit:
			exit = &f
		}
		if exit != nil {
			break
		}
	}
	if exit == nil {
		t.Fatal("stream ended without an exit frame")
	}
	if exit.Code != 5 {
		t.Fatalf("exit code %d want 5", exit.Code)
	}
	for _, want := range []string{"line1", "line2", "line3"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q; got %q", want, out.String())
		}
	}
}

func TestExecStreamAcceptsStdin(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(h.http.URL, "http") + "/v1/exec/stream?cmd=cat"
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	send := func(f api.Frame) {
		b, _ := json.Marshal(f)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	send(api.Frame{T: api.FrameStdin, D: base64.StdEncoding.EncodeToString([]byte("ping\n"))})

	deadline := time.Now().Add(15 * time.Second)
	var got strings.Builder
	for time.Now().Before(deadline) {
		_, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		var f api.Frame
		json.Unmarshal(data, &f)
		if f.T == api.FrameStdout {
			raw, _ := base64.StdEncoding.DecodeString(f.D)
			got.Write(raw)
			if strings.Contains(got.String(), "ping") {
				return // cat echoed our stdin back
			}
		}
	}
	t.Fatalf("stdin was not delivered; read %q", got.String())
}

func TestUnixSocketListenerIsPrivate(t *testing.T) {
	// t.TempDir() produces a path long enough to exceed sun_path on macOS, and
	// this test is about the socket, not about that limit.
	dir, err := os.MkdirTemp("", "jdix")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode is %o; anything but 0600 lets other processes in", perm)
	}
	// A second Listen must succeed by clearing the stale socket, or a crashed
	// predecessor would wedge the sandbox permanently.
	ln.Close()
	ln2, err := Listen(sock)
	if err != nil {
		t.Fatalf("relisten over a stale socket: %v", err)
	}
	ln2.Close()

	var _ net.Listener = ln
}

func TestListenRejectsOverlongSocketPath(t *testing.T) {
	long := "/tmp/" + strings.Repeat("x", 120) + ".sock"
	if _, err := Listen(long); err == nil {
		t.Fatal("expected a rejection")
	} else if !strings.Contains(err.Error(), "kernel limit") {
		t.Fatalf("error should explain the length limit, got: %v", err)
	}
}
