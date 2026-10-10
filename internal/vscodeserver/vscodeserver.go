// Package vscodeserver installs and starts VS Code for the Web (the VS Code
// server's "-web" build, which also serves the workbench UI) inside a pod.
//
// The server tarball is downloaded on the client (and cached) and uploaded
// over SSH, so pods need no internet access, only tar.
package vscodeserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultDownloadURL is Microsoft's per-commit web server build.
	DefaultDownloadURL = "https://update.code.visualstudio.com/commit:${commit}/server-${platform}-web/${quality}"
	latestCommitURL    = "https://update.code.visualstudio.com/api/latest/server-${platform}-web/${quality}"
	dataFolder         = ".vscode-server"
)

// Runner runs a POSIX sh script in the pod with positional args, feeding
// stdin if given, and returns its stdout. podssh.Client implements it.
type Runner interface {
	Run(script string, stdin io.Reader, args ...string) (string, error)
}

type Options struct {
	// Commit to run; empty means the latest one for Quality.
	Commit string
	// Quality is "stable" or "insider".
	Quality string
	// DownloadURL is a template with ${commit} ${quality} ${platform} ${os}
	// ${arch}; empty means DefaultDownloadURL.
	DownloadURL string
	// CacheDir holds downloaded tarballs; empty means the user cache dir.
	CacheDir string
	// Host and Port the server listens on inside the pod; defaults are
	// 127.0.0.1 and any free port.
	Host string
	Port int
	// BasePath serves the workbench under a path prefix, e.g. behind the
	// relay's /vscode/<namespace>/<pod>/.
	BasePath string
	// Step, if set, is called with a short description of each step.
	Step func(string)
	// Progress, if set, receives download progress lines.
	Progress io.Writer
}

// Server is a running VS Code server inside the pod.
type Server struct {
	// Port the server listens on inside the pod.
	Port int
	// Token is the connection token (?tkn=).
	Token string
	// Home is the login user's home directory in the pod.
	Home     string
	Commit   string
	Platform string
}

type platform struct {
	os, arch string // VSCodium naming: linux|alpine, x64|arm64|armhf
	id       string // Microsoft naming: linux-x64, linux-alpine, alpine-arm64, ...
}

// Ensure installs the server if needed and starts it, or reuses one that is
// already running for the same commit.
func Ensure(r Runner, o Options) (*Server, error) {
	step := o.Step
	if step == nil {
		step = func(string) {}
	}
	if o.Quality == "" {
		o.Quality = "stable"
	}
	if o.DownloadURL == "" {
		o.DownloadURL = DefaultDownloadURL
	}

	step("detecting pod platform")
	out, err := r.Run(detectScript, nil)
	if err != nil {
		return nil, err
	}
	f := strings.Fields(out) // uname -s, uname -m, libc, $HOME
	if len(f) < 4 || f[0] != "Linux" {
		return nil, fmt.Errorf("unsupported pod: %q", strings.TrimSpace(out))
	}
	p, err := toPlatform(f[1], f[2])
	if err != nil {
		return nil, err
	}
	commit := o.Commit
	if commit == "" {
		if commit, err = latestCommit(p, o.Quality); err != nil {
			return nil, err
		}
	}
	key := commit + "-web"
	bin := serverBinary(o.DownloadURL)
	step(fmt.Sprintf("pod is %s; VS Code server %s", p.id, commit[:min(12, len(commit))]))

	installed, err := r.Run(installedScript, nil, dataFolder, key, bin)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(installed) == "yes" {
		step("server already installed in the pod")
	} else {
		url := expand(o.DownloadURL, map[string]string{
			"commit": commit, "quality": o.Quality, "platform": p.id, "os": p.os, "arch": p.arch,
		})
		tarball, err := download(url, o.CacheDir, fmt.Sprintf("%s-%s-web.tar.gz", commit, p.id), step, o.Progress)
		if err != nil {
			return nil, err
		}
		step("uploading server to the pod")
		fh, err := os.Open(tarball)
		if err != nil {
			return nil, err
		}
		defer fh.Close()
		if _, err := r.Run(uploadScript, fh, dataFolder, key); err != nil {
			return nil, err
		}
	}

	step("starting server")
	host := o.Host
	if host == "" {
		host = "127.0.0.1"
	}
	out, err = r.Run(startScript, nil, dataFolder, key, bin, host, strconv.Itoa(o.Port), o.BasePath)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(regexp.MustCompile(`(?m)^port=(\d+)$`).FindStringSubmatch(out + "\nport=0")[1])
	token := regexp.MustCompile(`(?m)^token=(\S+)$`).FindStringSubmatch(out)
	if port == 0 || token == nil {
		return nil, fmt.Errorf("could not parse server start output: %q", out)
	}
	return &Server{Port: port, Token: token[1], Home: f[3], Commit: commit, Platform: p.id}, nil
}

