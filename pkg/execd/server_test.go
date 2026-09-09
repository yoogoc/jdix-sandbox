package execd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/bwrap"
	"jdix.io/sandbox/pkg/isolation"
)

// initBinary is jdix-init, compiled once for the whole package.
var initBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "jxbuild")
	if err != nil {
		panic(err)
	}
	initBinary = filepath.Join(dir, "jdix-init")
	build := exec.Command("go", "build", "-o", initBinary, "jdix.io/sandbox/cmd/jdix-init")
	if out, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		panic("building jdix-init: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fixture struct {
	t       *testing.T
	srv     *Server
	control *httptest.Server
	data    *httptest.Server
	layout  bwrap.Layout
	ws      string
	expired chan string
}

// newFixture wires execd to a real jdix-init without bubblewrap in between.
//
// The launcher takes the argv execd built, drops everything up to "--" and runs
// jdix-init directly. Skipping bwrap costs the namespace, which macOS could not
// provide anyway; everything else on the bind path — argument generation,
// waiting for the socket, configuring, proxying, auth, TTL — is the real code.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	// Short base path: a unix socket under the default temp dir would exceed
	// sun_path on macOS.
	base, err := os.MkdirTemp("/tmp", "jx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	layout := bwrap.DefaultLayout()
	layout.WorkspaceDir = filepath.Join(base, "ws")
	layout.VolumeRoot = filepath.Join(base, "vol")
	layout.IPCDir = filepath.Join(base, "ipc")
	// Inside and outside coincide when there is no mount namespace, so the
	// --socket path in the generated argv is directly usable.
	layout.IPCMount = layout.IPCDir
	for _, d := range []string{layout.WorkspaceDir, layout.VolumeRoot, layout.IPCDir,
		filepath.Join(layout.VolumeRoot, "corpus")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(layout.VolumeRoot, "corpus", "readme.txt"), []byte("corpus"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, layout: layout, ws: layout.WorkspaceDir, expired: make(chan string, 1)}

	launcher := func(ctx context.Context, argv []string) (*exec.Cmd, error) {
		inner := argv
		for i, a := range argv {
			if a == "--" {
				inner = argv[i+1:]
				break
			}
		}
		if len(inner) == 0 {
			t.Fatal("generated argv had no command after --")
		}
		cmd := exec.CommandContext(ctx, initBinary, inner[1:]...)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		return cmd, cmd.Start()
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.srv = New(log, Config{
		Layout:   layout,
		Tier:     bwrap.TierUserns,
		Volumes:  map[string]string{"corpus": filepath.Join(layout.VolumeRoot, "corpus")},
		Launcher: launcher,
		UID:      1000, GID: 1000,
		InitDial:  10 * time.Second,
		GraceKill: time.Second,
	}, isolation.Result{Tier: bwrap.TierUserns, Reason: "test", Probes: map[string]string{}}, "internal-tok")
	f.srv.SetOnExpire(func(reason string) {
		select {
		case f.expired <- reason:
		default:
		}
	})

	f.control = httptest.NewServer(f.srv.ControlHandler())
	f.data = httptest.NewServer(f.srv.DataHandler())
	t.Cleanup(func() {
		f.control.Close()
		f.data.Close()
		f.srv.expire("test cleanup")
	})
	return f
}

func (f *fixture) bindReq(ttl int) api.BindRequest {
	return api.BindRequest{
		SandboxID:  "sbx_test",
		Tenant:     "tenant-abc",
		Token:      "sbt_secret_token",
		TTLSeconds: ttl,
		Env:        map[string]string{"RUN_ID": "42"},
		Secrets:    map[string]string{"TOKEN": "s3cr3t"},
		Filesystem: api.FilesystemSpec{
			Workspace: api.Workspace{Path: f.ws},
		},
	}
}

func (f *fixture) bind(req api.BindRequest) *http.Response {
	f.t.Helper()
	body, _ := json.Marshal(req)
	hr, _ := http.NewRequest(http.MethodPost, f.control.URL+"/internal/v1/bind", bytes.NewReader(body))
	hr.Header.Set("Authorization", "Bearer internal-tok")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *fixture) execAs(token, cmd string) (*http.Response, api.ExecResult) {
	f.t.Helper()
	body, _ := json.Marshal(api.ExecRequest{Cmd: cmd, TimeoutSeconds: 20})
	hr, _ := http.NewRequest(http.MethodPost, f.data.URL+"/v1/exec", bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	if token != "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		f.t.Fatal(err)
	}
	var out api.ExecResult
	if resp.StatusCode == 200 {
		json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

func TestBindThenExecThroughTheProxy(t *testing.T) {
	f := newFixture(t)

	resp := f.bind(f.bindReq(300))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("bind: %d %s", resp.StatusCode, b)
	}
	var br api.BindResponse
	json.NewDecoder(resp.Body).Decode(&br)
	if br.SandboxID != "sbx_test" || br.IsolationTier != string(bwrap.TierUserns) {
		t.Fatalf("bind response: %+v", br)
	}
	if len(br.BwrapArgs) == 0 {
		t.Fatal("bind response should echo the generated argv for auditing")
	}

	httpResp, out := f.execAs("sbt_secret_token", "echo $RUN_ID-$TOKEN")
	httpResp.Body.Close()
	if httpResp.StatusCode != 200 {
		t.Fatalf("exec through the proxy: %d", httpResp.StatusCode)
	}
	if strings.TrimSpace(out.Stdout) != "42-s3cr3t" {
		t.Fatalf("env and secrets did not reach the sandbox: %q", out.Stdout)
	}
}

func TestDataPlaneRejectsBadTokens(t *testing.T) {
	f := newFixture(t)

	// Before any bind, the data plane must not serve anyone.
	resp, _ := f.execAs("sbt_secret_token", "echo hi")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unbound Pod answered %d, want 401", resp.StatusCode)
	}

	f.bind(f.bindReq(300)).Body.Close()

	for _, tok := range []string{"", "wrong", "sbt_secret_toke", "sbt_secret_tokenn"} {
		resp, _ := f.execAs(tok, "echo hi")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q was accepted with status %d", tok, resp.StatusCode)
		}
	}
}

func TestControlPlaneRequiresItsOwnToken(t *testing.T) {
	f := newFixture(t)
	body, _ := json.Marshal(f.bindReq(300))
	resp, err := http.Post(f.control.URL+"/internal/v1/bind", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated bind returned %d, want 401", resp.StatusCode)
	}
	// The readiness probe stays open: the kubelet cannot carry a token.
	resp, err = http.Get(f.control.URL + "/internal/v1/probe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("probe returned %d; the kubelet must be able to reach it", resp.StatusCode)
	}
}

func TestPodBindsOnlyOnce(t *testing.T) {
	f := newFixture(t)
	f.bind(f.bindReq(300)).Body.Close()

	req := f.bindReq(300)
	req.SandboxID = "sbx_other"
	req.Token = "another-token"
	resp := f.bind(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second bind returned %d, want 409: a Pod serves one sandbox for its whole life", resp.StatusCode)
	}
	// The first tenant's token must still work, and the second must not.
	r1, _ := f.execAs("sbt_secret_token", "echo ok")
	r1.Body.Close()
	if r1.StatusCode != 200 {
		t.Fatalf("first tenant lost access: %d", r1.StatusCode)
	}
	r2, _ := f.execAs("another-token", "echo ok")
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rejected bind still granted access: %d", r2.StatusCode)
	}
}

