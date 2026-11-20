package workload

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/yijiangchengming/permabox/internal/ownership"
)

func TestPodAnnotationsOverrideWorkloadDefaults(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/test/pods/pod":
			fmt.Fprint(w, `{"metadata":{"uid":"pod-id","annotations":{"permabox.io/gc-on-workload-delete":"true","permabox.io/gc-on-scale-to-zero":"false","other":"private"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"StatefulSet","name":"app","uid":"app-id","controller":true}]}}`)
		case "/apis/apps/v1/namespaces/test/statefulsets/app":
			fmt.Fprint(w, `{"metadata":{"uid":"app-id"}}`)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	upstreamClient := Client{base: upstream.URL, http: upstream.Client()}
	bridge := httptest.NewServer(http.HandlerFunc(upstreamClient.serveMetadata))
	defer bridge.Close()
	client := Client{base: bridge.URL, http: bridge.Client()}
	owner, err := client.Resolve(context.Background(), "test", "pod", "pod-id")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	var saved ownership.Owner
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if del, zero := ApplyOverrides(saved.Overrides, false, true); !del || zero {
		t.Fatalf("round-tripped overrides: delete=%v, zero=%v", del, zero)
	}
	obj, err := client.get(context.Background(), "Pod", "test", "pod")
	if err != nil || len(obj.Metadata.Annotations) != 2 {
		t.Fatalf("bridge exposed unexpected metadata: %+v, %v", obj.Metadata.Annotations, err)
	}
	if del, zero := ApplyOverrides(readOverrides(map[string]string{scaleToZeroAnnotation: ""}), true, true); !del || zero {
		t.Fatalf("omitted / invalid overrides: delete=%v, zero=%v", del, zero)
	}
}

func TestUnixBridgePreservesAuthoritativeNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/apis/apps/v1/namespaces/test/statefulsets/app"; r.URL.Path != want {
			t.Errorf("upstream path = %q; want %q", r.URL.Path, want)
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"reason":"NotFound","details":{"name":"app"}}`)
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "bridge.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c := Client{base: upstream.URL, http: upstream.Client()}
	server := &http.Server{Handler: http.HandlerFunc(c.serveMetadata)}
	defer server.Close()
	go server.Serve(listener)
	client := UnixClient(path)
	if ok, err := client.Reclaim(context.Background(), ownership.Owner{Namespace: "test", Kind: "StatefulSet", Name: "app", UID: "old-uid"}, true, false); err != nil || !ok {
		t.Fatalf("bridge reclaim = %v, %v", ok, err)
	}
	for _, path := range []string{
		"/api/v1/namespaces/test/secrets/app",
		"/apis/apps/v1/namespaces/test/deployments/app",
		"/apis/apps/v1/namespaces/test/replicasets/app",
	} {
		resp, err := client.request(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("unsupported path %s returned %d", path, resp.StatusCode)
		}
	}
}

func TestResolveStatefulSetOwner(t *testing.T) {
	for _, tc := range []struct{ name, uid, rsUID, liveSetUID, kind string }{
		{"replicaset ignored", "pod-id", "rs-id", "", ""},
		{"statefulset", "pod-id", "", "app-id", "StatefulSet"},
		{"replaced statefulset", "pod-id", "", "new-app-id", ""},
		{"replacement-pod", "old-pod", "rs-id", "", ""},
		{"standalone", "pod-id", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/namespaces/test/pods/pod":
					ref := `[]`
					if tc.rsUID != "" {
						ref = fmt.Sprintf(`[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"rs","uid":%q,"controller":true}]`, tc.rsUID)
					} else if tc.liveSetUID != "" {
						ref = `[{"apiVersion":"apps/v1","kind":"StatefulSet","name":"app","uid":"app-id","controller":true}]`
					}
					fmt.Fprintf(w, `{"metadata":{"uid":"pod-id","ownerReferences":%s}}`, ref)
				case "/apis/apps/v1/namespaces/test/statefulsets/app":
					fmt.Fprintf(w, `{"metadata":{"uid":%q}}`, tc.liveSetUID)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			c := Client{base: server.URL, http: server.Client()}
			got, err := c.Resolve(context.Background(), "test", "pod", tc.uid)
			wantError := tc.name == "replaced statefulset"
			if (err != nil) != wantError || got.Kind != tc.kind || (tc.kind != "" && got != (ownership.Owner{Namespace: "test", Kind: tc.kind, Name: "app", UID: "app-id"})) {
				t.Fatalf("owner = %+v, %v", got, err)
			}
		})
	}
}

func TestPolicyRequiresVerifiedWorkloadState(t *testing.T) {
	for _, tc := range []struct {
		name, body                  string
		status                      int
		want, deleteOnly, scaleOnly bool
	}{
		{"deleted", `{"reason":"NotFound","details":{"name":"app"}}`, 404, true, true, false},
		{"replacement", `{"metadata":{"uid":"other"},"spec":{"replicas":0}}`, 200, true, true, false},
		{"deletion accepted with finalizer", `{"metadata":{"uid":"app-id","deletionTimestamp":"2026-11-19T12:00:00Z"},"spec":{"replicas":0}}`, 200, true, true, false},
		{"zero", `{"metadata":{"uid":"app-id"},"spec":{"replicas":0}}`, 200, true, false, true},
		{"pod deleted or rolling update", `{"metadata":{"uid":"app-id"},"spec":{"replicas":1}}`, 200, false, false, false},
		{"missing replicas", `{"metadata":{"uid":"app-id"}}`, 200, false, false, false},
		{"missing UID", `{"spec":{"replicas":0}}`, 200, false, false, false},
		{"unrecognized 404", `{}`, 404, false, false, false},
		{"forbidden", `{"reason":"Forbidden"}`, 403, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c := Client{base: s.URL, http: s.Client()}
			owner := ownership.Owner{Namespace: "test", Kind: "StatefulSet", Name: "app", UID: "app-id"}
			for _, p := range []struct{ del, scale, want bool }{{true, true, tc.want}, {true, false, tc.deleteOnly}, {false, true, tc.scaleOnly}, {false, false, false}} {
				got, _ := c.Reclaim(context.Background(), owner, p.del, p.scale)
				if got != p.want {
					t.Fatalf("delete=%v scale=%v: reclaim=%v; want %v", p.del, p.scale, got, p.want)
				}
			}
		})
	}
}
