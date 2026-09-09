package initd

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/safepath"
)

// maxUploadBytes bounds a single PUT. Larger payloads go through the archive
// endpoints, which stream.
const maxUploadBytes = 512 << 20

var errNoRoot = errors.New("path is outside every directory this sandbox may touch")

// resolveRoot maps a caller-supplied absolute path to (root, relative) using
// the longest matching configured root.
//
// The lexical work here only picks which root applies. Containment is proved by
// safepath, which walks the real filesystem — a symlink inside the root cannot
// talk its way past this function.
func resolveRoot(roots []api.Root, p string) (api.Root, string, error) {
	if p == "" || !strings.HasPrefix(p, "/") {
		return api.Root{}, "", errNoRoot
	}
	clean := path.Clean(p)
	var best api.Root
	var bestRel string
	found := false
	for _, r := range roots {
		rc := path.Clean(r.Path)
		var rel string
		switch {
		case clean == rc:
			rel = "."
		case strings.HasPrefix(clean, rc+"/"):
			rel = strings.TrimPrefix(clean, rc+"/")
		default:
			continue
		}
		if !found || len(rc) > len(path.Clean(best.Path)) {
			best, bestRel, found = r, rel, true
		}
	}
	if !found {
		return api.Root{}, "", errNoRoot
	}
	return best, bestRel, nil
}

func (s *Server) openForRead(p string) (*os.File, api.Root, error) {
	_, roots, _ := s.snapshot()
	root, rel, err := resolveRoot(roots, p)
	if err != nil {
		return nil, root, err
	}
	f, err := safepath.OpenBeneath(root.Path, rel, os.O_RDONLY, 0)
	return f, root, err
}

// fileError turns a resolution failure into the right status code. Confusing
// "outside your roots" with "not found" would either leak the existence of
// paths or hide real mistakes, so they stay distinct.
func fileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoRoot):
		writeErr(w, http.StatusForbidden, "path_forbidden", err.Error())
	case safepath.IsEscape(err):
		writeErr(w, http.StatusForbidden, "path_escape", "path is malformed or traverses a symlink out of its directory")
	case errors.Is(err, os.ErrNotExist):
		writeErr(w, http.StatusNotFound, "not_found", "no such file or directory")
	case errors.Is(err, os.ErrPermission):
		writeErr(w, http.StatusForbidden, "permission_denied", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "io_error", err.Error())
	}
}

func (s *Server) handleFileGet(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	f, _, err := s.openForRead(p)
	if err != nil {
		fileError(w, err)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		fileError(w, err)
		return
	}
	if fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "is_directory", "use /v1/files/list for directories")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(p)+"\"")
	// ServeContent gives us Range support, which matters for resuming a large
	// artefact download.
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

func (s *Server) handleFilePut(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	_, roots, _ := s.snapshot()
	root, rel, err := resolveRoot(roots, p)
	if err != nil {
		fileError(w, err)
		return
	}
	if root.ReadOnly {
		writeErr(w, http.StatusForbidden, "read_only", "this path is mounted read-only")
		return
	}
	if rel == "." {
		writeErr(w, http.StatusBadRequest, "is_directory", "cannot write to a directory")
		return
	}

	// Parent directories are created inside the root, never above it.
	if dir := path.Dir(rel); dir != "." {
		if err := mkdirAllBeneath(root.Path, dir); err != nil {
			fileError(w, err)
			return
		}
	}
	f, err := safepath.OpenBeneath(root.Path, rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		fileError(w, err)
		return
	}
	defer f.Close()

	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUploadBytes))
	if err != nil {
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path.Clean(p), "bytes": n})
}

// mkdirAllBeneath creates each missing segment through safepath, so a symlinked
// intermediate directory cannot redirect the creation outside the root.
func mkdirAllBeneath(root, rel string) error {
	cur := ""
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		if cur == "" {
			cur = seg
		} else {
			cur = cur + "/" + seg
		}
		f, err := safepath.OpenBeneath(root, cur, os.O_RDONLY, 0)
		if err == nil {
			f.Close()
			continue
		}
		if safepath.IsEscape(err) {
			return err
		}
		if err := os.Mkdir(filepath.Join(root, filepath.FromSlash(cur)), 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	_, roots, _ := s.snapshot()
	root, rel, err := resolveRoot(roots, p)
	if err != nil {
		fileError(w, err)
		return
	}
	if root.ReadOnly {
		writeErr(w, http.StatusForbidden, "read_only", "this path is mounted read-only")
		return
	}
	if rel == "." {
		writeErr(w, http.StatusForbidden, "root_delete", "refusing to delete a mount root")
		return
	}
	// Prove containment before unlinking: os.Remove would happily follow a
	// symlinked parent directory.
	f, err := safepath.OpenBeneath(root.Path, rel, os.O_RDONLY, 0)
	if err != nil {
		fileError(w, err)
		return
	}
	f.Close()

	target := filepath.Join(root.Path, filepath.FromSlash(rel))
	if r.URL.Query().Get("recursive") == "true" {
		err = os.RemoveAll(target)
	} else {
		err = os.Remove(target)
	}
	if err != nil {
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path.Clean(p)})
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		_, _, ws := s.snapshot()
		p = ws
	}
	f, _, err := s.openForRead(p)
	if err != nil {
		fileError(w, err)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		fileError(w, err)
		return
	}
	if !fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "not_a_directory", "use /v1/files to read a file")
		return
	}
	ents, err := f.ReadDir(-1)
	if err != nil {
		fileError(w, err)
		return
	}
	out := make([]api.FileInfo, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, api.FileInfo{
			Name:     e.Name(),
			Path:     path.Join(path.Clean(p), e.Name()),
			Size:     info.Size(),
			Mode:     info.Mode().Perm().String(),
			IsDir:    e.IsDir(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
			// Surfaced rather than hidden: a symlink is visible in a listing but
			// will be refused if the caller tries to read through it.
			Symlink: info.Mode()&os.ModeSymlink != 0,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": path.Clean(p), "entries": out})
}
