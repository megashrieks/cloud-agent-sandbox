package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/megashrieks/sandbox-orchestrator/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	sandboxAppLabel     = "sandbox"
	sessionLabel        = "sandbox/session"
	poolLabel           = "sandbox/pool"
	workspaceVolumeName = "workspace"
	caVolumeName        = "sandbox-ca"
	caSecretKey         = "ca.crt"
)

// KubeRuntime implements Runtime using Kubernetes pods as sandboxes and PVCs
// as retained workspaces.
type KubeRuntime struct {
	cs        kubernetes.Interface
	namespace string
	sc        config.SandboxConfig
}

func NewKubeRuntime(cs kubernetes.Interface, namespace string, sc config.SandboxConfig) *KubeRuntime {
	return &KubeRuntime{cs: cs, namespace: namespace, sc: sc}
}

func (r *KubeRuntime) Create(ctx context.Context, spec SandboxSpec) (*SandboxHandle, error) {
	pvcName := pvcName(spec.SessionID)
	quantity, err := resource.ParseQuantity(r.sc.WorkspaceSize)
	if err != nil {
		return nil, fmt.Errorf("parse workspace size: %w", err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: r.namespace,
			Labels:    r.labels(spec),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: quantity},
			},
		},
	}
	if _, err := r.cs.CoreV1().PersistentVolumeClaims(r.namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create pvc %q: %w", pvcName, err)
	}

	if err := r.createCASecret(ctx, spec); err != nil {
		return nil, err
	}

	pod := r.buildPod(spec, pvcName)
	created, err := r.cs.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// Keep Create atomic: roll back the PVC and CA secret we just created so
		// a rejected/failed pod (e.g. missing RuntimeClass, quota) does not leak
		// resources on every retry.
		bg := context.Background()
		_ = r.deleteCASecret(bg, spec.SessionID)
		_ = r.cs.CoreV1().PersistentVolumeClaims(r.namespace).Delete(bg, pvcName, metav1.DeleteOptions{})
		return nil, fmt.Errorf("create pod %q: %w", pod.Name, err)
	}
	return handleFromPod(created), nil
}

