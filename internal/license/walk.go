package license

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Files walks in-memory files, keyed by slash-separated path, in path order.
type Files map[string][]byte

func (f Files) Walk(ctx context.Context, fn FileFunc) error {
	paths := make([]string, 0, len(f))
	for p := range f {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(p, int64(len(f[p])), bytes.NewReader(f[p])); err != nil {
			return err
		}
	}
	return nil
}

// Dir walks a local directory. Symlinks are not followed.
type Dir string

func (d Dir) Walk(ctx context.Context, fn FileFunc) error {
	root := string(d)
	return filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return fn(filepath.ToSlash(rel), info.Size(), f)
	})
}

// TarGz walks a gzipped tarball as a stream, without extracting it.
type TarGz struct {
	R io.Reader
	// StripComponents drops this many leading path elements, like tar's
	// --strip-components. GitHub tarballs wrap everything in one directory.
	StripComponents int
}

func (t TarGz) Walk(ctx context.Context, fn FileFunc) error {
	zr, err := gzip.NewReader(t.R)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		parts := strings.SplitN(name, "/", t.StripComponents+1)
		if len(parts) <= t.StripComponents {
			continue
		}
		name = parts[t.StripComponents]
		if name == "" || name == "." || strings.HasPrefix(name, "../") {
			continue
		}
		if err := fn(name, hdr.Size, tr); err != nil {
			return err
		}
	}
}
