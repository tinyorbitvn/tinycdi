package api

import (
	"net/http"
	"strconv"
	"time"
)

// TemplateHandler implements GET /v1/templates per openapi.yaml.
type TemplateHandler struct {
	catalog TemplateCatalog
	tenants TenantResolver
}

// NewTemplateHandler wires the catalog endpoint.
func NewTemplateHandler(c TemplateCatalog, t TenantResolver) *TemplateHandler {
	return &TemplateHandler{catalog: c, tenants: t}
}

type templateResources struct {
	CPUMillis  int64 `json:"cpuMillicores"`
	MemoryMiB  int64 `json:"memoryMib"`
	StorageGiB int64 `json:"storageGib"`
}

type lifecycleDefaults struct {
	IdleTimeoutSeconds     int64 `json:"idleTimeoutSeconds"`
	DisconnectGraceSeconds int64 `json:"disconnectGraceSeconds"`
	MaxRunningSeconds      int64 `json:"maxRunningSeconds"`
}

type templateView struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	Revision          int64             `json:"revision"`
	Runtime           string            `json:"runtime"`
	Experience        string            `json:"experience"`
	Resources         templateResources `json:"resources"`
	LifecycleDefaults lifecycleDefaults `json:"lifecycleDefaults"`
	DataPolicyDefault string            `json:"dataPolicyDefault"`
	ClipboardPolicy   string            `json:"clipboardPolicy"`
	NetworkProfile    string            `json:"networkProfile"`
	PublishedAt       time.Time         `json:"publishedAt"`
}

type templateList struct {
	Items         []templateView `json:"items"`
	NextPageToken string         `json:"nextPageToken,omitempty"`
}

// List handles GET /v1/templates.
func (h *TemplateHandler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	if _, ok := h.tenants.Namespace(p.TenantID); !ok {
		writeError(w, r, CodeForbidden, "tenant is not provisioned")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, r, CodeInvalidRequest, "limit must be 1..200")
			return
		}
		limit = n
	}
	entries, next, err := h.catalog.List(r.Context(), p.TenantID,
		r.URL.Query().Get("runtime"), r.URL.Query().Get("pageToken"), limit)
	if err != nil {
		writeError(w, r, CodeInternal, "internal error")
		return
	}
	out := templateList{Items: make([]templateView, 0, len(entries)), NextPageToken: next}
	for _, e := range entries {
		out.Items = append(out.Items, templateView{
			ID:          e.ID,
			Name:        e.Name,
			Description: e.Description,
			Revision:    e.Revision,
			Runtime:     e.Runtime,
			Experience:  e.Experience,
			Resources: templateResources{
				CPUMillis:  e.CPUMillis,
				MemoryMiB:  e.MemoryMiB,
				StorageGiB: e.StorageGiB,
			},
			LifecycleDefaults: lifecycleDefaults{
				IdleTimeoutSeconds:     e.IdleTimeoutSeconds,
				DisconnectGraceSeconds: e.DisconnectGraceSeconds,
				MaxRunningSeconds:      e.MaxRunningSeconds,
			},
			DataPolicyDefault: e.DataPolicyDefault,
			ClipboardPolicy:   e.ClipboardPolicy,
			NetworkProfile:    e.NetworkProfile,
			PublishedAt:       e.PublishedAt,
		})
	}
	respondJSON(w, out)
}