func (r *KubeRuntime) WaitReady(ctx context.Context, podName string, timeout time.Duration) error {
	waitCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		handle, err := r.Get(waitCtx, podName)
		if err == nil && handle.Ready {
			return nil
		}
		// Fail fast on an unrecoverable image pull error instead of blocking
		// until the ready deadline. This surfaces a clear message when a caller
		// passes a bad image (e.g. a non-existent ref) rather than a generic
		// "context deadline exceeded".
		if err == nil {
			if reason, msg, bad := r.imagePullError(waitCtx, podName); bad {
				return fmt.Errorf("sandbox image cannot be pulled (%s): %s", reason, msg)
			}
		}

		select {
		case <-waitCtx.Done():
			if err != nil {
				return fmt.Errorf("wait for pod %q ready: %w", podName, err)
			}
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

// imagePullError reports whether the pod's sandbox container is stuck on an
// unrecoverable image pull (ErrImagePull / ImagePullBackOff) and returns the
// waiting reason and message for a clear error.
func (r *KubeRuntime) imagePullError(ctx context.Context, podName string) (reason, message string, bad bool) {
	pod, err := r.cs.CoreV1().Pods(r.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", "", false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		if w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull" {
			return w.Reason, w.Message, true
		}
	}
	return "", "", false
}

func (r *KubeRuntime) Stop(ctx context.Context, podName string) error {
	sessionID := sessionIDFromPodName(podName)
	if pod, err := r.cs.CoreV1().Pods(r.namespace).Get(ctx, podName, metav1.GetOptions{}); err == nil {
		if v := pod.Labels[sessionLabel]; v != "" {
			sessionID = v
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get pod %q: %w", podName, err)
	}

	propagation := metav1.DeletePropagationBackground
	err := r.cs.CoreV1().Pods(r.namespace).Delete(ctx, podName, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %q: %w", podName, err)
	}
	if sessionID != "" {
		if err := r.deleteCASecret(ctx, sessionID); err != nil {
			return err
		}
	}
	return nil
}

func (r *KubeRuntime) Resume(ctx context.Context, spec SandboxSpec, pvcName string) (*SandboxHandle, error) {
	if err := r.createCASecret(ctx, spec); err != nil {
		return nil, err
	}
	pod := r.buildPod(spec, pvcName)
	created, err := r.cs.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create pod %q: %w", pod.Name, err)
	}
	return handleFromPod(created), nil
}

func (r *KubeRuntime) Purge(ctx context.Context, podName, pvcName string) error {
	propagation := metav1.DeletePropagationBackground
	err := r.cs.CoreV1().Pods(r.namespace).Delete(ctx, podName, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %q: %w", podName, err)
	}

	if sessionID := sessionIDFromPodName(podName); sessionID != "" {
		if err := r.deleteCASecret(ctx, sessionID); err != nil {
			return err
		}
	}

	err = r.cs.CoreV1().PersistentVolumeClaims(r.namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pvc %q: %w", pvcName, err)
	}
	return nil
}

func (r *KubeRuntime) Get(ctx context.Context, podName string) (*SandboxHandle, error) {
	pod, err := r.cs.CoreV1().Pods(r.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %q: %w", podName, err)
	}
	return handleFromPod(pod), nil
}

// ListSandboxes returns all sandbox pods (label app=sandbox), excluding those
// already being deleted.
func (r *KubeRuntime) ListSandboxes(ctx context.Context) ([]SandboxRef, error) {
	pods, err := r.cs.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + sandboxAppLabel})
	if err != nil {
		return nil, fmt.Errorf("list sandbox pods: %w", err)
	}
	refs := make([]SandboxRef, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		sid := p.Labels[sessionLabel]
		refs = append(refs, SandboxRef{
			PodName:   p.Name,
			PVCName:   pvcName(sid),
			SessionID: sid,
			Pool:      p.Labels[poolLabel] != "",
			CreatedAt: p.CreationTimestamp.Time,
		})
	}
	return refs, nil
}

// ListWorkspaces returns all sandbox workspace PVCs (label app=sandbox).
func (r *KubeRuntime) ListWorkspaces(ctx context.Context) ([]WorkspaceRef, error) {
	pvcs, err := r.cs.CoreV1().PersistentVolumeClaims(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + sandboxAppLabel})
	if err != nil {
		return nil, fmt.Errorf("list sandbox pvcs: %w", err)
	}
	refs := make([]WorkspaceRef, 0, len(pvcs.Items))
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.DeletionTimestamp != nil {
			continue
		}
		refs = append(refs, WorkspaceRef{
			PVCName:   pvc.Name,
			PodName:   podName(pvc.Labels[sessionLabel]),
			SessionID: pvc.Labels[sessionLabel],
			Pool:      pvc.Labels[poolLabel] != "",
			CreatedAt: pvc.CreationTimestamp.Time,
		})
	}
	return refs, nil
}

