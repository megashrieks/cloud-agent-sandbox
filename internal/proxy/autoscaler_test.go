package proxy

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/megashrieks/sandbox-orchestrator/internal/config"
)

func newTestAutoscaler(cs *fake.Clientset, per, minR, maxR int) *Autoscaler {
	return NewAutoscaler(cs, config.ProxyConfig{
		DeploymentName:    "mitmproxy",
		SandboxesPerProxy: per,
		MinReplicas:       minR,
		MaxReplicas:       maxR,
		AutoscaleInterval: time.Second,
		ScaleUpTimeout:    time.Second,
	}, "sandboxes")
}

func TestDesiredReplicas(t *testing.T) {
	a := newTestAutoscaler(fake.NewSimpleClientset(), 100, 0, 10)

	cases := []struct {
		n    int
		want int
	}{
		{0, 0},    // no sandboxes -> scale to zero
		{1, 1},    // any sandbox -> at least 1
		{100, 1},  // exactly one group
		{101, 2},  // spills into a second proxy
		{250, 3},  // ceil(250/100)
		{5000, 10}, // clamped to MaxReplicas
	}
	for _, c := range cases {
		if got := a.desiredReplicas(c.n); got != c.want {
			t.Errorf("desiredReplicas(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestDesiredReplicasHonorsMinReplicas(t *testing.T) {
	a := newTestAutoscaler(fake.NewSimpleClientset(), 100, 2, 10)
	if got := a.desiredReplicas(0); got != 2 {
		t.Errorf("desiredReplicas(0) with min=2 = %d, want 2", got)
	}
	if got := a.desiredReplicas(50); got != 2 {
		t.Errorf("desiredReplicas(50) with min=2 = %d, want 2", got)
	}
}

func TestReconcileScalesDeploymentFromSandboxCount(t *testing.T) {
	ctx := context.Background()
	replicas := int32(0)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "mitmproxy", Namespace: "sandboxes"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	// 150 sandbox pods -> ceil(150/100) = 2 replicas.
	objs := []k8sruntime.Object{dep}
	for i := 0; i < 150; i++ {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
				Namespace: "sandboxes",
				Labels:    map[string]string{"app": "sandbox"},
			},
		})
	}
	cs := fake.NewSimpleClientset(objs...)
	a := newTestAutoscaler(cs, 100, 0, 10)

	if err := a.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := cs.AppsV1().Deployments("sandboxes").Get(ctx, "mitmproxy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Fatalf("replicas = %v, want 2", got.Spec.Replicas)
	}
}

func TestReconcileScalesToZeroWhenNoSandboxes(t *testing.T) {
	ctx := context.Background()
	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "mitmproxy", Namespace: "sandboxes"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	cs := fake.NewSimpleClientset(dep)
	a := newTestAutoscaler(cs, 100, 0, 10)

	if err := a.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := cs.AppsV1().Deployments("sandboxes").Get(ctx, "mitmproxy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Fatalf("replicas = %v, want 0", got.Spec.Replicas)
	}
}
