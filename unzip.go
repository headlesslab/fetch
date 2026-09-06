package fetch

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// maxLinkHops bounds how many symbolic links a path may pass through before
// it counts as a loop.
const maxLinkHops = 40

// unzip extracts the archive at archivePath into the existing directory root
// and reports the top-level names it wrote, each with whether it is a
// directory. Entries that would land outside root fail the extraction.
func unzip(ctx context.Context, archivePath, root string) (map[string]bool, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", archivePath, err)
	}
	defer func() { _ = zr.Close() }()

	x := &extractor{root: root, links: map[string]string{}, tops: map[string]bool{}}

	for _, f := range zr.File {
		err = ctx.Err()
		if err != nil {
			return nil, err
		}

		err = x.extract(f)
		if err != nil {
			return nil, fmt.Errorf("entry %q: %w", f.Name, err)
		}
	}

	return x.tops, nil
}

// extractor writes archive entries under one root that starts out empty, so
// the only symbolic links that can exist there are the ones it made. It
// never writes through one of them, and it checks the target of every link
// against the links made so far, so nothing under root can lead outside it.
type extractor struct {
	root  string
	links map[string]string // slash-separated entry name to its link target
	tops  map[string]bool   // top-level name to whether it is a directory
}

// extract writes one archive entry under root.
func (x *extractor) extract(f *zip.File) error {
	name, err := localPath(f.Name)
	if err != nil {
		return err
	}

	err = x.notThroughLink(name)
	if err != nil {
		return err
	}

	target := filepath.Join(x.root, filepath.FromSlash(name))
	mode := f.Mode()

	top, _, nested := strings.Cut(name, "/")
	x.tops[top] = x.tops[top] || nested || mode.IsDir()

	switch {
	case mode.IsDir():
		return os.MkdirAll(target, 0o755)
	case mode&fs.ModeSymlink != 0:
		return x.symlink(f, name, target)
	default:
		return extractFile(f, target, mode.Perm())
	}
}

// notThroughLink fails when name, or a directory on the way to it, is a
// symbolic link made earlier: writing there would follow the link.
func (x *extractor) notThroughLink(name string) error {
	for p := name; p != "."; p = path.Dir(p) {
		if _, isLink := x.links[p]; isLink {
			return fmt.Errorf("path %q passes through the symbolic link %q", name, p)
		}
	}

	return nil
}

func extractFile(f *zip.File, target string, perm fs.FileMode) error {
	err := os.MkdirAll(filepath.Dir(target), 0o755)
	if err != nil {
		return err
	}

	src, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}

	_, err = io.Copy(dst, src)
	if err != nil {
		_ = dst.Close()

		return err
	}

	return dst.Close()
}

// symlink recreates a symbolic-link entry, whose content is the link target.
// The target must be relative and stay under root once every link made so
// far is followed.
func (x *extractor) symlink(f *zip.File, name, target string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	link, err := io.ReadAll(src)
	if err != nil {
		return err
	}

	dest := strings.ReplaceAll(string(link), "\\", "/")
	if path.IsAbs(dest) || filepath.IsAbs(filepath.FromSlash(dest)) {
		return fmt.Errorf("symbolic link to absolute path %q", dest)
	}

	_, err = x.resolve(path.Dir(name) + "/" + dest)
	if err != nil {
		return fmt.Errorf("symbolic link to %q: %w", dest, err)
	}

	err = os.MkdirAll(filepath.Dir(target), 0o755)
	if err != nil {
		return err
	}

	err = os.Symlink(filepath.FromSlash(dest), target)
	if err != nil {
		return err
	}

	x.links[name] = dest

	return nil
}

// resolve follows p, a slash-separated path relative to root, one component
// at a time the way the operating system would, expanding the links made so
// far, and returns where it ends up. It fails when the path climbs out of
// root or loops.
func (x *extractor) resolve(p string) (string, error) {
	var cur []string

	rest := strings.Split(p, "/")
	hops := 0

	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]

		switch c {
		case "", ".":
		case "..":
			if len(cur) == 0 {
				return "", errors.New("escapes the archive")
			}

			cur = cur[:len(cur)-1]
		default:
			cur = append(cur, c)

			dest, isLink := x.links[strings.Join(cur, "/")]
			if !isLink {
				continue
			}

			hops++
			if hops > maxLinkHops {
				return "", errors.New("loops")
			}

			cur = cur[:len(cur)-1]
			rest = append(strings.Split(dest, "/"), rest...)
		}
	}

	return strings.Join(cur, "/"), nil
}

// localPath turns an archive entry name into a cleaned, slash-separated
// relative path that stays inside the extraction root, or fails. Backslashes
// count as separators, as archives written on Windows may use them.
func localPath(name string) (string, error) {
	slashed := strings.ReplaceAll(name, "\\", "/")
	cleaned := path.Clean(slashed)

	if path.IsAbs(slashed) || cleaned == "." || !filepath.IsLocal(filepath.FromSlash(cleaned)) {
		return "", fmt.Errorf("path %q escapes the archive", name)
	}

	return cleaned, nil
}

// stripped returns the one top-level directory under tree, for
// WithStripFirstDir, given the top-level names unzip reported.
func stripped(tree string, tops map[string]bool) (string, error) {
	if len(tops) == 1 {
		for name, isDir := range tops {
			if isDir {
				return filepath.Join(tree, name), nil
			}
		}
	}

	names := make([]string, 0, len(tops))
	for name := range tops {
		names = append(names, name)
	}

	sort.Strings(names)

	return "", fmt.Errorf("expected one top-level directory to strip, found %q", names)
}
