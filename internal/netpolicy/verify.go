// Package netpolicy provides a startup guard that verifies the cluster will
// actually enforce the sandbox egress NetworkPolicy.
//
// This matters because a Kubernetes NetworkPolicy is INERT unless the cluster's
// CNI (network plugin) enforces it. On clusters without an enforcing CNI (e.g.
// kind's default kindnet, or an AKS cluster created without --network-policy),
// the policy object exists but does nothing: untrusted sandbox code can reach
// the internet directly and bypass the credential-injecting proxy. That is a
// silent, fail-OPEN security hole.
//
// The orchestrator uses Verify to fail CLOSED instead: if enforcement cannot be
// confirmed, it refuses to start (unless explicitly configured otherwise).
package netpolicy

import (
	"context"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Options controls the verification behavior.
type Options struct {
	// Namespace where sandbox pods (and their NetworkPolicy) live.
	Namespace string
	// SandboxPodLabelKey/Value identify sandbox pods; the deny policy must select
	// them. Defaults to app=sandbox.
	SandboxPodLabelKey   string
	SandboxPodLabelValue string
	// EnforcedOverride, when non-nil, bypasses CNI auto-detection: true asserts
	// the operator has confirmed enforcement (e.g. GKE Dataplane V2, EKS VPC-CNI
	// policy, a managed engine we can't fingerprint); false forces a failure.
	EnforcedOverride *bool
}

// knownEnforcingDaemonSets are name substrings of DaemonSets that indicate a
// CNI / policy engine which enforces NetworkPolicy. Matching is case-insensitive
// substring on DaemonSet names in kube-system.
var knownEnforcingDaemonSets = []string{
	"calico-node", // Calico
	"cilium",      // Cilium
	"anetd",       // GKE Dataplane V2 (Cilium-based)
	"azure-npm",   // AKS Azure Network Policy Manager
	"kube-router", // kube-router
	"weave-net",   // Weave Net
}

// Result describes what Verify found, for logging.
type Result struct {
	PolicyFound   bool
	PolicyName    string
	EnforcerFound bool
	EnforcerName  string
	// Reason is a human-readable explanation when enforcement is NOT confirmed.
	Reason string
}

// Verify checks that (a) an egress NetworkPolicy selecting the sandbox pods
// exists in the namespace, and (b) the cluster runs a CNI/engine that enforces
// NetworkPolicy. It returns a non-nil error when enforcement cannot be
// confirmed. The Result is always returned (even on error) for logging.
func Verify(ctx context.Context, cs kubernetes.Interface, opts Options) (Result, error) {
	if opts.SandboxPodLabelKey == "" {
		opts.SandboxPodLabelKey = "app"
	}
	if opts.SandboxPodLabelValue == "" {
		opts.SandboxPodLabelValue = "sandbox"
	}

	var res Result

	// (a) An egress NetworkPolicy that selects the sandbox pods must exist.
	pol, err := findSandboxEgressPolicy(ctx, cs, opts)
	if err != nil {
		res.Reason = fmt.Sprintf("could not list NetworkPolicies in namespace %q: %v", opts.Namespace, err)
		return res, fmt.Errorf("network policy verification failed: %s", res.Reason)
	}
	if pol == "" {
		res.Reason = fmt.Sprintf("no egress NetworkPolicy in namespace %q selects sandbox pods (%s=%s); "+
			"apply deploy/k8s/20-networkpolicy-sandbox.yaml", opts.Namespace, opts.SandboxPodLabelKey, opts.SandboxPodLabelValue)
		return res, fmt.Errorf("network policy verification failed: %s", res.Reason)
	}
	res.PolicyFound = true
	res.PolicyName = pol

	// (b) Enforcement. An explicit override wins over auto-detection.
	if opts.EnforcedOverride != nil {
		if *opts.EnforcedOverride {
			res.EnforcerFound = true
			res.EnforcerName = "operator-asserted (SANDBOX_NETWORK_POLICY_ENFORCED=true)"
			return res, nil
		}
		res.Reason = "operator set SANDBOX_NETWORK_POLICY_ENFORCED=false"
		return res, fmt.Errorf("network policy enforcement disabled by operator: %s", res.Reason)
	}

	name, err := detectEnforcingCNI(ctx, cs)
	if err != nil {
		res.Reason = fmt.Sprintf("could not list DaemonSets in kube-system to detect a policy-enforcing CNI: %v", err)
		return res, fmt.Errorf("network policy verification failed: %s", res.Reason)
	}
	if name == "" {
		res.Reason = "no known NetworkPolicy-enforcing CNI detected in kube-system " +
			"(looked for calico-node, cilium, anetd, azure-npm, kube-router, weave-net). " +
			"Untrusted sandbox egress would NOT be contained. Install/enable a policy-enforcing " +
			"CNI, or if you have confirmed enforcement another way (e.g. GKE Dataplane V2, EKS " +
			"VPC-CNI NetworkPolicy) set SANDBOX_NETWORK_POLICY_ENFORCED=true to override."
		return res, fmt.Errorf("network policy enforcement not confirmed: %s", res.Reason)
	}
	res.EnforcerFound = true
	res.EnforcerName = name
	return res, nil
}

// findSandboxEgressPolicy returns the name of a NetworkPolicy in the namespace
// that has an Egress policy type and whose podSelector matches the sandbox
// label, or "" if none is found.
func findSandboxEgressPolicy(ctx context.Context, cs kubernetes.Interface, opts Options) (string, error) {
	list, err := cs.NetworkingV1().NetworkPolicies(opts.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for i := range list.Items {
		np := &list.Items[i]
		if !hasEgressType(np) {
			continue
		}
		if selectorMatches(np.Spec.PodSelector.MatchLabels, opts.SandboxPodLabelKey, opts.SandboxPodLabelValue) {
			return np.Name, nil
		}
	}
	return "", nil
}

func hasEgressType(np *networkingv1.NetworkPolicy) bool {
	for _, t := range np.Spec.PolicyTypes {
		if t == networkingv1.PolicyTypeEgress {
			return true
		}
	}
	return false
}

// selectorMatches reports whether an empty selector (matches all pods, including
// sandboxes) or an explicit label match covers the sandbox pods.
func selectorMatches(matchLabels map[string]string, key, value string) bool {
	if len(matchLabels) == 0 {
		// Empty podSelector selects ALL pods in the namespace, which includes
		// the sandbox pods.
		return true
	}
	v, ok := matchLabels[key]
	return ok && v == value
}

// detectEnforcingCNI returns the name of a recognized policy-enforcing CNI
// DaemonSet in kube-system, or "" if none is found.
func detectEnforcingCNI(ctx context.Context, cs kubernetes.Interface) (string, error) {
	ds, err := cs.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for i := range ds.Items {
		name := strings.ToLower(ds.Items[i].Name)
		for _, known := range knownEnforcingDaemonSets {
			if strings.Contains(name, known) {
				return ds.Items[i].Name, nil
			}
		}
	}
	return "", nil
}
