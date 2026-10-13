// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const schemaSnapshot = "testdata/schema.snapshot.sql"

// TestBaseline_SchemaMatchesSnapshot: the schema the migrator produces on an
// empty database equals the committed snapshot. A deliberate schema change
// regenerates it with TCDI_UPDATE_SNAPSHOT=1.
func TestBaseline_SchemaMatchesSnapshot(t *testing.T) {
	_, name := newNamedDB(t)
	got := dumpSchema(t, name)
	if os.Getenv("TCDI_UPDATE_SNAPSHOT") == "1" {
		if err := os.WriteFile(schemaSnapshot, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", schemaSnapshot)
		return
	}
	want, err := os.ReadFile(schemaSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("schema drifted from %s; regenerate with TCDI_UPDATE_SNAPSHOT=1\n%s",
			schemaSnapshot, diffLines(string(want), got))
	}
}

// TestBaseline_MigrateIsIdempotent: a second Migrate on a migrated database
// applies nothing and the version table holds only the baseline.
func TestBaseline_MigrateIsIdempotent(t *testing.T) {
	db := newDB(t) // already migrated once
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n, version int
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*), max(version) FROM schema_migrations`).Scan(&n, &version); err != nil {
		t.Fatal(err)
	}
	if n != 1 || version != 1 {
		t.Fatalf("schema_migrations rows = %d (max version %d), want 1 row at version 1", n, version)
	}
}

// TestBaseline_PortalSessionDigestNotNull: every lease and ticket is bound to
// a portal session — the columns admit no NULL.
func TestBaseline_PortalSessionDigestNotNull(t *testing.T) {
	db := newDB(t)
	for _, tbl := range []string{"connection_lease", "launch_ticket"} {
		var nullable string
		err := db.Pool().QueryRow(context.Background(),
			`SELECT is_nullable FROM information_schema.columns
			 WHERE table_name = $1 AND column_name = 'portal_session_digest'`, tbl).Scan(&nullable)
		if err != nil {
			t.Fatal(err)
		}
		if nullable != "NO" {
			t.Fatalf("%s.portal_session_digest nullable=%s", tbl, nullable)
		}
	}
}

// dumpSchema runs pg_dump --schema-only inside the test container and
// normalizes it: comments, SET/set_config lines and psql meta-commands
// (pg_dump 18 emits a per-run random \restrict key) are dropped, and runs of
// blank lines collapse to one.
func dumpSchema(t *testing.T, dbName string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", pgContainer,
		"pg_dump", "-U", "postgres", "--schema-only", "--no-owner", "--no-privileges", dbName).Output()
	if err != nil {
		t.Fatalf("pg_dump: %v", err)
	}
	var b strings.Builder
	blank := true // drop leading blank lines
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "--"),
			strings.HasPrefix(line, "SET "),
			strings.HasPrefix(line, "SELECT pg_catalog.set_config"),
			strings.HasPrefix(line, `\`):
			continue
		case strings.TrimSpace(line) == "":
			if blank {
				continue
			}
			blank = true
			b.WriteString("\n")
		default:
			blank = false
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// diffLines lists the lines only in want ("-") and only in got ("+").
func diffLines(want, got string) string {
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			m[l]++
		}
		return m
	}
	w, g := count(want), count(got)
	var b strings.Builder
	for _, l := range strings.Split(want, "\n") {
		if g[l] > 0 {
			g[l]--
			continue
		}
		b.WriteString("- " + l + "\n")
	}
	g = count(got)
	for _, l := range strings.Split(got, "\n") {
		if w[l] > 0 {
			w[l]--
			continue
		}
		b.WriteString("+ " + l + "\n")
	}
	return b.String()
}
