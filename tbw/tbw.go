// Copyright The Go AUTHORS, Tailscale Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The tbw command is the Tailscale Build worker daemon. It is an HTTP server
// that untars content to disk and runs commands it has untarred, streaming
// their output back over HTTP.
//
// It steals some design & code from Go's build system (x/build/cmd/buildlet),
// originally written by the same author as this code.
//
// This program intentionally allows remote code execution, and
// provides no security of its own. It is assumed that any user uses
// it with an appropriately-configured firewall between their VM
// instances.
package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	workDir = "/home/workdir"      // and HOME
	codeDir = "/home/workdir/code" // where the repo gets pushed
)

func main() {
	dur, _ := time.ParseDuration(os.Getenv("VM_MAX_DURATION"))
	if dur == 0 {
		dur = 1 * time.Hour
	}

	time.AfterFunc(dur, shutdownOfLastResort)
	if os.Getenv("EXIT_ON_START") == "1" {
		fmt.Println("tb shutting down on start per EXIT_ON_START")
		return
	}
	fmt.Println("tbw running.")

	if v := os.Getenv("REGISTER_ALIVE_URL"); v != "" {
		go func() {
			res, err := http.Get(v)
			if err != nil {
				log.Printf("Alive URL %v error: %v", v, err)
				return
			}
			defer res.Body.Close()
			log.Printf("Alive URL %v says: %v", v, res.Status)
		}()
	}

	if err := os.MkdirAll(workDir, 0755); err != nil {
		log.Fatal(err)
	}

	if err := unix.Mount("/dev/tmpfs", workDir, "tmpfs", 0, ""); err != nil {
		log.Fatal(err)
	}
	if false {
		mounts, _ := os.ReadFile("/proc/mounts")
		log.Printf("Mounts: %s", mounts)
		modules, _ := os.ReadFile("/proc/modules")
		log.Printf("Modules: %s", modules)
		filesystems, _ := os.ReadFile("/proc/filesystems")
		log.Printf("Filesystems: %s", filesystems)
	}

	if err := os.MkdirAll(codeDir, 0755); err != nil {
		log.Fatal(err)
	}

	m := http.NewServeMux()
	m.HandleFunc("/", handle)
	m.HandleFunc("/put", handlePutTarball)
	m.HandleFunc("/status", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "OK\n")
	}))
	m.HandleFunc("/gen204", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	m.HandleFunc("/quitquitquit", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		os.Exit(0)
	}))
	m.HandleFunc("/test/", test)
	m.HandleFunc("/check/go-version", toolGoVersion)
	m.HandleFunc("/env", env)
	log.Fatal(http.ListenAndServe(":8080", m))
}

func shutdownOfLastResort() {
	log.Fatalf("shutdown of last resort")
}

func handle(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "hi from fly machines.\n")
}

type webWriter struct {
	typ int // 1=stdout, 2=stderr, -1, exit status
	mu  *sync.Mutex
	je  *json.Encoder
	f   http.Flusher
}

func (w webWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	err = w.je.Encode([]any{w.typ, string(p)})
	if err != nil {
		return 0, err
	}
	w.f.Flush()
	return len(p), nil
}

func test(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	cacheServer := query.Get("cache-server")
	cacheServerVerbose, _ := strconv.ParseBool(query.Get("cache-server-verbose"))
	pkg := query.Get("pkg")
	log.Printf("testing %v ...", pkg)
	t0 := time.Now()
	cmd := exec.Command(filepath.Join(codeDir, "tool/go"), "test", "-json", "-v")

	var mu sync.Mutex
	je := json.NewEncoder(w)
	f := w.(http.Flusher)

	cmd.Args = append(cmd.Args, pkg)
	cmd.Dir = codeDir
	cmd.Stdout = &webWriter{1, &mu, je, f}
	cmd.Stderr = &webWriter{2, &mu, je, f}
	cmd.Env = append(os.Environ(),
		"HOME="+workDir,
		fmt.Sprintf("GOCACHEPROG=/usr/local/bin/go-cacher --verbose=%v --cache-server=%s", cacheServerVerbose, cacheServer),
	)

	for _, v := range r.Header["Test-Env"] {
		cmd.Env = append(cmd.Env, v)
	}
	err := cmd.Run()
	d := time.Since(t0).Round(time.Millisecond)
	log.Printf("test of %v = %v in %v", pkg, err, d)
}