func (r *KubeRuntime) buildPod(spec SandboxSpec, pvcName string) *corev1.Pod {
	labels := r.labels(spec)
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	privileged := false
	readOnlyRootFilesystem := true
	runAsUser := r.sc.RunAsUser
	// Installable dev sandbox: run as root and/or with a writable root fs so an
	// arbitrary user-chosen image can install system packages (apk/apt/dnf) into
	// itself. Containment still comes from the runtime class, dropped caps,
	// seccomp, no SA token, and the egress NetworkPolicy.
	if spec.RunAsRoot {
		runAsNonRoot = false
		runAsUser = 0
	}
	if spec.WritableRoot {
		readOnlyRootFilesystem = false
	}
	runtimeClass := spec.RuntimeClass
	if runtimeClass == "" {
		runtimeClass = r.sc.RuntimeClass
	}

	volumes := []corev1.Volume{
		{
			Name: workspaceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
			},
		},
		{
			Name: "tmp",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: quantityPtrOrNil(r.sc.TmpSizeLimit),
				},
			},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: workspaceVolumeName, MountPath: r.sc.WorkspacePath},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if len(spec.CACert) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: caVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: caSecretName(spec.SessionID)},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      caVolumeName,
			MountPath: r.sc.CACertPath,
			SubPath:   caSecretKey,
			ReadOnly:  true,
		})
	}

	container := corev1.Container{
		Name:            "sandbox",
		Image:           imageOrDefault(spec.Image, r.sc.DefaultImage),
		ImagePullPolicy: imagePullPolicy(r.sc.ImagePullPolicy),
		Command:         []string{"/bin/sh", "-c", sandboxInitScript},
		VolumeMounts:    mounts,
		Env:             append(proxyEnv(spec.ProxyEndpoint), caTrustEnv(len(spec.CACert) > 0, r.sc.CACertPath)...),
		Resources: corev1.ResourceRequirements{
			Limits:   resourceList(r.sc.CPULimit, r.sc.MemoryLimit, r.sc.EphemeralStorageLimit),
			Requests: resourceList(r.sc.CPURequest, r.sc.MemoryRequest, r.sc.EphemeralStorageRequest),
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
			Privileged:               &privileged,
			ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	automountSAToken := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(spec.SessionID),
			Namespace: r.namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &automountSAToken,
			RestartPolicy:                corev1.RestartPolicyNever,
			RuntimeClassName:             stringPtrOrNil(runtimeClass),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &runAsNonRoot,
				RunAsUser:      &runAsUser,
				RunAsGroup:     &runAsUser,
				FSGroup:        &runAsUser,
				SeccompProfile: seccompProfile(r.sc.SeccompProfilePath),
			},
			Volumes:    volumes,
			Containers: []corev1.Container{container},
		},
	}
	// Kubernetes has no portable PodSpec pids-limit field in core/v1; enforce it
	// with runtime or admission configuration outside this spec when available.
	return pod
}

func (r *KubeRuntime) labels(spec SandboxSpec) map[string]string {
	labels := make(map[string]string, len(spec.Labels)+2)
	labels["app"] = sandboxAppLabel
	labels[sessionLabel] = spec.SessionID
	for k, v := range spec.Labels {
		labels[k] = v
	}
	return labels
}

func (r *KubeRuntime) createCASecret(ctx context.Context, spec SandboxSpec) error {
	if len(spec.CACert) == 0 {
		return nil
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caSecretName(spec.SessionID),
			Namespace: r.namespace,
			Labels:    r.labels(spec),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{caSecretKey: spec.CACert},
	}
	if _, err := r.cs.CoreV1().Secrets(r.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create ca secret %q: %w", secret.Name, err)
	}
	return nil
}

