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