func TestBindRejectsAHostileFilesystemSpec(t *testing.T) {
	f := newFixture(t)
	req := f.bindReq(300)
	req.Filesystem.Mounts = []api.Mount{{
		Path:   "/opt/jdix/bin",
		Source: api.MountSource{Volume: "corpus"},
	}}
	resp := f.bind(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bind accepted a mount over the platform directory: %d", resp.StatusCode)
	}
	// A rejected bind must leave the Pod bindable, not half-broken.
	if resp2 := f.bind(f.bindReq(300)); resp2.StatusCode != 200 {
		b, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("Pod unusable after a rejected bind: %d %s", resp2.StatusCode, b)
	}
}

func TestKeepaliveExtendsTheDeadline(t *testing.T) {
	f := newFixture(t)
	f.bind(f.bindReq(2)).Body.Close()

	f.srv.mu.RLock()
	before := f.srv.sb.expiresAt
	f.srv.mu.RUnlock()

	hr, _ := http.NewRequest(http.MethodPost, f.data.URL+"/v1/keepalive?ttlSeconds=600", nil)
	hr.Header.Set("Authorization", "Bearer sbt_secret_token")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("keepalive: %d", resp.StatusCode)
	}

	f.srv.mu.RLock()
	after := f.srv.sb.expiresAt
	f.srv.mu.RUnlock()
	if !after.After(before.Add(time.Minute)) {
		t.Fatalf("deadline did not move: %s -> %s", before, after)
	}
}

func TestTTLExpiryTearsTheSandboxDown(t *testing.T) {
	f := newFixture(t)
	f.bind(f.bindReq(1)).Body.Close()

	select {
	case reason := <-f.expired:
		if reason != "ttl" {
			t.Fatalf("expiry reason %q, want ttl", reason)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("TTL did not fire; a sandbox that outlives its deadline is the platform's worst leak")
	}

	resp, _ := f.execAs("sbt_secret_token", "echo hi")
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("the data plane still served an expired sandbox")
	}
}

func TestUnbindIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.bind(f.bindReq(300)).Body.Close()
	for i := 0; i < 3; i++ {
		hr, _ := http.NewRequest(http.MethodPost, f.control.URL+"/internal/v1/unbind", nil)
		hr.Header.Set("Authorization", "Bearer internal-tok")
		resp, err := http.DefaultClient.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("unbind %d returned %d", i, resp.StatusCode)
		}
	}
}

func TestStatusReportsTierAndBinding(t *testing.T) {
	f := newFixture(t)
	get := func() api.StatusResponse {
		hr, _ := http.NewRequest(http.MethodGet, f.control.URL+"/internal/v1/status", nil)
		hr.Header.Set("Authorization", "Bearer internal-tok")
		resp, err := http.DefaultClient.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st api.StatusResponse
		json.NewDecoder(resp.Body).Decode(&st)
		return st
	}
	if st := get(); st.Bound || st.IsolationTier != string(bwrap.TierUserns) {
		t.Fatalf("before bind: %+v", st)
	}
	f.bind(f.bindReq(300)).Body.Close()
	if st := get(); !st.Bound || st.SandboxID != "sbx_test" || st.ExpiresAt == nil {
		t.Fatalf("after bind: %+v", st)
	}
}
