package workload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

const DefaultSocket = "/opt/permabox-k3s/workload.sock"

func CheckSocket(path string) error {
	c := UnixClient(path)
	c.http.Timeout = 4 * time.Second
	resp, err := c.request(context.Background(), "/readyz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return errors.New("workload bridge cannot read Kubernetes metadata")
	}
	return nil
}

func UnixClient(path string) Client {
	return Client{base: "http://permabox.local", http: &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialBridge(ctx, path)
		}},
		CheckRedirect: noRedirect,
	}}
}

func checkSocketDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("workload socket must have a clean absolute path")
	}
	dir, err := trustedpath.OpenDir(filepath.Dir(path), false, true)
	if err != nil {
		return fmt.Errorf("workload socket directory: %w", err)
	}
	return dir.Close()
}

func dialBridge(ctx context.Context, path string) (net.Conn, error) {
	if err := checkSocketDir(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	uid := uint32(os.Geteuid())
	if err := checkSocketOwner(info, uid); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := checkBridgePeer(conn.(*net.UnixConn), uid); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func checkSocketOwner(info os.FileInfo, uid uint32) error {
	if info.Mode()&os.ModeSocket == 0 || info.Sys().(*syscall.Stat_t).Uid != uid {
		return errors.New("workload socket must be a Unix socket owned by the runtime user")
	}
	return nil
}

func checkBridgePeer(conn *net.UnixConn, uid uint32) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peer *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		peer, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if peer.Uid != uid {
		return fmt.Errorf("workload socket peer UID %d does not match runtime UID %d", peer.Uid, uid)
	}
	return nil
}

func noRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func (c Client) request(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.tokenPath != "" {
		token, err := os.ReadFile(c.tokenPath)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	return c.http.Do(req)
}

func inCluster() (Client, error) {
	const dir = "/var/run/secrets/kubernetes.io/serviceaccount/"
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return Client{}, errors.New("Kubernetes API service address not set")
	}
	cert, err := os.ReadFile(dir + "ca.crt")
	if err != nil {
		return Client{}, err
	}
	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(cert) {
		return Client{}, errors.New("invalid Kubernetes API CA")
	}
	return Client{base: "https://" + net.JoinHostPort(host, port), tokenPath: dir + "token", http: &http.Client{
		Timeout:       1500 * time.Millisecond,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca, MinVersion: tls.VersionTLS12}},
		CheckRedirect: noRedirect,
	}}, nil
}

// Serve exposes only the fields needed for ownership and reclamation. It
// keeps the Kubernetes credentials inside the installer Pod.
func Serve(path string) error {
	c, err := inCluster()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(c.tokenPath), "namespace"))
	if err != nil {
		return err
	}
	namespace := strings.TrimSpace(string(data))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", c.readiness(namespace))
	mux.HandleFunc("/", c.serveMetadata)
	if err := checkSocketDir(path); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	if socket, err := os.Lstat(path); err == nil {
		if err := checkSocketOwner(socket, uint32(os.Geteuid())); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	srv := http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout: 4 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	return srv.Serve(listener)
}

func (c Client) readiness(namespace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, kind := range []string{"Pod", "StatefulSet"} {
			// An authoritative NotFound also demonstrates authenticated read access.
			if _, err := c.get(r.Context(), kind, namespace, "permabox-readiness"); err != nil && !errors.Is(err, errNotFound) {
				http.Error(w, "Kubernetes metadata unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (c Client) serveMetadata(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if r.Method != http.MethodGet || len(parts) < 3 || r.URL.RawQuery != "" {
		http.Error(w, "invalid metadata request", http.StatusBadRequest)
		return
	}
	name, resource, namespace := parts[len(parts)-1], parts[len(parts)-2], parts[len(parts)-3]
	for kind, plural := range collection {
		if plural != resource {
			continue
		}
		path, err := resourcePath(kind, namespace, name)
		if err != nil || r.URL.Path != path {
			break
		}
		obj, err := c.get(r.Context(), kind, namespace, name)
		w.Header().Set("Content-Type", "application/json")
		if errors.Is(err, errNotFound) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"reason": "NotFound", "details": map[string]string{"name": name}})
		} else if err != nil {
			http.Error(w, "Kubernetes metadata unavailable", http.StatusBadGateway)
		} else {
			annotations := obj.Metadata.Annotations
			obj.Metadata.Annotations = nil
			if kind == "Pod" {
				obj.Metadata.Annotations = make(map[string]string, 3)
				for _, key := range []string{deleteAnnotation, scaleToZeroAnnotation, persistExcludeAnnotation} {
					if value, ok := annotations[key]; ok {
						obj.Metadata.Annotations[key] = value
					}
				}
			}
			_ = json.NewEncoder(w).Encode(obj)
		}
		return
	}
	http.Error(w, "unsupported metadata request", http.StatusBadRequest)
}