// serverBinary is the server launcher inside the tarball's bin/ directory.
func serverBinary(urlTemplate string) string {
	if strings.Contains(urlTemplate, "vscodium") {
		return "codium-server"
	}
	return "code-server"
}

func toPlatform(machine, libc string) (platform, error) {
	arch := map[string]string{"x86_64": "x64", "amd64": "x64", "aarch64": "arm64", "arm64": "arm64", "armv7l": "armhf", "armv8l": "armhf"}[machine]
	switch {
	case arch == "":
		return platform{}, fmt.Errorf("unsupported pod architecture %s", machine)
	case libc != "musl":
		return platform{"linux", arch, "linux-" + arch}, nil
	case arch == "x64":
		return platform{"alpine", arch, "linux-alpine"}, nil
	case arch == "arm64":
		return platform{"alpine", arch, "alpine-arm64"}, nil
	}
	return platform{}, fmt.Errorf("no VS Code server build for musl on %s", arch)
}

func latestCommit(p platform, quality string) (string, error) {
	resp, err := http.Get(expand(latestCommitURL, map[string]string{"platform": p.id, "quality": quality}))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.Version == "" {
		return "", fmt.Errorf("latest VS Code server for %s: %s %v", p.id, resp.Status, err)
	}
	return v.Version, nil
}

// download fetches url into cacheDir/name, reusing a previous download.
// Interrupted downloads are kept as name.part and resumed with HTTP Range
// requests, both across retries here and across runs.
func download(url, cacheDir, name string, step func(string), progressOut io.Writer) (string, error) {
	if cacheDir == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		cacheDir = filepath.Join(dir, "podrelay")
	}
	dest := filepath.Join(cacheDir, name)
	if _, err := os.Stat(dest); err == nil {
		step("using cached " + dest)
		return dest, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	part := dest + ".part"
	step("downloading " + url)
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if err = fetch(url, part, progressOut); err == nil {
			return dest, os.Rename(part, dest)
		}
		if attempt < downloadAttempts {
			step(fmt.Sprintf("download interrupted (%v); resuming", err))
			time.Sleep(time.Duration(attempt) * retryDelay)
		}
	}
	return "", fmt.Errorf("download %s: %w", url, err)
}

var (
	downloadAttempts = 5
	retryDelay       = time.Second
	// stallTimeout aborts (and then resumes) a download that stops sending data.
	stallTimeout = time.Minute
)

// fetch appends the rest of url to part, starting where part ends.
func fetch(url, part string, progressOut io.Writer) error {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent && off > 0:
		// resuming
	case resp.StatusCode == http.StatusOK:
		// fresh download, or the server ignored Range: start over
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		off = 0
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && off > 0:
		return nil // part is already complete
	default:
		return fmt.Errorf("%s", resp.Status)
	}

	total := int64(-1)
	if resp.ContentLength >= 0 {
		total = off + resp.ContentLength
	}
	var pr *progress
	sink := io.Writer(stallReset{stall})
	if progressOut != nil {
		pr = &progress{out: progressOut, total: total, n: off}
		sink = io.MultiWriter(sink, pr)
	}
	n, err := io.Copy(f, io.TeeReader(resp.Body, sink))
	if pr != nil {
		pr.done()
	}
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("no data for %s", stallTimeout)
		}
		return err
	}
	if total >= 0 && off+n != total {
		return fmt.Errorf("got %d of %d bytes", off+n, total)
	}
	return nil
}

// stallReset restarts the stall timer whenever data arrives.
type stallReset struct{ t *time.Timer }

func (s stallReset) Write(b []byte) (int, error) {
	s.t.Reset(stallTimeout)
	return len(b), nil
}

func expand(tmpl string, vars map[string]string) string {
	return regexp.MustCompile(`\$\{(\w+)\}`).ReplaceAllStringFunc(tmpl, func(m string) string {
		if v, ok := vars[m[2:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// progress prints download progress at most twice a second.
type progress struct {
	out      io.Writer
	total, n int64
	last     time.Time
}

func (p *progress) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()
		p.print()
	}
	return len(b), nil
}

func (p *progress) print() {
	if p.total > 0 {
		fmt.Fprintf(p.out, "\r    %.1f / %.1f MB (%d%%)", float64(p.n)/1e6, float64(p.total)/1e6, p.n*100/p.total)
	} else {
		fmt.Fprintf(p.out, "\r    %.1f MB", float64(p.n)/1e6)
	}
}

func (p *progress) done() {
	p.print()
	fmt.Fprintln(p.out)
}

// The scripts run in the pod as `sh -c <script> sh <args...>`; see scripts/.
var (
	//go:embed scripts/detect.sh
	detectScript string
	//go:embed scripts/installed.sh
	installedScript string
	//go:embed scripts/upload.sh
	uploadScript string
	//go:embed scripts/start.sh
	startScript string
)
