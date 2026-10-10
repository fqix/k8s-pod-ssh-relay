package vscodeserver

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestToPlatform(t *testing.T) {
	tests := []struct {
		machine, libc string
		want          platform
		wantErr       bool
	}{
		{"x86_64", "glibc", platform{"linux", "x64", "linux-x64"}, false},
		{"aarch64", "glibc", platform{"linux", "arm64", "linux-arm64"}, false},
		{"armv7l", "glibc", platform{"linux", "armhf", "linux-armhf"}, false},
		{"x86_64", "musl", platform{"alpine", "x64", "linux-alpine"}, false},
		{"aarch64", "musl", platform{"alpine", "arm64", "alpine-arm64"}, false},
		{"armv7l", "musl", platform{}, true},
		{"riscv64", "glibc", platform{}, true},
	}
	for _, tt := range tests {
		got, err := toPlatform(tt.machine, tt.libc)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("toPlatform(%s, %s) = %+v, %v", tt.machine, tt.libc, got, err)
		}
	}
}

func TestExpand(t *testing.T) {
	got := expand(DefaultDownloadURL, map[string]string{"commit": "abc", "platform": "alpine-arm64", "quality": "stable"})
	want := "https://update.code.visualstudio.com/commit:abc/server-alpine-arm64-web/stable"
	if got != want {
		t.Errorf("expand = %q, want %q", got, want)
	}
	if got := expand("${unknown}/${os}", map[string]string{"os": "linux"}); got != "${unknown}/linux" {
		t.Errorf("unknown variables should be kept, got %q", got)
	}
}

// fakePod answers the scripts Ensure runs.
type fakePod struct {
	installed bool
	uploaded  []byte
	startOut  string
	calls     []string
}

func (f *fakePod) Run(script string, stdin io.Reader, args ...string) (string, error) {
	switch {
	case script == detectScript:
		f.calls = append(f.calls, "detect")
		return "Linux aarch64 musl /home/demo\n", nil
	case script == installedScript:
		f.calls = append(f.calls, "check:"+strings.Join(args, ","))
		if f.installed {
			return "yes\n", nil
		}
		return "no\n", nil
	case script == uploadScript:
		f.calls = append(f.calls, "upload")
		f.uploaded, _ = io.ReadAll(stdin)
		f.installed = true
		return "", nil
	case script == startScript:
		f.calls = append(f.calls, "start:"+strings.Join(args[3:], ","))
		return f.startOut, nil
	}
	panic("unexpected script: " + script)
}

func TestEnsureReusesInstalledServer(t *testing.T) {
	pod := &fakePod{installed: true, startOut: "port=4321\ntoken=t0k\n"}
	srv, err := Ensure(pod, Options{Commit: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	want := Server{Port: 4321, Token: "t0k", Home: "/home/demo", Commit: "abc123", Platform: "alpine-arm64"}
	if *srv != want {
		t.Errorf("Ensure = %+v, want %+v", *srv, want)
	}
	if got := strings.Join(pod.calls, " "); got != "detect check:.vscode-server,abc123-web,code-server start:127.0.0.1,0," {
		t.Errorf("calls = %s", got)
	}
}

func TestEnsureDownloadsUploadsAndCaches(t *testing.T) {
	tarball := []byte("pretend this is a tar.gz")
	var hits atomic.Int32
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/abc123/alpine-arm64/stable" {
			http.NotFound(w, r)
			return
		}
		w.Write(tarball)
	}))
	defer dl.Close()
	opts := Options{
		Commit:      "abc123",
		DownloadURL: dl.URL + "/${commit}/${platform}/${quality}",
		CacheDir:    t.TempDir(),
	}

	pod := &fakePod{startOut: "port=1\ntoken=x\n"}
	if _, err := Ensure(pod, opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pod.uploaded, tarball) {
		t.Fatalf("uploaded %q", pod.uploaded)
	}

	// A second pod gets the tarball from the cache.
	pod2 := &fakePod{startOut: "port=1\ntoken=x\n"}
	if _, err := Ensure(pod2, opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pod2.uploaded, tarball) || hits.Load() != 1 {
		t.Fatalf("second upload %q after %d downloads, want 1", pod2.uploaded, hits.Load())
	}
}

func TestEnsureBadStartOutput(t *testing.T) {
	pod := &fakePod{installed: true, startOut: "something else\n"}
	if _, err := Ensure(pod, Options{Commit: "abc"}); err == nil {
		t.Fatal("expected an error for unparseable start output")
	}
}

func TestServerBinary(t *testing.T) {
	if serverBinary(DefaultDownloadURL) != "code-server" {
		t.Error("Microsoft builds use code-server")
	}
	if serverBinary("https://github.com/VSCodium/vscodium/releases/download/x/vscodium-reh-web-${os}-${arch}-x.tar.gz") != "codium-server" {
		t.Error("VSCodium builds use codium-server")
	}
}

func TestDownloadResumesAfterDroppedConnection(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 100000) // 1 MB
	var calls atomic.Int32
	var ranges []string
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		if calls.Add(1) == 1 {
			// Promise the whole file, send part of it, then drop the connection.
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data[:len(data)/3])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer dl.Close()
	retryDelay = time.Millisecond
	defer func() { retryDelay = time.Second }()

	dir := t.TempDir()
	path, err := download(dl.URL, dir, "server.tar.gz", func(string) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, data) {
		t.Fatalf("downloaded %d bytes, want %d identical bytes", len(got), len(data))
	}
	if len(ranges) != 2 || ranges[0] != "" || ranges[1] != "bytes="+strconv.Itoa(len(data)/3)+"-" {
		t.Fatalf("requests' Range headers = %q, want a fresh request then a resume", ranges)
	}
	if _, err := os.Stat(filepath.Join(dir, "server.tar.gz.part")); !os.IsNotExist(err) {
		t.Fatal("the .part file was left behind")
	}
}

func TestDownloadResumesLeftoverPartFromEarlierRun(t *testing.T) {
	data := []byte("the whole server tarball")
	var ranges []string
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer dl.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s.tar.gz.part"), data[:9], 0o644)

	path, err := download(dl.URL, dir, "s.tar.gz", func(string) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
		t.Fatalf("got %q", got)
	}
	if len(ranges) != 1 || ranges[0] != "bytes=9-" {
		t.Fatalf("Range headers = %q, want one resume from byte 9", ranges)
	}
}

func TestEnsurePassesListenSettings(t *testing.T) {
	pod := &fakePod{installed: true, startOut: "port=13337\ntoken=t\n"}
	if _, err := Ensure(pod, Options{Commit: "c", Host: "0.0.0.0", Port: 13337, BasePath: "/vscode/ns/web"}); err != nil {
		t.Fatal(err)
	}
	if last := pod.calls[len(pod.calls)-1]; last != "start:0.0.0.0,13337,/vscode/ns/web" {
		t.Errorf("start called with %s", last)
	}
}
