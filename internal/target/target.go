// Package target names the pod a client connects to.
//
// The canonical form is "namespace/pod": it is what the client sends in
// ?target= and signs during authentication. Where '/' can't be used (ssh
// host names, VS Code remote authorities) the dotted form "<pod>.<namespace>"
// is used instead; namespaces can't contain dots, so the last dot separates
// the two, and pod names may contain dots.
package target

import (
	"fmt"
	"regexp"
	"strings"
)

// Target is a pod in a namespace.
type Target struct {
	Namespace string
	Pod       string
}

var (
	// RFC 1123 label: namespace names.
	labelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// RFC 1123 subdomain: pod names.
	subdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// ParseCanonical parses "namespace/pod" only.
func ParseCanonical(s string) (Target, error) {
	ns, pod, ok := strings.Cut(s, "/")
	if !ok {
		return Target{}, fmt.Errorf("target %q: want namespace/pod", s)
	}
	return validate(Target{Namespace: ns, Pod: pod}, s)
}

// Parse parses "namespace/pod" or the dotted form "<pod>.<namespace>",
// optionally followed by ".relay" (the ssh host suffix).
func Parse(s string) (Target, error) {
	if strings.Contains(s, "/") {
		return ParseCanonical(s)
	}
	host := strings.TrimSuffix(s, ".relay")
	i := strings.LastIndex(host, ".")
	if i < 0 {
		return Target{}, fmt.Errorf("target %q: want namespace/pod or <pod>.<namespace>.relay", s)
	}
	return validate(Target{Namespace: host[i+1:], Pod: host[:i]}, s)
}

// String returns the canonical "namespace/pod".
func (t Target) String() string {
	return t.Namespace + "/" + t.Pod
}

// Dotted returns "<pod>.<namespace>".
func (t Target) Dotted() string {
	return t.Pod + "." + t.Namespace
}

func validate(t Target, s string) (Target, error) {
	if len(t.Namespace) > 63 || !labelRE.MatchString(t.Namespace) {
		return Target{}, fmt.Errorf("target %q: invalid namespace %q", s, t.Namespace)
	}
	if len(t.Pod) > 253 || !subdomainRE.MatchString(t.Pod) {
		return Target{}, fmt.Errorf("target %q: invalid pod name %q", s, t.Pod)
	}
	return t, nil
}
