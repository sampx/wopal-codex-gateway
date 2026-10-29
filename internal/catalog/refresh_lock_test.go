package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCatalogRefreshLockSerializesAndHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog-refresh.lock")
	first, err := lockCatalogRefresh(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := lockCatalogRefresh(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want deadline exceeded", err)
	}
	first.Unlock()

	second, err := lockCatalogRefresh(context.Background(), path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second.Unlock()
}
