// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestMigrate_ConcurrentReplicas (R1a): replicas start together, so Migrate
// must serialise on a database-wide lock — every caller returns nil and each
// migration version is recorded exactly once.
func TestMigrate_ConcurrentReplicas(t *testing.T) {
	db := newBareDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const replicas = 8
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	start := make(chan struct{})
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = db.Migrate(ctx)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: Migrate = %v, want nil", i, err)
		}
	}

	rows, err := db.Pool().Query(ctx, `SELECT version, count(*) FROM schema_migrations GROUP BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var v, c int
		if err := rows.Scan(&v, &c); err != nil {
			t.Fatal(err)
		}
		if c != 1 {
			t.Errorf("migration %d recorded %d times, want 1", v, c)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no migrations recorded")
	}
}
