// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// FileTransferDeps is what minting needs beyond the workspace backend.
type FileTransferDeps interface {
	ResolveSessionPolicy(ctx context.Context, tenantID string, tpl provisioning.TemplateCatalogEntry) (provisioning.SessionPolicy, error)
	InsertFileTransfer(ctx context.Context, t store.FileTransfer) error
}

type createFileTransferRequest struct {
	Direction string `json:"direction"`
	Path      string `json:"path"`
	Bytes     *int64 `json:"bytes"`
}

type fileTransferView struct {
	ID            string    `json:"id"`
	Direction     string    `json:"direction"`
	Path          string    `json:"path"`
	DeclaredBytes int64     `json:"declaredBytes"`
	MaxBytes      int64     `json:"maxBytes"`
	ExpiresAt     time.Time `json:"expiresAt"`
	ContentURL    string    `json:"contentUrl"`
}

// FileTransferHandler mints single-use transfer grants (ADR 0010 T11).
type FileTransferHandler struct {
	backend workspaceBackend
	deps    FileTransferDeps
	status  StatusView
	domain  sessionhost.Domain
	audit   observability.AuditSink
	now     func() time.Time
	maxBody int64
}

func NewFileTransferHandler(be workspaceBackend, deps FileTransferDeps, status StatusView, domain sessionhost.Domain) *FileTransferHandler {
	return &FileTransferHandler{backend: be, deps: deps, status: status, domain: domain, now: time.Now, maxBody: 4 << 10}
}

func (h *FileTransferHandler) WithAuditSink(s observability.AuditSink) *FileTransferHandler {
	h.audit = s
	return h
}

// MountFileTransferRoutes mounts the mint route behind the session-keyed
// rate limit sign-out-everywhere uses.
func MountFileTransferRoutes(mux RouteMux, authn *Authenticator, h *FileTransferHandler, limit func(http.Handler) http.Handler) {
	mux.Handle(routeFileTransferCreate, authn.RequireAuth(limit(
		audited(h.audit, routeFileTransferCreate, authn.RequireCSRF(http.HandlerFunc(h.Create))))))
}

var transferIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTransferID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "xf_" + strings.ToLower(transferIDEncoding.EncodeToString(b[:])), nil
}

// validTransferName accepts one file name inside the flat transfer root:
// 1..255 bytes of valid UTF-8, no path separator, NUL or control
// character, no ";", "%" or "..", and not ".". The in-pod helper re-checks the same
// rule and opens the name through an os.Root.
func validTransferName(name string) string {
	switch {
	case name == "" || len(name) > 255:
		return "path must be 1..255 bytes"
	case !utf8.ValidString(name):
		return "path must be valid UTF-8"
	case name == "." || name == "..":
		return "path must name a file"
	case strings.ContainsAny(name, "/\\"):
		return "path must be a file name without directories"
	case strings.ContainsAny(name, ";%") || strings.Contains(name, ".."):
		// The gateway leg rebuilds /xfer/files/<name> through
		// cleanedProxyPath, which refuses ";", "..", and %-encodings of
		// banned bytes. Refuse them here so a claim is never spent on a
		// name the leg will 404.
		return "path must not contain ';', '%' or '..'"
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) {
			return "path must not contain control characters"
		}
	}
	return ""
}

func (h *FileTransferHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	if !workspaceIDPattern.MatchString(id) {
		writeError(w, r, CodeInvalidRequest, "bad workspace id")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	var req createFileTransferRequest
	if decodeJSON(body, &req) != nil || (req.Direction != "upload" && req.Direction != "download") {
		writeError(w, r, CodeInvalidRequest, `direction must be "upload" or "download"`)
		return
	}
	if req.Direction == "download" {
		auditSetAction(ctx, auditActionFileTransferDownload)
	}
	auditSetDetail(ctx, "phase", "mint")
	auditSetDetail(ctx, "direction", req.Direction)
	if msg := validTransferName(req.Path); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	auditSetDetail(ctx, "path", req.Path)
	// Owner only — a tenant admin acts on others' workspaces but never
	// moves their files: the scope is always the caller.
	rec, err := h.backend.GetWorkspace(ctx, p.TenantID, p.Owner(), id)
	if errors.Is(err, provisioning.ErrWorkspaceNotFound) {
		writeError(w, r, CodeNotFound, "workspace not found")
		return
	}
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	obs, err := h.status.WorkspaceStatus(ctx, p.TenantID, id)
	if err != nil || !obs.Fresh || !obs.Found || obs.Phase != "Ready" || obs.RuntimeUID == "" {
		writeError(w, r, CodeInvalidState, "the workspace must be running to transfer files")
		return
	}
	pol, err := h.deps.ResolveSessionPolicy(ctx, p.TenantID, provisioning.TemplateCatalogEntry{
		ClipboardPolicy: rec.Template.ClipboardPolicy, FileTransferPolicy: rec.Template.FileTransferPolicy,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	auditSetDetail(ctx, "policy", string(pol.FileTransfer))
	if !provisioning.DirectionAllows(pol.FileTransfer, req.Direction) {
		e := NewError(CodeForbidden, "file "+req.Direction+" is not allowed for this workspace")
		e.Details = &ErrorDetails{Reason: ReasonTransferDenied}
		WriteError(w, r, e)
		return
	}
	var declared int64
	if req.Direction == "upload" {
		if req.Bytes == nil || *req.Bytes < 0 || *req.Bytes > pol.MaxFileBytes {
			writeError(w, r, CodeInvalidRequest, "bytes must be 0.."+strconv.FormatInt(pol.MaxFileBytes, 10)+" for an upload")
			return
		}
		declared = *req.Bytes
		auditSetDetail(ctx, "bytes", strconv.FormatInt(declared, 10))
	}
	tid, err := newTransferID()
	if err != nil {
		writeError(w, r, CodeInternal, "could not mint a transfer")
		return
	}
	origin, err := h.domain.Origin(id)
	if err != nil {
		writeError(w, r, CodeInternal, "session origin unavailable")
		return
	}
	expires := h.now().Add(store.FileTransferTTL).UTC()
	if err := h.deps.InsertFileTransfer(ctx, store.FileTransfer{
		ID: tid, WorkspaceID: id, Tenant: p.TenantID, PrincipalIssuer: p.Issuer, PrincipalSubject: p.Subject,
		RuntimeGeneration: obs.ObservedRuntimeGeneration, RuntimeUID: obs.RuntimeUID,
		Direction: req.Direction, Path: req.Path, DeclaredBytes: declared,
		ResolvedPolicy: string(pol.FileTransfer), ExpiresAt: expires,
	}); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(fileTransferView{
		ID: tid, Direction: req.Direction, Path: req.Path, DeclaredBytes: declared, MaxBytes: pol.MaxFileBytes,
		ExpiresAt: expires, ContentURL: strings.TrimSuffix(origin, "/") + "/v1/transfers/" + tid + "/content",
	})
}
