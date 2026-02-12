package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// restartDockerContainer restarts a Docker container via the Docker Engine API
// over a Unix socket. This requires the Docker socket to be mounted into the
// sidecar container (e.g., -v /var/run/docker.sock:/var/run/docker.sock).
func restartDockerContainer(socketPath, container string) error {
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.DialTimeout("unix", socketPath, 10*time.Second)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second, // container stop can take a while
	}

	// POST /containers/{id}/restart with a 30-second timeout for the stop phase
	url := fmt.Sprintf("http://localhost/containers/%s/restart?t=30", container)
	resp, err := client.Post(url, "", nil)
	if err != nil {
		return fmt.Errorf("docker restart request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		log.Printf("[sidecar] Docker container %s restarted successfully", container)
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("docker restart failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// restartKubePod deletes a Kubernetes pod, relying on the controller (Deployment,
// StatefulSet, etc.) to recreate it. Uses the in-cluster service account credentials
// mounted at /var/run/secrets/kubernetes.io/serviceaccount/.
func restartKubePod(namespace, pod string) error {
	// Read service account credentials
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return fmt.Errorf("failed to read service account token: %w (is the sidecar running in a K8s pod?)", err)
	}

	// Auto-detect namespace if not specified
	if namespace == "" {
		nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			return fmt.Errorf("failed to detect namespace: %w", err)
		}
		namespace = strings.TrimSpace(string(nsBytes))
	}

	// Load the cluster CA certificate
	caCert, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		log.Printf("[sidecar] Warning: could not load CA cert, using insecure TLS: %v", err)
	}

	var transport *http.Transport
	if caCert != nil {
		pool, _ := newCertPool(caCert)
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		}
	} else {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	// DELETE /api/v1/namespaces/{ns}/pods/{pod}
	url := fmt.Sprintf("https://kubernetes.default.svc/api/v1/namespaces/%s/pods/%s", namespace, pod)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("K8s API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
		log.Printf("[sidecar] K8s pod %s/%s deleted (will be recreated by controller)", namespace, pod)
		return nil
	}

	body, _ := io.ReadAll(resp.Body)

	// Parse K8s API error for a cleaner message
	var apiErr struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
		return fmt.Errorf("K8s pod delete failed (HTTP %d): %s", resp.StatusCode, apiErr.Message)
	}

	return fmt.Errorf("K8s pod delete failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// restartResult holds the outcome of a restart attempt.
type restartResult struct {
	Attempted bool   `json:"attempted"`
	Success   bool   `json:"success"`
	Method    string `json:"method,omitempty"` // "docker" or "kubernetes"
	Error     string `json:"error,omitempty"`
}

// tryRestart attempts to restart the target app using whichever method is configured.
// Returns a restartResult describing what happened. If no restart method is configured,
// Attempted will be false.
func tryRestart(cfg *config) restartResult {
	if cfg.DockerContainer != "" {
		err := restartDockerContainer(cfg.DockerHost, cfg.DockerContainer)
		if err != nil {
			return restartResult{Attempted: true, Success: false, Method: "docker", Error: err.Error()}
		}
		return restartResult{Attempted: true, Success: true, Method: "docker"}
	}

	if cfg.KubePod != "" {
		err := restartKubePod(cfg.KubeNamespace, cfg.KubePod)
		if err != nil {
			return restartResult{Attempted: true, Success: false, Method: "kubernetes", Error: err.Error()}
		}
		return restartResult{Attempted: true, Success: true, Method: "kubernetes"}
	}

	return restartResult{Attempted: false}
}
