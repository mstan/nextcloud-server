package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func getBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func getJSON(ctx context.Context, url string, v any) error {
	b, err := getBytes(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dst + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// unzip extracts an ncwin release zip (whose entries are the install_dir
// contents, as produced by the release workflow) into dst.
func unzip(src, dst string) error {
	z, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer z.Close()
	root, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	for _, f := range z.File {
		target := filepath.Join(root, filepath.FromSlash(f.Name))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("zip entry escapes destination: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := extractOne(f, target); err != nil {
			return err
		}
	}
	return nil
}

func extractOne(f *zip.File, target string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		return copyFile(p, t)
	})
}

// rename retries os.Rename for up to ~30s: on Windows a directory cannot be
// renamed while any file inside it is open, and antivirus scanners briefly
// open freshly extracted files. Observed in tests as an immediate failure.
func rename(oldPath, newPath string) error {
	var err error
	for delay := 50 * time.Millisecond; ; delay *= 2 {
		if err = os.Rename(oldPath, newPath); err == nil {
			return nil
		}
		if delay > 8*time.Second {
			return err
		}
		time.Sleep(delay)
	}
}
