package podresolver

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const keys = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIF4Rq9htwer+4pZSE1T4vl44J4nY9nRte8YH+ZmxVTTS test"

func pod(ns, name string, annotations map[string]string, phase corev1.PodPhase, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: annotations},
		Status:     corev1.PodStatus{Phase: phase, PodIP: ip},
	}
}

func enabled(extra ...string) map[string]string {
	a := map[string]string{AnnotationEnabled: "true", AnnotationAuthorizedKeys: keys}
	for i := 0; i+1 < len(extra); i += 2 {
		a[extra[i]] = extra[i+1]
	}
	return a
}

func newResolver(t *testing.T, namespaces []string, pods ...*corev1.Pod) (*Resolver, *fake.Clientset) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var objs []runtime.Object
	for _, p := range pods {
		objs = append(objs, p)
	}
	cs := fake.NewClientset(objs...)
	r, err := NewResolverFromClient(ctx, cs, namespaces, 22, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return r, cs
}

func TestResolve(t *testing.T) {
	r, _ := newResolver(t, nil,
		pod("a", "ok", enabled(), corev1.PodRunning, "10.0.0.1"),
		pod("b", "custom-port", enabled(AnnotationPort, "2222"), corev1.PodRunning, "10.0.0.2"),
		pod("a", "bad-port", enabled(AnnotationPort, "ssh"), corev1.PodRunning, "10.0.0.3"),
		pod("a", "not-enabled", map[string]string{AnnotationAuthorizedKeys: keys}, corev1.PodRunning, "10.0.0.4"),
		pod("a", "pending", enabled(), corev1.PodPending, ""),
	)
	tests := []struct {
		ns, name, want, wantErr string
	}{
		{ns: "a", name: "ok", want: "10.0.0.1:22"},
		{ns: "b", name: "custom-port", want: "10.0.0.2:2222"},
		{ns: "a", name: "bad-port", wantErr: "invalid"},
		{ns: "a", name: "not-enabled", wantErr: "not annotated"},
		{ns: "a", name: "pending", wantErr: "not running"},
		{ns: "a", name: "missing", wantErr: "not found"},
		{ns: "b", name: "ok", wantErr: "not found"}, // right name, wrong namespace
		{ns: "", name: "ok", wantErr: "namespace/pod"},
	}
	for _, tt := range tests {
		got, err := r.Resolve(tt.ns, tt.name)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Resolve(%s/%s) error = %v, want %q", tt.ns, tt.name, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%s/%s) = %q, %v; want %q", tt.ns, tt.name, got, err, tt.want)
		}
	}
}

func TestAuthorizedKeys(t *testing.T) {
	r, _ := newResolver(t, nil,
		pod("a", "ok", enabled(), corev1.PodRunning, "10.0.0.1"),
		pod("a", "no-keys", map[string]string{AnnotationEnabled: "true"}, corev1.PodRunning, "10.0.0.2"),
		pod("a", "not-enabled", map[string]string{AnnotationAuthorizedKeys: keys}, corev1.PodRunning, "10.0.0.3"),
	)
	if got, err := r.AuthorizedKeys("a", "ok"); err != nil || got != keys {
		t.Errorf("AuthorizedKeys(a/ok) = %q, %v", got, err)
	}
	if _, err := r.AuthorizedKeys("a", "no-keys"); err == nil || !strings.Contains(err.Error(), "no "+AnnotationAuthorizedKeys) {
		t.Errorf("AuthorizedKeys(a/no-keys) error = %v", err)
	}
	if _, err := r.AuthorizedKeys("a", "not-enabled"); err == nil || !strings.Contains(err.Error(), "not annotated") {
		t.Errorf("AuthorizedKeys(a/not-enabled) error = %v", err)
	}
}

func TestOnlyWatchedNamespaces(t *testing.T) {
	r, _ := newResolver(t, []string{"a"},
		pod("a", "ok", enabled(), corev1.PodRunning, "10.0.0.1"),
		pod("b", "ok", enabled(), corev1.PodRunning, "10.0.0.2"),
	)
	if _, err := r.Resolve("a", "ok"); err != nil {
		t.Errorf("Resolve(a/ok): %v", err)
	}
	if _, err := r.Resolve("b", "ok"); err == nil || !strings.Contains(err.Error(), "not watched") {
		t.Errorf("Resolve(b/ok) error = %v, want not watched", err)
	}
}

func TestFollowsAnnotationChanges(t *testing.T) {
	r, cs := newResolver(t, nil, pod("a", "web", enabled(), corev1.PodRunning, "10.0.0.1"))
	ctx := context.Background()

	p, err := cs.CoreV1().Pods("a").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delete(p.Annotations, AnnotationEnabled)
	if _, err := cs.CoreV1().Pods("a").Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, err := r.Resolve("a", "web"); return err != nil }, "revoking the annotation")

	if err := cs.CoreV1().Pods("a").Delete(ctx, "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		_, err := r.Resolve("a", "web")
		return err != nil && strings.Contains(err.Error(), "not found")
	}, "deleting the pod")
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("informer did not observe %s", what)
}

func TestPodIP(t *testing.T) {
	r, _ := newResolver(t, nil,
		pod("a", "ok", enabled(AnnotationPort, "2222"), corev1.PodRunning, "10.0.0.1"),
		pod("a", "pending", enabled(), corev1.PodPending, ""),
	)
	if ip, err := r.PodIP("a", "ok"); err != nil || ip != "10.0.0.1" {
		t.Errorf("PodIP(a/ok) = %q, %v", ip, err)
	}
	if _, err := r.PodIP("a", "pending"); err == nil {
		t.Error("PodIP of a pending pod should fail")
	}
}
