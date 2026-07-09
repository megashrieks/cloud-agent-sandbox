package runtime

import (
	"context"
	"testing"

	"github.com/megashrieks/cloud-agent-sandbox/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubeRuntimeCreateCreatesHardenedPodAndPVC(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	spec := SandboxSpec{
		SessionID:     "session-1",
		Image:         "alpine:latest",
		RuntimeClass:  "kata",
		ProxyEndpoint: "proxy:8080",
		CACert:        []byte("cert"),
		Labels:        map[string]string{"pool": "warm"},
	}

	handle, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if handle.PodName != "sbx-session-1" || handle.PVCName != "pvc-session-1" {
		t.Fatalf("unexpected handle: %#v", handle)
	}

	pvc, err := rt.cs.CoreV1().PersistentVolumeClaims(rt.namespace).Get(ctx, "pvc-session-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if got := pvc.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteOnce {
		t.Fatalf("access modes = %#v, want RWO", got)
	}
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "1Gi" {
		t.Fatalf("storage request = %s, want 1Gi", got)
	}

	pod, err := rt.cs.CoreV1().Pods(rt.namespace).Get(ctx, "sbx-session-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels["app"] != "sandbox" || pod.Labels[sessionLabel] != "session-1" || pod.Labels["pool"] != "warm" {
		t.Fatalf("labels = %#v", pod.Labels)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "kata" {
		t.Fatalf("RuntimeClassName = %#v, want kata", pod.Spec.RuntimeClassName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("AutomountServiceAccountToken = %#v, want false", pod.Spec.AutomountServiceAccountToken)
	}
	if pod.Spec.SecurityContext == nil {
		t.Fatal("pod security context is nil")
	}
	if pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("RunAsNonRoot = %#v, want true", pod.Spec.SecurityContext.RunAsNonRoot)
	}
	if pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != 1000 {
		t.Fatalf("RunAsUser = %#v, want 1000", pod.Spec.SecurityContext.RunAsUser)
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("SeccompProfile = %#v, want RuntimeDefault", pod.Spec.SecurityContext.SeccompProfile)
	}

	container := pod.Spec.Containers[0]
	if container.SecurityContext == nil {
		t.Fatal("container security context is nil")
	}
	if container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("ReadOnlyRootFilesystem = %#v, want true", container.SecurityContext.ReadOnlyRootFilesystem)
	}
	if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("AllowPrivilegeEscalation = %#v, want false", container.SecurityContext.AllowPrivilegeEscalation)
	}
	if container.SecurityContext.Privileged == nil || *container.SecurityContext.Privileged {
		t.Fatalf("Privileged = %#v, want false", container.SecurityContext.Privileged)
	}
	if drops := container.SecurityContext.Capabilities.Drop; len(drops) != 1 || drops[0] != corev1.Capability("ALL") {
		t.Fatalf("dropped caps = %#v, want ALL", drops)
	}
	if got := container.Resources.Limits.Cpu().String(); got != "500m" {
		t.Fatalf("cpu limit = %s, want 500m", got)
	}
	if got := container.Resources.Limits.Memory().String(); got != "256Mi" {
		t.Fatalf("memory limit = %s, want 256Mi", got)
	}
	if !hasMount(container.VolumeMounts, "tmp", "/tmp", false) {
		t.Fatalf("missing /tmp emptyDir mount: %#v", container.VolumeMounts)
	}
	if !hasMount(container.VolumeMounts, caVolumeName, "/etc/sandbox/ca.crt", true) {
		t.Fatalf("missing CA cert mount: %#v", container.VolumeMounts)
	}
	if got := envMap(container.Env)["HTTPS_PROXY"]; got != "http://proxy:8080" {
		t.Fatalf("HTTPS_PROXY = %q, want proxy URL", got)
	}

	if _, err := rt.cs.CoreV1().Secrets(rt.namespace).Get(ctx, "ca-session-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("get ca secret: %v", err)
	}
}

func TestKubeRuntimeStopDeletesPodAndKeepsPVC(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	spec := SandboxSpec{SessionID: "session-2", Image: "alpine:latest", CACert: []byte("cert")}
	if _, err := rt.Create(ctx, spec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := rt.Stop(ctx, "sbx-session-2"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if _, err := rt.cs.CoreV1().Pods(rt.namespace).Get(ctx, "sbx-session-2", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod get err = %v, want NotFound", err)
	}
	if _, err := rt.cs.CoreV1().PersistentVolumeClaims(rt.namespace).Get(ctx, "pvc-session-2", metav1.GetOptions{}); err != nil {
		t.Fatalf("pvc should remain, got err = %v", err)
	}
	if _, err := rt.cs.CoreV1().Secrets(rt.namespace).Get(ctx, "ca-session-2", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("secret get err = %v, want NotFound", err)
	}
}

func TestKubeRuntimePurgeDeletesPodAndPVC(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime()
	spec := SandboxSpec{SessionID: "session-3", Image: "alpine:latest"}
	if _, err := rt.Create(ctx, spec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := rt.Purge(ctx, "sbx-session-3", "pvc-session-3"); err != nil {
		t.Fatalf("Purge() error = %v", err)
	}

	if _, err := rt.cs.CoreV1().Pods(rt.namespace).Get(ctx, "sbx-session-3", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod get err = %v, want NotFound", err)
	}
	if _, err := rt.cs.CoreV1().PersistentVolumeClaims(rt.namespace).Get(ctx, "pvc-session-3", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pvc get err = %v, want NotFound", err)
	}
}

func newTestRuntime() *KubeRuntime {
	return NewKubeRuntime(fake.NewSimpleClientset(), "sandboxes", config.SandboxConfig{
		DefaultImage:  "alpine:latest",
		RuntimeClass:  "gvisor",
		WorkspacePath: "/workspace",
		WorkspaceSize: "1Gi",
		CPULimit:      "500m",
		MemoryLimit:   "256Mi",
		RunAsUser:     1000,
		CACertPath:    "/etc/sandbox/ca.crt",
	})
}

func hasMount(mounts []corev1.VolumeMount, name, path string, readOnly bool) bool {
	for _, mount := range mounts {
		if mount.Name == name && mount.MountPath == path && mount.ReadOnly == readOnly {
			return true
		}
	}
	return false
}

func envMap(env []corev1.EnvVar) map[string]string {
	values := map[string]string{}
	for _, item := range env {
		values[item.Name] = item.Value
	}
	return values
}
