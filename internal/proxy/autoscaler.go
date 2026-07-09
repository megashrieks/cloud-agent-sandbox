package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/megashrieks/cloud-agent-sandbox/internal/config"
)

// sandboxSelector matches sandbox pods (warm-pool + running). The autoscaler
// counts these to decide how many mitmproxy replicas are needed.
const sandboxSelector = "app=sandbox"

// proxySelector matches mitmproxy pods (used to wait for readiness on scale-up).
const proxySelector = "app=mitmproxy"

// Autoscaler dynamically scales the mitmproxy Deployment to roughly one replica
// per SandboxesPerProxy active sandbox pods, scaling to MinReplicas (default 0)
// when there are no sandboxes. It reconciles on a timer and can also be driven
// on demand via EnsureReady when a new sandbox is about to start.
type Autoscaler struct {
	cs          kubernetes.Interface
	namespace   string
	deployment  string
	perProxy    int
	minReplicas int
	maxReplicas int
	interval    time.Duration
	scaleUpTO   time.Duration
	now         func() time.Time
}

// NewAutoscaler builds an Autoscaler from proxy configuration.
func NewAutoscaler(cs kubernetes.Interface, cfg config.ProxyConfig, namespace string) *Autoscaler {
	return &Autoscaler{
		cs:          cs,
		namespace:   namespace,
		deployment:  cfg.DeploymentName,
		perProxy:    cfg.SandboxesPerProxy,
		minReplicas: cfg.MinReplicas,
		maxReplicas: cfg.MaxReplicas,
		interval:    cfg.AutoscaleInterval,
		scaleUpTO:   cfg.ScaleUpTimeout,
		now:         time.Now,
	}
}

// Run reconciles proxy replicas on a timer until ctx is cancelled.
func (a *Autoscaler) Run(ctx context.Context) {
	if a.interval <= 0 {
		a.interval = 15 * time.Second
	}
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	if err := a.reconcile(ctx); err != nil {
		slog.WarnContext(ctx, "proxy autoscaler initial reconcile failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.reconcile(ctx); err != nil {
				slog.WarnContext(ctx, "proxy autoscaler reconcile failed", "err", err)
			}
		}
	}
}

// reconcile sets replicas to the desired count for the current sandbox count.
func (a *Autoscaler) reconcile(ctx context.Context) error {
	n, err := a.countSandboxPods(ctx)
	if err != nil {
		return err
	}
	desired := a.desiredReplicas(n)
	changed, current, err := a.scaleTo(ctx, desired)
	if err != nil {
		return err
	}
	if changed {
		slog.InfoContext(ctx, "proxy autoscaler adjusted replicas",
			"sandboxes", n, "from", current, "to", desired,
			"sandboxes_per_proxy", a.perProxy)
	}
	return nil
}

// desiredReplicas computes ceil(n / perProxy), clamped to [minReplicas, maxReplicas].
// With no sandboxes the result is minReplicas (0 by default → scale to zero).
func (a *Autoscaler) desiredReplicas(n int) int {
	if n <= 0 {
		return a.minReplicas
	}
	per := a.perProxy
	if per <= 0 {
		per = 100
	}
	r := (n + per - 1) / per
	if r < 1 {
		r = 1
	}
	if r < a.minReplicas {
		r = a.minReplicas
	}
	if a.maxReplicas > 0 && r > a.maxReplicas {
		r = a.maxReplicas
	}
	return r
}

// EnsureReady guarantees at least one mitmproxy replica is running and Ready
// before a new sandbox begins sending traffic. It only scales UP (the timed
// reconcile handles scale-down) and blocks until a proxy pod is Ready or the
// configured timeout elapses.
func (a *Autoscaler) EnsureReady(ctx context.Context) error {
	n, err := a.countSandboxPods(ctx)
	if err != nil {
		return err
	}
	// Account for the sandbox that is about to be created.
	desired := a.desiredReplicas(n + 1)
	if desired < 1 {
		desired = 1
	}
	if _, _, err := a.scaleUpTo(ctx, desired); err != nil {
		return err
	}
	return a.waitProxyReady(ctx)
}

// countSandboxPods returns the number of non-terminating sandbox pods.
func (a *Autoscaler) countSandboxPods(ctx context.Context) (int, error) {
	pods, err := a.cs.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: sandboxSelector})
	if err != nil {
		return 0, fmt.Errorf("list sandbox pods: %w", err)
	}
	count := 0
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			count++
		}
	}
	return count, nil
}

// scaleTo patches the Deployment replicas to exactly desired (up or down).
func (a *Autoscaler) scaleTo(ctx context.Context, desired int) (changed bool, current int32, err error) {
	dep, err := a.cs.AppsV1().Deployments(a.namespace).Get(ctx, a.deployment, metav1.GetOptions{})
	if err != nil {
		return false, 0, fmt.Errorf("get proxy deployment %s: %w", a.deployment, err)
	}
	current = 0
	if dep.Spec.Replicas != nil {
		current = *dep.Spec.Replicas
	}
	if int(current) == desired {
		return false, current, nil
	}
	if err := a.patchReplicas(ctx, desired); err != nil {
		return false, current, err
	}
	return true, current, nil
}

// scaleUpTo patches replicas only when the current count is below desired.
func (a *Autoscaler) scaleUpTo(ctx context.Context, desired int) (changed bool, current int32, err error) {
	dep, err := a.cs.AppsV1().Deployments(a.namespace).Get(ctx, a.deployment, metav1.GetOptions{})
	if err != nil {
		return false, 0, fmt.Errorf("get proxy deployment %s: %w", a.deployment, err)
	}
	current = 0
	if dep.Spec.Replicas != nil {
		current = *dep.Spec.Replicas
	}
	if int(current) >= desired {
		return false, current, nil
	}
	if err := a.patchReplicas(ctx, desired); err != nil {
		return false, current, err
	}
	slog.InfoContext(ctx, "proxy autoscaler scaled up on demand", "from", current, "to", desired)
	return true, current, nil
}

func (a *Autoscaler) patchReplicas(ctx context.Context, desired int) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, desired))
	if _, err := a.cs.AppsV1().Deployments(a.namespace).Patch(ctx, a.deployment,
		types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch proxy deployment replicas: %w", err)
	}
	return nil
}

// waitProxyReady blocks until at least one mitmproxy pod is Ready.
func (a *Autoscaler) waitProxyReady(ctx context.Context) error {
	timeout := a.scaleUpTO
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := a.now().Add(timeout)
	for {
		ready, err := a.proxyReadyCount(ctx)
		if err == nil && ready >= 1 {
			return nil
		}
		if a.now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timed out waiting for proxy readiness: %w", err)
			}
			return fmt.Errorf("timed out waiting for a ready mitmproxy replica after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (a *Autoscaler) proxyReadyCount(ctx context.Context) (int, error) {
	pods, err := a.cs.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: proxySelector})
	if err != nil {
		return 0, err
	}
	ready := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
				break
			}
		}
	}
	return ready, nil
}
