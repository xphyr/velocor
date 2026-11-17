package csi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// ApplyStorageProfiles merges spec into KubeVirt CDI's StorageProfile for each StorageClass.
//
// CDI creates a StorageProfile for every StorageClass and fills in what to ask for (access
// mode, volume mode, clone strategy) only for drivers it knows, which Velocor isn't; without
// one, every DataVolume has to spell those out. CDI owns the object, so it's patched rather
// than created, after waiting for CDI to make it. It runs as a Helm hook on every install and
// upgrade, which also restores a profile that CDI reset because its class was recreated.
func ApplyStorageProfiles(ctx context.Context, classes []string, spec json.RawMessage) error {
	k, err := inClusterClient()
	if err != nil {
		return err
	}
	return applyStorageProfiles(ctx, k, classes, spec, 3*time.Second)
}

func applyStorageProfiles(ctx context.Context, k *kubeClient, classes []string, spec json.RawMessage, poll time.Duration) error {
	body, err := json.Marshal(map[string]json.RawMessage{"spec": spec})
	if err != nil {
		return err
	}
	for _, class := range classes {
		path := "/apis/cdi.kubevirt.io/v1beta1/storageprofiles/" + class
		for {
			code, err := k.do(ctx, http.MethodGet, path, "", nil)
			if err == nil && code == http.StatusOK {
				break
			}
			if code != http.StatusNotFound && err == nil {
				return fmt.Errorf("get StorageProfile %s: HTTP %d", class, code)
			}
			log.Printf("waiting for CDI to create the StorageProfile for %s", class)
			select {
			case <-ctx.Done():
				return fmt.Errorf("StorageProfile %s never appeared (is CDI installed, and does the StorageClass exist?): %w", class, ctx.Err())
			case <-time.After(poll):
			}
		}
		if code, err := k.do(ctx, http.MethodPatch, path, "application/merge-patch+json", body); err != nil || code != http.StatusOK {
			return fmt.Errorf("patch StorageProfile %s: HTTP %d %v", class, code, err)
		}
		log.Printf("StorageProfile %s: applied %s", class, spec)
	}
	return nil
}

type kubeClient struct {
	base, token string
	http        *http.Client
}

func inClusterClient() (*kubeClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	token, err := os.ReadFile(serviceAccountDir + "/token")
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("service account ca.crt holds no certificate")
	}
	return &kubeClient{
		base:  "https://" + net.JoinHostPort(host, port),
		token: string(bytes.TrimSpace(token)),
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}},
	}, nil
}

func (k *kubeClient) do(ctx context.Context, method, path, contentType string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("%s", bytes.TrimSpace(msg))
	}
	return resp.StatusCode, nil
}