func toolGoVersion(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	cacheServer := query.Get("cache-server")
	cacheServerVerbose, _ := strconv.ParseBool(query.Get("cache-server-verbose"))
	cmd := exec.Command(filepath.Join(codeDir, "tool/go"), "version")
	cmd.Dir = codeDir
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Env = append(os.Environ(),
		"HOME="+workDir,
		fmt.Sprintf("GOCACHEPROG=/usr/local/bin/go-cacher --verbose=%v --cache-server=%s", cacheServerVerbose, cacheServer),
	)
	cmd.Run()
}

func env(w http.ResponseWriter, r *http.Request) {
	j, _ := json.MarshalIndent(os.Environ(), "", "  ")
	w.Write(j)
}

func handlePutTarball(w http.ResponseWriter, r *http.Request) {
	urlParam, _ := url.ParseQuery(r.URL.RawQuery)
	baseDir := workDir
	if dir := urlParam.Get("dir"); dir != "" {
		var err error
		dir, err = nativeRelPath(dir)
		if err != nil {
			log.Printf("writetgz: bogus dir %q", dir)
			http.Error(w, "invalid 'dir' parameter: "+err.Error(), http.StatusBadRequest)
			return
		}
		baseDir = filepath.Join(baseDir, dir)

		if err := os.MkdirAll(baseDir, 0755); err != nil {
			log.Printf("writetgz: %v", err)
			http.Error(w, "mkdir of base: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	var tgz io.Reader
	var urlStr string
	switch r.Method {
	case "PUT":
		tgz = r.Body
		log.Printf("writetgz: untarring Request.Body into %s", baseDir)
	case "POST":
		urlStr = r.FormValue("url")
		if urlStr == "" {
			log.Printf("writetgz: missing url POST param")
			http.Error(w, "missing url POST param", http.StatusBadRequest)
			return
		}
		t0 := time.Now()
		res, err := http.Get(urlStr)
		if err != nil {
			log.Printf("writetgz: failed to fetch tgz URL %s: %v", urlStr, err)
			http.Error(w, fmt.Sprintf("fetching URL %s: %v", urlStr, err), http.StatusInternalServerError)
			return
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			log.Printf("writetgz: failed to fetch tgz URL %s: status=%v", urlStr, res.Status)
			http.Error(w, fmt.Sprintf("writetgz: fetching provided URL %q: %s", urlStr, res.Status), http.StatusInternalServerError)
			return
		}
		tgz = res.Body
		log.Printf("writetgz: untarring %s (got headers in %v) into %s", urlStr, time.Since(t0), baseDir)
	default:
		log.Printf("writetgz: invalid method %q", r.Method)
		http.Error(w, "requires PUT or POST method", http.StatusBadRequest)
		return
	}

	err := untar(tgz, baseDir)
	if err != nil {
		http.Error(w, err.Error(), httpStatus(err))
		return
	}

	if hash, ok := strings.CutPrefix(urlParam.Get("dir"), "tailscale-go/"); ok {
		os.MkdirAll(filepath.Join(workDir, ".cache"), 0755)
		if err := os.Rename(
			filepath.Join(workDir, "tailscale-go", hash, "go"),
			filepath.Join(workDir, ".cache", "tailscale-go")); err != nil {
			http.Error(w, err.Error(), httpStatus(err))
			return
		}
		if err := os.WriteFile(
			filepath.Join(workDir, ".cache", "tailscale-go.extracted"),
			[]byte(hash+"\n"), 0644); err != nil {
			http.Error(w, err.Error(), httpStatus(err))
			return
		}
	}

	io.WriteString(w, "OK")
}

// untar reads the gzip-compressed tar file from r and writes it into dir.
func untar(r io.Reader, dir string) (err error) {
	t0 := time.Now()
	nFiles := 0
	madeDir := map[string]bool{}
	defer func() {
		td := time.Since(t0)
		if err == nil {
			log.Printf("extracted tarball into %s: %d files, %d dirs (%v)", dir, nFiles, len(madeDir), td)
		} else {
			log.Printf("error extracting tarball into %s after %d files, %d dirs, %v: %v", dir, nFiles, len(madeDir), td, err)
		}
	}()
	zr, err := gzip.NewReader(r)
	if err != nil {
		return badRequestf("requires gzip-compressed body: %w", err)
	}
	tr := tar.NewReader(zr)
	loggedChtimesError := false
	for {
		f, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("tar reading error: %v", err)
			return badRequestf("tar error: %w", err)
		}
		if f.Typeflag == tar.TypeXGlobalHeader {
			// golang.org/issue/22748: git archive exports
			// a global header ('g') which after Go 1.9
			// (for a bit?) contained an empty filename.
			// Ignore it.
			continue
		}
		rel, err := nativeRelPath(f.Name)
		if err != nil {
			return badRequestf("tar file contained invalid name %q: %v", f.Name, err)
		}
		abs := filepath.Join(dir, rel)

		fi := f.FileInfo()
		mode := fi.Mode()
		switch {
		case mode.IsRegular():
			// Make the directory. This is redundant because it should
			// already be made by a directory entry in the tar
			// beforehand. Thus, don't check for errors; the next
			// write will fail with the same error.
			dir := filepath.Dir(abs)
			if !madeDir[dir] {
				if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
					return err
				}
				madeDir[dir] = true
			}
			wf, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode.Perm())
			if err != nil {
				return err
			}
			n, err := io.Copy(wf, tr)
			if closeErr := wf.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
			if err != nil {
				return fmt.Errorf("error writing to %s: %v", abs, err)
			}
			if n != f.Size {
				return fmt.Errorf("only wrote %d bytes to %s; expected %d", n, abs, f.Size)
			}
			modTime := f.ModTime
			if modTime.After(t0) {
				// Clamp modtimes at system time. See
				// golang.org/issue/19062 when clock on
				// buildlet was behind the gitmirror server
				// doing the git-archive.
				modTime = t0
			}
			if !modTime.IsZero() {
				if err := os.Chtimes(abs, modTime, modTime); err != nil && !loggedChtimesError {
					// benign error. Gerrit doesn't even set the
					// modtime in these, and we don't end up relying
					// on it anywhere (the gomote push command relies
					// on digests only), so this is a little pointless
					// for now.
					log.Printf("error changing modtime: %v (further Chtimes errors suppressed)", err)
					loggedChtimesError = true // once is enough
				}
			}
			nFiles++
		case mode.IsDir():
			if err := os.MkdirAll(abs, 0755); err != nil {
				return err
			}
			madeDir[abs] = true
		case mode&os.ModeSymlink != 0:
			// TODO: ignore these for now. They were breaking x/build tests.
			// Implement these if/when we ever have a test that needs them.
			// But maybe we'd have to skip creating them on Windows for some builders
			// without permissions.
		default:
			return badRequestf("tar file entry %s contained unsupported file type %v", f.Name, mode)
		}
	}
	return nil
}

