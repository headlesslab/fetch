package fetch

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// unzip extracts the archive at archivePath into root, which it creates.
// Entries that would land outside root fail the extraction.
func unzip(ctx context.Context, archivePath, root string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("%s: %w", archivePath, err)
	}
	defer func() { _ = zr.Close() }()

	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := extract(f, root); err != nil {
			return fmt.Errorf("entry %q: %w", f.Name, err)
		}
	}

	return nil
}

// extract writes one archive entry under root.
func extract(f *zip.File, root string) error {
	name, err := localPath(f.Name)
	if err != nil {
		return err
	}

	target := filepath.Join(root, name)
	mode := f.Mode()

	switch {
	case mode.IsDir():
		return os.MkdirAll(target, 0o755)
	case mode&fs.ModeSymlink != 0:
		return extractSymlink(f, name, target)
	default:
		return extractFile(f, target, mode.Perm())
	}
}

func extractFile(f *zip.File, target string, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
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

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()

		return err
	}

	return dst.Close()
}

// extractSymlink recreates a symbolic-link entry, whose content is the link
// target. The target must stay under root once resolved against the link's
// own directory.
func extractSymlink(f *zip.File, name, target string) error {
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

	if _, err := localPath(path.Join(path.Dir(filepath.ToSlash(name)), dest)); err != nil {
		return fmt.Errorf("symbolic link to %q escapes the archive", dest)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	return os.Symlink(filepath.FromSlash(dest), target)
}

// localPath turns an archive entry name into a relative, cleaned path that
// stays inside the extraction root, or fails. Backslashes count as
// separators, as archives written on Windows may use them.
func localPath(name string) (string, error) {
	slashed := strings.ReplaceAll(name, "\\", "/")
	cleaned := path.Clean(slashed)
	local := filepath.FromSlash(cleaned)

	if path.IsAbs(slashed) || cleaned == "." || !filepath.IsLocal(local) {
		return "", fmt.Errorf("path %q escapes the archive", name)
	}

	return local, nil
}
