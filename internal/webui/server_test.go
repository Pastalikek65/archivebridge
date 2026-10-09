package webui

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
)

func viewerFixture(t *testing.T) (*httptest.Server, *bridge.Manifest, []byte, string) {
	t.Helper()
	base := t.TempDir()
	var pixels bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 12), uint8(y * 16), 120, 255})
		}
	}
	if err := png.Encode(&pixels, img); err != nil {
		t.Fatal(err)
	}
	var source bytes.Buffer
	zw := zip.NewWriter(&source)
	for name, content := range map[string][]byte{
		"Takeout/Google Photos/Test album/synthetic.png":      pixels.Bytes(),
		"Takeout/Google Photos/Test album/synthetic.png.json": []byte(`{"title":"synthetic.png","photoTakenTime":{"timestamp":"1700000000"}}`),
	} {
		member, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(base, "part.zip")
	if err := os.WriteFile(sourcePath, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := bridge.Inspect(context.Background(), []string{sourcePath}, bridge.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "portable")
	if _, err := bridge.Export(context.Background(), plan, out); err != nil {
		t.Fatal(err)
	}
	manifest, err := bridge.ReadManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = newHandler(out, root, manifest, server.Listener.Addr().String())
	server.Start()
	t.Cleanup(server.Close)
	return server, manifest, pixels.Bytes(), out
}

func responseBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestViewerLoadsRealExportAndVerifiesWithoutWrites(t *testing.T) {
	server, manifest, _, out := viewerFixture(t)
	before, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Get(server.URL + "/api/manifest")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("manifest status %d", response.StatusCode)
	}
	var received bridge.Manifest
	if err := json.Unmarshal(responseBody(t, response), &received); err != nil {
		t.Fatal(err)
	}
	if received.PlanID != manifest.PlanID || len(received.Files) != 1 {
		t.Fatal("viewer did not return the actual export manifest")
	}
	response, err = server.Client().Get(server.URL + "/api/verify")
	if err != nil {
		t.Fatal(err)
	}
	var report bridge.VerifyReport
	if err := json.Unmarshal(responseBody(t, response), &report); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || report.Status != "ok" || report.FilesChecked != 1 {
		t.Fatalf("verification result: %+v, HTTP %d", report, response.StatusCode)
	}
	after, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only viewer changed the manifest")
	}
}

func TestViewerRejectsForeignOriginsAndNonReadMethods(t *testing.T) {
	server, _, _, _ := viewerFixture(t)
	for _, control := range []struct {
		name, method, host, origin, site string
		status                           int
	}{
		{"foreign-host", "GET", "example.invalid", "", "", 403},
		{"foreign-origin", "GET", "", "https://example.invalid", "", 403},
		{"cross-site", "GET", "", "", "cross-site", 403},
		{"no-write", "POST", "", "", "", 405},
	} {
		t.Run(control.name, func(t *testing.T) {
			request, _ := http.NewRequest(control.method, server.URL+"/api/manifest", strings.NewReader("do-not-write"))
			if control.host != "" {
				request.Host = control.host
			}
			request.Header.Set("Origin", control.origin)
			request.Header.Set("Sec-Fetch-Site", control.site)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body := responseBody(t, response)
			if response.StatusCode != control.status || bytes.Contains(body, []byte("sourceIndex")) {
				t.Fatalf("unexpected rejection: HTTP %d %s", response.StatusCode, body)
			}
		})
	}
}

func TestViewerDownloadsExactOriginalAndSetsSecurityHeaders(t *testing.T) {
	server, manifest, original, _ := viewerFixture(t)
	response, err := server.Client().Get(server.URL + "/api/media/" + manifest.Files[0].ID + "?download=1")
	if err != nil {
		t.Fatal(err)
	}
	body := responseBody(t, response)
	if response.StatusCode != 200 || !bytes.Equal(body, original) || !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatal("download did not preserve the original")
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Cross-Origin-Resource-Policy") != "same-origin" || !strings.Contains(response.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("required viewer security headers absent")
	}
	checksum := sha256.Sum256(body)
	if hex.EncodeToString(checksum[:]) != manifest.Files[0].SHA256 {
		t.Fatal("original download hash differs")
	}
	response, err = server.Client().Get(server.URL + "/api/media/" + manifest.Files[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "image/png" {
		t.Fatal("PNG preview failed")
	}
	_ = responseBody(t, response)
}

func TestViewerReportsChangedArchive(t *testing.T) {
	server, manifest, _, out := viewerFixture(t)
	if err := os.WriteFile(filepath.Join(out, filepath.FromSlash(manifest.Files[0].OutputPath)), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Get(server.URL + "/api/verify")
	if err != nil {
		t.Fatal(err)
	}
	body := responseBody(t, response)
	if response.StatusCode != 422 || !bytes.Contains(body, []byte(`"status":"failed"`)) {
		t.Fatalf("tamper result: %d %s", response.StatusCode, body)
	}
	request, _ := http.NewRequest(http.MethodHead, server.URL+"/api/verify", nil)
	head, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	headBody := responseBody(t, head)
	if head.StatusCode != 422 || len(headBody) != 0 {
		t.Fatalf("HEAD verification status differs: HTTP %d, %d body bytes", head.StatusCode, len(headBody))
	}
}

func TestViewerRefusesExternalListener(t *testing.T) {
	if err := Serve(context.Background(), t.TempDir(), "0.0.0.0:4175"); err == nil {
		t.Fatal("external listener accepted")
	}
}

func TestViewerRejectsManifestChangedAfterLoading(t *testing.T) {
	server, _, _, out := viewerFixture(t)
	base := filepath.Dir(out)
	raw, err := os.ReadFile(filepath.Join(base, "part.zip"))
	if err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(base, "renamed-part.zip")
	if err := os.WriteFile(renamed, raw, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := bridge.Inspect(context.Background(), []string{renamed}, bridge.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "other")
	if _, err := bridge.Export(context.Background(), plan, other); err != nil {
		t.Fatal(err)
	}
	// Same original bytes, but a different valid source manifest and ownership
	// record. Disk verification passes; the viewer must reject its stale snapshot.
	for _, name := range []string{"manifest.json", ".archivebridge-owner.json"} {
		data, err := os.ReadFile(filepath.Join(other, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	verified, err := bridge.Verify(context.Background(), out)
	if err != nil || verified.Status != "ok" {
		t.Fatalf("control manifest invalid: %v %+v", err, verified)
	}
	response, err := server.Client().Get(server.URL + "/api/verify")
	if err != nil {
		t.Fatal(err)
	}
	body := responseBody(t, response)
	if response.StatusCode != 422 || !bytes.Contains(body, []byte(`"status":"failed"`)) {
		t.Fatalf("changed manifest was endorsed: HTTP %d %s", response.StatusCode, body)
	}
}
