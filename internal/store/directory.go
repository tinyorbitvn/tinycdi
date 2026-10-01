// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
)

// DirectoryEntry is the display identity last seen at login for one owner
// reference (issuer|sub) in a tenant. Display data only — never used for
// authorization decisions.
type DirectoryEntry struct {
	OwnerRef    string
	Subject     string
	DisplayName string
	Email       string
}

// maxDirectoryLookup bounds one Lookup call (a page of owners).
const maxDirectoryLookup = 500

// PrincipalDirectory persists DirectoryEntry rows (migration 012).
type PrincipalDirectory struct{ db *DB }

// NewPrincipalDirectory wraps db.
func NewPrincipalDirectory(db *DB) *PrincipalDirectory { return &PrincipalDirectory{db: db} }

// Remember upserts the caller's display identity after a successful login.
func (d *PrincipalDirectory) Remember(ctx context.Context, tenantID string, e DirectoryEntry) error {
	_, err := d.db.Pool().Exec(ctx, `
		INSERT INTO principal_directory (tenant_id, owner_ref, subject, display_name, email, last_login_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (tenant_id, owner_ref) DO UPDATE SET
			subject = EXCLUDED.subject, display_name = EXCLUDED.display_name,
			email = EXCLUDED.email, last_login_at = now()`,
		tenantID, e.OwnerRef, e.Subject, e.DisplayName, e.Email)
	if err != nil {
		return fmt.Errorf("directory remember: %w", err)
	}
	return nil
}

// Lookup returns the known entries for ownerRefs within tenantID, keyed by
// owner reference. Unknown owners are simply absent.
func (d *PrincipalDirectory) Lookup(ctx context.Context, tenantID string, ownerRefs []string) (map[string]DirectoryEntry, error) {
	out := map[string]DirectoryEntry{}
	if len(ownerRefs) == 0 {
		return out, nil
	}
	if len(ownerRefs) > maxDirectoryLookup {
		ownerRefs = ownerRefs[:maxDirectoryLookup]
	}
	rows, err := d.db.Pool().Query(ctx, `
		SELECT owner_ref, subject, display_name, email
		FROM principal_directory
		WHERE tenant_id = $1 AND owner_ref = ANY($2)`, tenantID, ownerRefs)
	if err != nil {
		return nil, fmt.Errorf("directory lookup: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e DirectoryEntry
		if err := rows.Scan(&e.OwnerRef, &e.Subject, &e.DisplayName, &e.Email); err != nil {
			return nil, fmt.Errorf("directory lookup: %w", err)
		}
		out[e.OwnerRef] = e
	}
	return out, rows.Err()
}
