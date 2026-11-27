package api

import (
	"context"
	"log"
	"time"

	"deploy-api/internal/discovery"
)

const argoNotifyTimeout = 30 * time.Second

// ArgoCDClient is implemented by *argocd.Client.
type ArgoCDClient interface {
	Refresh(ctx context.Context, name string) error
	Sync(ctx context.Context, name string) error
}

// ArgoCDAppLookup is implemented by *discovery.Registry.
type ArgoCDAppLookup interface {
	Lookup(appName, env string) (discovery.DiscoveredApp, bool)
}

// SetArgoCD enables the post-push refresh. Either argument nil keeps it disabled.
func (h *DeploymentsHandler) SetArgoCD(client ArgoCDClient, apps ArgoCDAppLookup) {
	h.argoClient = client
	h.argoApps = apps
}

// notifyArgoCD is best-effort: it runs in the background and only logs failures.
func (h *DeploymentsHandler) notifyArgoCD(appName, environment string) {
	if h.argoClient == nil || h.argoApps == nil {
		return
	}
	app, ok := h.argoApps.Lookup(appName, environment)
	if !ok || app.ArgoName == "" {
		log.Printf("[ArgoCD] No discovered Application for %s in %s, skipping refresh", appName, environment)
		return
	}
	client := h.argoClient
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), argoNotifyTimeout)
		defer cancel()

		if err := client.Refresh(ctx, app.ArgoName); err != nil {
			log.Printf("[ArgoCD] Refresh of %s (%s/%s) failed: %v", app.ArgoName, appName, environment, err)
		} else {
			log.Printf("[ArgoCD] Refreshed %s (%s/%s)", app.ArgoName, appName, environment)
		}

		if app.AutoSync {
			return
		}
		if err := client.Sync(ctx, app.ArgoName); err != nil {
			log.Printf("[ArgoCD] Sync of %s (%s/%s) failed: %v", app.ArgoName, appName, environment, err)
			return
		}
		log.Printf("[ArgoCD] Sync triggered for %s (%s/%s)", app.ArgoName, appName, environment)
	}()
}
