package csi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The profile is patched (never created: CDI owns it) once CDI has made it.
func TestApplyStorageProfilesWaitsForCDI(t *testing.T) {
	var mu sync.Mutex
	gets, patched := 0, ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/apis/cdi.kubevirt.io/v1beta1/storageprofiles/velocor-nvme-tcp" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodGet:
			gets++
			if gets < 3 { // CDI hasn't made it yet
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		case http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				http.Error(w, "content type", http.StatusUnsupportedMediaType)
				return
			}
			b, _ := io.ReadAll(r.Body)
			patched = string(b)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	defer ts.Close()
	k := &kubeClient{base: ts.URL, token: "tok", http: ts.Client()}
	spec := json.RawMessage(`{"claimPropertySets":[{"accessModes":["ReadWriteMany"],"volumeMode":"Block"}],"cloneStrategy":"csi-clone"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := applyStorageProfiles(ctx, k, []string{"velocor-nvme-tcp"}, spec, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(patched), &got); err != nil || string(got["spec"]) != string(spec) {
		t.Fatalf("patched %q, want spec %s", patched, spec)
	}
	if gets < 3 {
		t.Fatalf("patched after %d GETs; it should wait for CDI to create the profile", gets)
	}
}