func (r *KubeRuntime) deleteCASecret(ctx context.Context, sessionID string) error {
	err := r.cs.CoreV1().Secrets(r.namespace).Delete(ctx, caSecretName(sessionID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ca secret for session %q: %w", sessionID, err)
	}
	return nil
}

func handleFromPod(pod *corev1.Pod) *SandboxHandle {
	ready := pod.Status.Phase == corev1.PodRunning && len(pod.Status.ContainerStatuses) > 0
	for _, status := range pod.Status.ContainerStatuses {
		ready = ready && status.Ready
	}
	return &SandboxHandle{
		PodName: pod.Name,
		PVCName: firstPVCName(pod),
		Phase:   string(pod.Status.Phase),
		Ready:   ready,
	}
}

func firstPVCName(pod *corev1.Pod) string {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			return volume.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

func resourceList(cpu, memory, ephemeralStorage string) corev1.ResourceList {
	list := corev1.ResourceList{}
	if cpu != "" {
		list[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		list[corev1.ResourceMemory] = resource.MustParse(memory)
	}
	if ephemeralStorage != "" {
		list[corev1.ResourceEphemeralStorage] = resource.MustParse(ephemeralStorage)
	}
	return list
}

// quantityPtrOrNil parses a resource quantity string, returning nil for empty
// input (so an unset limit stays unset rather than becoming zero).
func quantityPtrOrNil(value string) *resource.Quantity {
	if value == "" {
		return nil
	}
	q := resource.MustParse(value)
	return &q
}

// seccompProfile selects a Localhost profile when a path is configured (used to
// ship a hardened profile that also blocks io_uring), otherwise RuntimeDefault.
func seccompProfile(localhostPath string) *corev1.SeccompProfile {
	if localhostPath != "" {
		return &corev1.SeccompProfile{
			Type:             corev1.SeccompProfileTypeLocalhost,
			LocalhostProfile: &localhostPath,
		}
	}
	return &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
}

func proxyEnv(endpoint string) []corev1.EnvVar {
	if endpoint == "" {
		return nil
	}
	proxy := "http://" + endpoint
	return []corev1.EnvVar{
		{Name: "HTTPS_PROXY", Value: proxy},
		{Name: "HTTP_PROXY", Value: proxy},
		{Name: "https_proxy", Value: proxy},
		{Name: "http_proxy", Value: proxy},
		{Name: "NO_PROXY", Value: "localhost,127.0.0.1"},
		// Placeholder token so the `gh` CLI operates through the proxy. gh
		// refuses to send API requests (and reports "not logged in") unless it
		// finds a token in its own env, so we give it a non-secret placeholder.
		// The proxy STRIPS this client Authorization header and injects the real
		// credential for api.github.com, so the sandbox never sees a real token
		// yet `gh auth status`/`gh pr create` work. Only set when egress is
		// forced through the proxy (endpoint != "").
		{Name: "GH_TOKEN", Value: "sandbox-proxy-injected"},
	}
}

// caBundlePath is where the sandbox init script assembles the trusted CA bundle
// (OS roots + injected proxy CA). It lives under /tmp because the root FS is
// read-only.
const caBundlePath = "/tmp/ca-bundle.crt"

// caTrustEnv returns the environment variables that point common toolchains at
// the assembled CA bundle so git/curl/node/python transparently trust the MITM
// proxy. Only emitted when a proxy CA is injected.
func caTrustEnv(hasCA bool, _ string) []corev1.EnvVar {
	if !hasCA {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "NODE_EXTRA_CA_CERTS", Value: caBundlePath},
		{Name: "REQUESTS_CA_BUNDLE", Value: caBundlePath},
		{Name: "SSL_CERT_FILE", Value: caBundlePath},
		{Name: "GIT_SSL_CAINFO", Value: caBundlePath},
		{Name: "CURL_CA_BUNDLE", Value: caBundlePath},
	}
}

// sandboxInitScript builds the CA trust bundle (if a proxy CA was mounted) and
// then keeps the container alive so the orchestrator can exec into it. Running
// this as the pod command (rather than relying on the image entrypoint) makes
// CA trust work for ANY user-supplied image, not just the curated default.
const sandboxInitScript = `
if [ -f /etc/sandbox/ca.crt ]; then
  if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
    cat /etc/ssl/certs/ca-certificates.crt /etc/sandbox/ca.crt > /tmp/ca-bundle.crt 2>/dev/null || cp /etc/sandbox/ca.crt /tmp/ca-bundle.crt
  else
    cp /etc/sandbox/ca.crt /tmp/ca-bundle.crt
  fi
  git config --global http.sslCAInfo /tmp/ca-bundle.crt 2>/dev/null || true
fi
exec sleep infinity
`

func imageOrDefault(image, fallback string) string {
	if image != "" {
		return image
	}
	return fallback
}

func imagePullPolicy(policy string) corev1.PullPolicy {
	switch corev1.PullPolicy(policy) {
	case corev1.PullAlways, corev1.PullNever, corev1.PullIfNotPresent:
		return corev1.PullPolicy(policy)
	default:
		return corev1.PullIfNotPresent
	}
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func podName(sessionID string) string {
	return "sbx-" + sessionID
}

func pvcName(sessionID string) string {
	return "pvc-" + sessionID
}

func caSecretName(sessionID string) string {
	return "ca-" + sessionID
}

func sessionIDFromPodName(podName string) string {
	return strings.TrimPrefix(podName, "sbx-")
}