// nativeRelPath verifies that p is a non-empty relative path
// using either slashes or the buildlet's native path separator,
// and returns it canonicalized to the native path separator.
func nativeRelPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path not provided")
	}

	if filepath.Separator != '/' && strings.Contains(p, string(filepath.Separator)) {
		clean := filepath.Clean(p)
		if filepath.IsAbs(clean) {
			return "", fmt.Errorf("path %q is not relative", p)
		}
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q refers to a parent directory", p)
		}
		if strings.HasPrefix(p, string(filepath.Separator)) || filepath.VolumeName(clean) != "" {
			// On Windows, this catches semi-relative paths like "C:" (meaning “the
			// current working directory on volume C:”) and "\windows" (meaning “the
			// windows subdirectory of the current drive letter”).
			return "", fmt.Errorf("path %q is relative to volume", p)
		}
		return p, nil
	}

	clean := path.Clean(p)
	if path.IsAbs(clean) {
		return "", fmt.Errorf("path %q is not relative", p)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q refers to a parent directory", p)
	}
	canon := filepath.FromSlash(p)
	if filepath.VolumeName(canon) != "" {
		return "", fmt.Errorf("path %q begins with a native volume name", p)
	}
	return canon, nil
}

// An httpError wraps an error with a corresponding HTTP status code.
type httpError struct {
	statusCode int
	err        error
}

func (he httpError) Error() string   { return he.err.Error() }
func (he httpError) Unwrap() error   { return he.err }
func (he httpError) httpStatus() int { return he.statusCode }

// badRequestf returns an httpError with status 400 and an error constructed by
// formatting the given arguments.
func badRequestf(format string, args ...interface{}) error {
	return httpError{http.StatusBadRequest, fmt.Errorf(format, args...)}
}

// httpStatus returns the httpStatus of err if it is or wraps an httpError,
// or StatusInternalServerError otherwise.
func httpStatus(err error) int {
	var he httpError
	if !errors.As(err, &he) {
		return http.StatusInternalServerError
	}
	return he.statusCode
}
