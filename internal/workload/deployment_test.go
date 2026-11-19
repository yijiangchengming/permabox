package workload

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveDeployment(t *testing.T) {
	const (
		podPath = "/api/v1/namespaces/test/pods/pod"
		rsPath  = "/apis/apps/v1/namespaces/test/replicasets/rs"
		depPath = "/apis/apps/v1/namespaces/test/deployments/app"
		pod     = `{"metadata":{"uid":"pod-id","annotations":{"permabox.io/reuse-policy":"deployment-local","permabox.io/gc-on-workload-delete":"true"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"rs","uid":"rs-id","controller":true}]}}`
		rs      = `{"metadata":{"uid":"rs-id","ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"app","uid":"app-id","controller":true}]}}`
		dep     = `{"metadata":{"uid":"app-id"}}`
	)
	owner := Owner{Namespace: "test", Kind: "Deployment", Name: "app", UID: "app-id", Overrides: Overrides{Delete: "true"}}
	for _, tc := range []struct {
		name, pod, rs, dep string
		depStatus          int
		want               Owner
		wantErr            bool
	}{
		{name: "verified", pod: pod, rs: rs, dep: dep, want: owner},
		{name: "missing opt-in", pod: strings.Replace(pod, `"permabox.io/reuse-policy":"deployment-local",`, "", 1)},
		{name: "wrong opt-in", pod: strings.Replace(pod, "deployment-local", "other", 1)},
		{name: "replacement pod", pod: strings.Replace(pod, "pod-id", "new-pod", 1)},
		{name: "non-replicaset owner", pod: strings.Replace(pod, "ReplicaSet", "StatefulSet", 1)},
		{name: "not a controller", pod: strings.Replace(pod, `"controller":true`, `"controller":false`, 1)},
		{name: "replacement replicaset", pod: pod, rs: strings.Replace(rs, "rs-id", "new-rs", 1)},
		{name: "non-deployment owner", pod: pod, rs: strings.Replace(rs, "Deployment", "StatefulSet", 1)},
		{name: "replacement deployment", pod: pod, rs: rs, dep: `{"metadata":{"uid":"new-app"}}`},
		{name: "deleting deployment", pod: pod, rs: rs, dep: `{"metadata":{"uid":"app-id","deletionTimestamp":"2026-11-19T12:00:00Z"}}`},
		{name: "missing deployment", pod: pod, rs: rs, dep: `{"reason":"NotFound","details":{"name":"app"}}`, depStatus: 404},
		{name: "unverified deployment", pod: pod, rs: rs, dep: `{}`, depStatus: 403, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testMetadataBridge(t, func(w http.ResponseWriter, r *http.Request) {
				var body string
				switch r.URL.Path {
				case podPath:
					body = tc.pod
				case rsPath:
					body = tc.rs
				case depPath:
					body = tc.dep
					if tc.depStatus != 0 {
						w.WriteHeader(tc.depStatus)
					}
				}
				if body == "" {
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
				fmt.Fprint(w, body)
			})
			got, err := client.ResolveDeployment(context.Background(), "test", "pod", "pod-id")
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("ResolveDeployment = %+v, %v; want %+v, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestPodFinished(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		status        int
		want, wantErr bool
	}{
		{"absent", `{"reason":"NotFound","details":{"name":"pod"}}`, 404, true, false},
		{"replacement", `{"metadata":{"uid":"new-pod"}}`, 200, true, false},
		{"succeeded", `{"metadata":{"uid":"pod-id"},"status":{"phase":"Succeeded"}}`, 200, true, false},
		{"failed", `{"metadata":{"uid":"pod-id"},"status":{"phase":"Failed"}}`, 200, true, false},
		{"terminating", `{"metadata":{"uid":"pod-id","deletionTimestamp":"2026-11-19T12:00:00Z"},"status":{"phase":"Running"}}`, 200, false, false},
		{"pending", `{"metadata":{"uid":"pod-id"},"status":{"phase":"Pending"}}`, 200, false, false},
		{"unknown phase", `{"metadata":{"uid":"pod-id"},"status":{"phase":"Unknown"}}`, 200, false, false},
		{"missing UID", `{"status":{"phase":"Failed"}}`, 200, false, true},
		{"unverified 404", `{"reason":"NotFound","details":{"name":"other"}}`, 404, false, true},
		{"forbidden", `{}`, 403, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testMetadataBridge(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/namespaces/test/pods/pod" {
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			got, err := client.PodFinished(context.Background(), "test", "pod", "pod-id")
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("PodFinished = %v, %v; want %v, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func testMetadataBridge(t *testing.T, upstreamHandler http.HandlerFunc) Client {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)
	upstreamClient := Client{base: upstream.URL, http: upstream.Client()}
	bridge := httptest.NewServer(http.HandlerFunc(upstreamClient.serveMetadata))
	t.Cleanup(bridge.Close)
	return Client{base: bridge.URL, http: bridge.Client()}
}
