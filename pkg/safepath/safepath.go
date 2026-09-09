// Package safepath opens paths that are guaranteed to stay inside a root
// directory, even if an attacker controls the contents of that directory.
//
// Lexical checks (path.Clean, rejecting "..") are necessary but not sufficient:
// a symlink placed inside the root can point anywhere, and a check-then-open
// sequence is a TOCTOU race. Both the file API and the bwrap source resolver
// go through here (DESIGN.md §03.4, §07).
package safepath

import (
	"errors"
	"os"
	"path"
	"strings"
)

var (
	// ErrEscape means the path resolved outside the root, or tried to.
	ErrEscape = errors.New("safepath: path escapes root")
	// ErrBadPath means the relative path was malformed before we even looked
	// at the filesystem.
	ErrBadPath = errors.New("safepath: malformed relative path")
)

// checkRel rejects anything that is not a clean, relative, traversal-free path.
// This is the cheap first gate; the filesystem walk is what actually enforces
// containment.
func checkRel(rel string) (string, error) {
	if rel == "" || rel == "." {
		return ".", nil
	}
	if strings.HasPrefix(rel, "/") {
		return "", ErrBadPath
	}
	if strings.ContainsRune(rel, 0) {
		return "", ErrBadPath
	}
	if path.Clean(rel) != rel {
		return "", ErrBadPath
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "" {
			return "", ErrBadPath
		}
	}
	return rel, nil
}

// OpenBeneath opens root/rel, guaranteeing the result is inside root.
//
// On Linux it uses openat2(RESOLVE_BENEATH|RESOLVE_NO_MAGICLINKS), which makes
// the guarantee in one atomic call. Where openat2 is unavailable it falls back
// to walking the path one segment at a time with O_NOFOLLOW, which is slower
// and not atomic against a concurrent rename, but still refuses symlinks.
func OpenBeneath(root, rel string, flags int, perm os.FileMode) (*os.File, error) {
	cleanRel, err := checkRel(rel)
	if err != nil {
		return nil, err
	}
	return openBeneath(root, cleanRel, flags, perm)
}

// ResolveBeneath verifies that root/rel exists inside root and returns the
// joined path. Callers that need to hand a path to another process (bwrap, for
// instance) use this: the open proves containment, the string is what gets
// passed on.
//
// The returned path is only as trustworthy as the directory is stable — if
// something can rename entries under root between this call and the use of the
// path, prefer OpenBeneath and pass the file descriptor.
func ResolveBeneath(root, rel string) (string, error) {
	f, err := OpenBeneath(root, rel, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if rel == "" || rel == "." {
		return root, nil
	}
	return path.Join(root, rel), nil
}

// IsEscape reports whether err indicates a containment failure rather than an
// ordinary I/O error, so callers can return 403 instead of 404.
func IsEscape(err error) bool {
	return errors.Is(err, ErrEscape) || errors.Is(err, ErrBadPath)
}
