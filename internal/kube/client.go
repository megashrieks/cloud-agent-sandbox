// Package kube builds shared Kubernetes client configuration used by the
// runtime and exec layers. Centralizing this avoids duplicate client wiring.
package kube

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client bundles the typed clientset with the REST config. The REST config is
// required by the exec layer to build SPDY/WebSocket streams for pods/exec.
type Client struct {
	Clientset kubernetes.Interface
	Config    *rest.Config
}

// New builds a Client. If kubeconfig is empty, in-cluster config is used;
// otherwise the given kubeconfig file path is loaded.
func New(kubeconfig string) (*Client, error) {
	cfg, err := buildConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	// The default client-go limiter (5 QPS / 10 burst) is too low for the warm
	// pool + reaper + WaitReady polling, causing "rate limiter Wait ... context
	// deadline exceeded" under load. Raise it for a control-plane component.
	if cfg.QPS == 0 {
		cfg.QPS = 50
	}
	if cfg.Burst == 0 {
		cfg.Burst = 100
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return &Client{Clientset: cs, Config: cfg}, nil
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		return cfg, nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %q: %w", kubeconfig, err)
	}
	return cfg, nil
}
