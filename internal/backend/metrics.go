// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// E8 runtime-image-age gauge: a periodic sync publishes
// tinycdi_runtime_image_age_seconds{family} from the template catalog — the
// age of each catalog family's newest published revision's image — so the
// stale-image signal exists without anyone querying the API.

import (
	"context"
	"log/slog"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// imageAgeSyncInterval is the refresh cadence for the image-age gauge; the
// underlying annotation changes only on the runtime-image publish train, so
// a minute is far finer than the signal's granularity.
const imageAgeSyncInterval = time.Minute

// cacheSyncer is the slice of the informer cache the sync waits on before
// its first pass (sigs.k8s.io/controller-runtime/pkg/cache.Cache).
type cacheSyncer interface {
	WaitForCacheSync(ctx context.Context) bool
}

// templateLister is the slice of the template catalog the sync reads: one
// entry per catalog family.
type templateLister interface {
	List(ctx context.Context, tenantID, runtimeFilter string) ([]provisioning.TemplateCatalogEntry, error)
}

// runImageAgeSync waits for the informer cache, then refreshes the
// image-age gauge once per interval until ctx ends. Families that leave
// the catalog have their series deleted so stale gauges never linger.
func runImageAgeSync(ctx context.Context, cache cacheSyncer, catalog templateLister,
	tenants provisioning.TenantNamespaces, m *observability.Metrics, log *slog.Logger) {
	if !cache.WaitForCacheSync(ctx) {
		return
	}
	ticker := time.NewTicker(imageAgeSyncInterval)
	defer ticker.Stop()
	known := map[string]struct{}{}
	for {
		syncImageAges(ctx, catalog, tenants, m, log, known)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncImageAges republishes every family's image age and deletes series for
// families no longer in the catalog. Malformed or absent build timestamps
// report no series — a missing value must never masquerade as a fresh or a
// stale image.
func syncImageAges(ctx context.Context, catalog templateLister,
	tenants provisioning.TenantNamespaces, m *observability.Metrics, log *slog.Logger,
	known map[string]struct{}) {
	seen := map[string]struct{}{}
	now := time.Now()
	for tenantID := range tenants {
		entries, err := catalog.List(ctx, tenantID, "")
		if err != nil {
			log.Warn("image-age sync: catalog list failed", "tenant", tenantID, "err", err)
			continue
		}
		for _, e := range entries {
			if e.ImageBuiltAt == "" {
				continue
			}
			built, err := time.Parse(time.RFC3339, e.ImageBuiltAt)
			if err != nil {
				log.Warn("image-age sync: ignoring malformed image-built-at",
					"family", e.Name, "value", e.ImageBuiltAt, "err", err)
				continue
			}
			seen[e.Name] = struct{}{}
			m.SetRuntimeImageAge(e.Name, now.Sub(built).Seconds())
		}
	}
	for family := range known {
		if _, ok := seen[family]; !ok {
			m.DeleteRuntimeImageAge(family)
		}
	}
	for family := range seen {
		known[family] = struct{}{}
	}
}
