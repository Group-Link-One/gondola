package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"deploy-api/internal/discovery"
)

type fakeArgoClient struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeArgoClient) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeArgoClient) Refresh(_ context.Context, name string) error {
	f.record("refresh:" + name)
	return nil
}

func (f *fakeArgoClient) Sync(_ context.Context, name string) error {
	f.record("sync:" + name)
	return nil
}

type fakeArgoApps map[string]discovery.DiscoveredApp

func (f fakeArgoApps) Lookup(appName, env string) (discovery.DiscoveredApp, bool) {
	a, ok := f[appName+"|"+env]
	return a, ok
}

func waitForCalls(t *testing.T, f *fakeArgoClient, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.calls) >= n {
			out := append([]string(nil), f.calls...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d calls", n)
	return nil
}

func TestNotifyArgoCD(t *testing.T) {
	apps := fakeArgoApps{
		"api|prod":    {Name: "api", ArgoName: "api-prod", AutoSync: true},
		"worker|prod": {Name: "worker", ArgoName: "worker-prod", AutoSync: false},
	}

	t.Run("autosync refreshes only", func(t *testing.T) {
		c := &fakeArgoClient{}
		h := &DeploymentsHandler{}
		h.SetArgoCD(c, apps)
		h.notifyArgoCD("api", "prod")
		calls := waitForCalls(t, c, 1)
		time.Sleep(20 * time.Millisecond)
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.calls) != 1 || calls[0] != "refresh:api-prod" {
			t.Fatalf("calls = %v", c.calls)
		}
	})

	t.Run("manual sync refreshes then syncs", func(t *testing.T) {
		c := &fakeArgoClient{}
		h := &DeploymentsHandler{}
		h.SetArgoCD(c, apps)
		h.notifyArgoCD("worker", "prod")
		calls := waitForCalls(t, c, 2)
		if calls[0] != "refresh:worker-prod" || calls[1] != "sync:worker-prod" {
			t.Fatalf("calls = %v", calls)
		}
	})

	t.Run("unknown app is skipped", func(t *testing.T) {
		c := &fakeArgoClient{}
		h := &DeploymentsHandler{}
		h.SetArgoCD(c, apps)
		h.notifyArgoCD("missing", "prod")
		time.Sleep(20 * time.Millisecond)
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.calls) != 0 {
			t.Fatalf("calls = %v", c.calls)
		}
	})

	t.Run("disabled is a no-op", func(t *testing.T) {
		h := &DeploymentsHandler{}
		h.notifyArgoCD("api", "prod")
	})
}
