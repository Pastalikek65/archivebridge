// Package webui provides the read-only localhost view of a portable archive.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
)

//go:embed web/*
var assets embed.FS

// Serve listens only on a loopback IP and never modifies the portable archive.
func Serve(ctx context.Context, directory, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("viewer listen address must be a loopback IP and port, for example 127.0.0.1:4175")
	}
	manifest, err := bridge.ReadManifest(directory)
	if err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("viewer archive must be a regular directory")
	}
	archiveRoot, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer archiveRoot.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: newHandler(directory, archiveRoot, manifest, listener.Addr().String()), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	log.Printf("ArchiveBridge viewer: http://%s", listener.Addr())
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		<-finished
		return nil
	}
}

func newHandler(directory string, root *os.Root, manifest *bridge.Manifest, authority string) http.Handler {
	media := make(map[string]bridge.MediaOccurrence, len(manifest.Files))
	for _, item := range manifest.Files {
		media[item.ID] = item
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != authority || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+authority) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "This viewer is available only from its own localhost origin.", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Read-only viewer.", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/", "/app.js", "/styles.css":
			name := "index.html"
			if r.URL.Path != "/" {
				name = strings.TrimPrefix(r.URL.Path, "/")
			}
			data, err := assets.ReadFile("web/" + name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			contentType := map[string]string{"index.html": "text/html; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "styles.css": "text/css; charset=utf-8"}[name]
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			_, _ = w.Write(data)
		case "/api/manifest":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(manifest)
		case "/api/verify":
			loaded, loadErr := bridge.ReadManifest(directory)
			if loadErr != nil || !reflect.DeepEqual(loaded, manifest) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "error": "The manifest changed after the viewer opened. Restart the viewer and verify again."})
				return
			}
			report, err := bridge.Verify(r.Context(), directory)
			after, afterErr := bridge.ReadManifest(directory)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if err != nil || report == nil || afterErr != nil || !reflect.DeepEqual(after, manifest) || report.PlanID != manifest.PlanID {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "error": "Archive verification failed. Run the CLI verify command for details."})
				return
			}
			if report.Status != "ok" {
				w.WriteHeader(http.StatusUnprocessableEntity)
			}
			_ = json.NewEncoder(w).Encode(report)
		default:
			if !strings.HasPrefix(r.URL.Path, "/api/media/") {
				http.NotFound(w, r)
				return
			}
			item, ok := media[strings.TrimPrefix(r.URL.Path, "/api/media/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			serveMedia(w, r, root, item)
		}
	})
}

func serveMedia(w http.ResponseWriter, r *http.Request, root *os.Root, item bridge.MediaOccurrence) {
	if path.IsAbs(item.OutputPath) || path.Clean(item.OutputPath) != item.OutputPath || strings.ContainsAny(item.OutputPath, "\\\x00:") || !strings.HasPrefix(item.OutputPath, "media/") {
		http.Error(w, "Invalid media reference.", http.StatusUnprocessableEntity)
		return
	}
	for current := item.OutputPath; current != "."; current = path.Dir(current) {
		info, err := root.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			http.NotFound(w, r)
			return
		}
	}
	file, err := root.Open(item.OutputPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != item.Bytes {
		http.Error(w, "Media size differs from the manifest. Run verify.", http.StatusUnprocessableEntity)
		return
	}
	preview := r.URL.Query().Get("download") != "1" && (item.MIME == "image/jpeg" || item.MIME == "image/png")
	if preview {
		config, format, err := image.DecodeConfig(io.LimitReader(file, 1<<20))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 32768 || config.Height > 32768 || int64(config.Width)*int64(config.Height) > 64_000_000 || (format == "jpeg" && item.MIME != "image/jpeg") || (format == "png" && item.MIME != "image/png") {
			http.Error(w, "This image is outside the viewer preview limits. Download the original instead.", http.StatusUnprocessableEntity)
			return
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "Media could not be read.", http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", item.MIME)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(item.EntryPath)}))
	}
	http.ServeContent(w, r, path.Base(item.EntryPath), info.ModTime(), file)
}
