// Package k8s resolves pod names to sshd addresses from a pod informer cache,
// so connections never hit the API server directly.
//
// A pod is reachable when it carries the annotation k8s-pod-ssh-relay/ssh: "true",
// and only by the keys listed in its k8s-pod-ssh-relay/authorized-keys annotation.
// Annotations can't be used as a server-side selector, so the informer watches
// every pod in the namespace and the annotation is checked locally.
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
	namespace   string
	defaultPort int
	pods        corelisters.PodLister
}

// NewResolver starts a pod informer for namespace and blocks until its cache
// has synced. The informer stops when ctx is cancelled.
func NewResolver(ctx context.Context, namespace string, defaultPort int) (*Resolver, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
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
	r := &Resolver{namespace: namespace, defaultPort: defaultPort, pods: podInformer.Lister()}

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
		return nil, err
	}

	factory.Start(ctx.Done())
	for typ, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return nil, fmt.Errorf("informer cache for %v did not sync", typ)
		}
	}
	return r, nil
}

// Resolve returns the host:port of the named pod's sshd.
func (r *Resolver) Resolve(name string) (string, error) {
	pod, err := r.get(name)
	if err != nil {
		return "", err
	}
	return r.addr(pod)
}

// AuthorizedKeys returns the raw k8s-pod-ssh-relay/authorized-keys annotation of an
// enabled pod.
func (r *Resolver) AuthorizedKeys(name string) (string, error) {
	pod, err := r.get(name)
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

func (r *Resolver) get(name string) (*corev1.Pod, error) {
	if name == "" {
		return nil, errors.New("missing target")
	}
	pod, err := r.pods.Pods(r.namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("pod %s/%s not found", r.namespace, name)
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
		log.Printf("pod %s: unreachable", oldPod.Name)
	default:
		log.Printf("pod %s: reachable at %s", newPod.Name, after)
	}
}
