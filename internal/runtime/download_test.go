package runtime

import (
	"crypto/sha512"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// flakyServer serves payload, but drops the connection after dropAfter bytes
// on the first full request; Range requests are honoured afterwards.
func flakyServer(t *testing.T, payload []byte, dropAfter int) *httptest.Server {
	t.Helper()
	dropped := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := int64(0)
		if rng := r.Header.Get("Range"); rng != "" {
			i := strings.Index(rng, "=")
			if n, err := strconv.ParseInt(rng[i+1:], 10, 64); err == nil {
				start = n
			}
		} else if !dropped {
			// first visit: send a prefix, then kill the connection
			dropped = true
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:dropAfter])
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Range",
			"bytes "+strconv.FormatInt(start, 10)+"-"+strconv.Itoa(len(payload)-1)+"/"+strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start:])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDownloadResumesAfterDrop(t *testing.T) {
	payload := []byte(strings.Repeat("zcode-quick-web-forward resume test\n", 4096))
	srv := flakyServer(t, payload, 1000)

	f := &Finder{CacheRoot: t.TempDir(), HTTP: srv.Client()}
	dst := filepath.Join(f.CacheRoot, "artifact.deb")
	sum := base64.StdEncoding.EncodeToString(sha512Sum(payload))
	if err := f.download(srv.URL+"/artifact.deb", dst, sum); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("resumed download corrupted: got %d bytes, want %d", len(got), len(payload))
	}
	if !isUpToDate(dst, sum) {
		t.Fatal("downloaded file failed checksum validation")
	}
}

func TestDownloadFailsOnChecksumMismatch(t *testing.T) {
	payload := []byte("this payload will be served intact")
	srv := flakyServer(t, payload, 0)

	f := &Finder{CacheRoot: t.TempDir(), HTTP: srv.Client()}
	dst := filepath.Join(f.CacheRoot, "artifact.deb")
	wrong := base64.StdEncoding.EncodeToString(sha512Sum([]byte("something else")))
	if err := f.download(srv.URL+"/artifact.deb", dst, wrong); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("corrupt download should not be kept")
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatal("corrupt .part should be removed")
	}
}

func sha512Sum(b []byte) []byte {
	h := sha512.Sum512(b)
	return h[:]
}
