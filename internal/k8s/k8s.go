// Package k8s resolves namespace/pod names to sshd addresses from a pod
// informer cache, so connections never hit the API server directly.
//
// A pod is reachable when it carries the annotation k8s-pod-ssh-relay/ssh: "true",
// and only by the keys listed in its k8s-pod-ssh-relay/authorized-keys annotation.
// Annotations can't be used as a server-side selector, so the informer watches
// every pod in the watched namespaces and the annotation is checked locally.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	// AnnotationEnabled must be "true" for a pod to be reachable via the relay.
	AnnotationEnabled = "k8s-pod-ssh-relay/ssh"
	// AnnotationPort optionally overrides the sshd port (default 22).
	AnnotationPort = "k8s-pod-ssh-relay/ssh-port"
	// AnnotationAuthorizedKeys lists the public keys, in authorized_keys
	// format, allowed to reach the pod through the relay.
	AnnotationAuthorizedKeys = "k8s-pod-ssh-relay/authorized-keys"
)

type Resolver struct {
	defaultPort int
	// listers by namespace; the key "" means all namespaces.
	listers map[string]corelisters.PodLister
}

// NewResolver starts pod informers for the given namespaces (all namespaces
// if none are given) and blocks until their caches have synced. The
// informers stop when ctx is cancelled.
func NewResolver(ctx context.Context, namespaces []string, defaultPort int) (*Resolver, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	r := &Resolver{defaultPort: defaultPort, listers: map[string]corelisters.PodLister{}}
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	for _, ns := range namespaces {
		if err := r.watch(ctx, cs, ns); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Resolver) watch(ctx context.Context, cs kubernetes.Interface, namespace string) error {
	factory := informers.NewSharedInformerFactoryWithOptions(cs, 10*time.Minute,
		informers.WithNamespace(namespace),
		// managedFields are never used here and are the bulk of each object.
		informers.WithTransform(func(obj any) (any, error) {
			if m, err := meta.Accessor(obj); err == nil {
				m.SetManagedFields(nil)
			}
			return obj, nil
		}),
	)
	podInformer := factory.Core().V1().Pods()
	r.listers[namespace] = podInformer.Lister()

	if _, err := podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { r.logChange(nil, obj.(*corev1.Pod)) },
		UpdateFunc: func(oldObj, newObj any) {
			r.logChange(oldObj.(*corev1.Pod), newObj.(*corev1.Pod))
		},
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			if pod, ok := obj.(*corev1.Pod); ok {
				r.logChange(pod, nil)
			}
		},
	}); err != nil {
		return err
	}

	factory.Start(ctx.Done())
	for typ, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("informer cache for %v in namespace %q did not sync", typ, namespace)
		}
	}
	return nil
}

// Resolve returns the host:port of the pod's sshd.
func (r *Resolver) Resolve(namespace, name string) (string, error) {
	pod, err := r.get(namespace, name)
	if err != nil {
		return "", err
	}
	return r.addr(pod)
}

// AuthorizedKeys returns the raw k8s-pod-ssh-relay/authorized-keys annotation of an
// enabled pod.
func (r *Resolver) AuthorizedKeys(namespace, name string) (string, error) {
	pod, err := r.get(namespace, name)
	if err != nil {
		return "", err
	}
	if pod.Annotations[AnnotationEnabled] != "true" {
		return "", fmt.Errorf("pod %s/%s is not annotated %s=true", pod.Namespace, pod.Name, AnnotationEnabled)
	}
	keys, ok := pod.Annotations[AnnotationAuthorizedKeys]
	if !ok {
		return "", fmt.Errorf("pod %s/%s has no %s annotation", pod.Namespace, pod.Name, AnnotationAuthorizedKeys)
	}
	return keys, nil
}

func (r *Resolver) get(namespace, name string) (*corev1.Pod, error) {
	if namespace == "" || name == "" {
		return nil, errors.New("target must be namespace/pod")
	}
	lister, ok := r.listers[metav1.NamespaceAll]
	if !ok {
		if lister, ok = r.listers[namespace]; !ok {
			return nil, fmt.Errorf("namespace %s is not watched by the relay", namespace)
		}
	}
	pod, err := lister.Pods(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("pod %s/%s not found", namespace, name)
	}
	return pod, err
}

func (r *Resolver) addr(pod *corev1.Pod) (string, error) {
	if pod.Annotations[AnnotationEnabled] != "true" {
		return "", fmt.Errorf("pod %s/%s is not annotated %s=true", pod.Namespace, pod.Name, AnnotationEnabled)
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod %s/%s is not running", pod.Namespace, pod.Name)
	}
	port := r.defaultPort
	if v, ok := pod.Annotations[AnnotationPort]; ok {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return "", fmt.Errorf("pod %s/%s has invalid %s=%q", pod.Namespace, pod.Name, AnnotationPort, v)
		}
		port = p
	}
	return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port)), nil
}

// logChange logs when a pod becomes reachable, changes address, or stops
// being reachable. Either argument may be nil.
func (r *Resolver) logChange(oldPod, newPod *corev1.Pod) {
	var before, after string
	if oldPod != nil {
		before, _ = r.addr(oldPod)
	}
	if newPod != nil {
		after, _ = r.addr(newPod)
	}
	switch {
	case before == after:
	case after == "":
		log.Printf("pod %s/%s: unreachable", oldPod.Namespace, oldPod.Name)
	default:
		log.Printf("pod %s/%s: reachable at %s", newPod.Namespace, newPod.Name, after)
	}
}
