package jdix

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Files is the sandbox file API.
//
// Everything here is io.Reader and io.Writer rather than []byte. Sandboxes
// produce model checkpoints and build artefacts; an API that insisted on
// materialising those in memory would be unusable exactly when it mattered.
type Files struct{ sbx *Sandbox }

// FileInfo describes one directory entry.
type FileInfo struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Mode     string    `json:"mode"`
	IsDir    bool      `json:"isDir"`
	Modified time.Time `json:"modified"`
	// Symlink is reported so a listing is honest, but reading through one is
	// refused: a link inside a volume can point anywhere.
	Symlink bool `json:"symlink"`
}

// Open returns a reader for a file. The caller closes it.
func (f *Files) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	u, err := f.sbx.endpointURL("/v1/files?path=" + url.QueryEscape(path))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.sbx.token)

	resp, err := f.sbx.client.http.Do(req)
	if err != nil {
		return nil, err
	}
	if apiErr := decodeError(resp); apiErr != nil {
		resp.Body.Close()
		return nil, apiErr
	}
	return resp.Body, nil
}

// ReadAll is the convenience form, for files you know are small.
func (f *Files) ReadAll(ctx context.Context, path string) ([]byte, error) {
	r, err := f.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Write streams content to a path, creating parent directories as needed.
func (f *Files) Write(ctx context.Context, path string, content io.Reader) error {
	u, err := f.sbx.endpointURL("/v1/files?path=" + url.QueryEscape(path))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, content)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.sbx.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := f.sbx.client.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if apiErr := decodeError(resp); apiErr != nil {
		return apiErr
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// WriteString is the convenience form.
func (f *Files) WriteString(ctx context.Context, path, content string) error {
	return f.Write(ctx, path, strings.NewReader(content))
}

// List returns the entries of a directory.
func (f *Files) List(ctx context.Context, path string) ([]FileInfo, error) {
	var out struct {
		Entries []FileInfo `json:"entries"`
	}
	err := f.sbx.dataPlane(ctx, http.MethodGet,
		"/v1/files/list?path="+url.QueryEscape(path), nil, &out)
	return out.Entries, err
}

// Remove deletes a file, or a directory tree when recursive is set.
func (f *Files) Remove(ctx context.Context, path string, recursive bool) error {
	q := "/v1/files?path=" + url.QueryEscape(path)
	if recursive {
		q += "&recursive=true"
	}
	return f.sbx.dataPlane(ctx, http.MethodDelete, q, nil, nil)
}

// Process is one process the sandbox started through the API.
type Process struct {
	PID     int    `json:"pid"`
	Cmd     string `json:"cmd"`
	Started string `json:"started"`
	Running bool   `json:"running"`
}

// Processes lists what this sandbox started.
//
// It reports only processes launched through the API, not everything running
// inside. A partial list presented as complete would be worse than a short one.
func (s *Sandbox) Processes(ctx context.Context) ([]Process, error) {
	var out struct {
		Processes []Process `json:"processes"`
	}
	err := s.dataPlane(ctx, http.MethodGet, "/v1/processes", nil, &out)
	return out.Processes, err
}

// Signal delivers a signal to a process group started by this sandbox.
func (s *Sandbox) Signal(ctx context.Context, pid int, sig string) error {
	return s.dataPlane(ctx, http.MethodDelete,
		"/v1/processes/"+itoa(pid)+"?signal="+url.QueryEscape(sig), nil, nil)
}

// Expose makes a port inside the sandbox reachable and returns its URL.
func (s *Sandbox) Expose(ctx context.Context, port int) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	body := map[string]any{"port": port, "public": true}
	if err := s.dataPlane(ctx, http.MethodPost, "/v1/ports", body, &out); err != nil {
		return "", err
	}
	if out.URL != "" {
		return out.URL, nil
	}
	// The gateway derives the hostname from the sandbox id and port, so it can
	// be built locally when the server does not echo it back.
	host := strings.TrimPrefix(strings.TrimPrefix(s.Endpoint, "https://"), "http://")
	name, rest, ok := strings.Cut(host, ".")
	if !ok {
		return "", &Error{Code: "no_endpoint", Message: "this sandbox has no published endpoint"}
	}
	return "https://" + name + "-" + itoa(port) + "." + rest, nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
