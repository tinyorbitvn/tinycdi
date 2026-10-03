package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// DefaultImageStaleAfter is the -image-stale-after default (14 days): a
// runtime image older than this reports imageStale on template and
// workspace views.
const DefaultImageStaleAfter = 14 * 24 * time.Hour

// DefaultImageBlockAfter is the -image-block-after default (45 days, E3):
// create, and a start that cannot move to a fresher revision, refuse a
// runtime image older than this with 409 IMAGE_STALE.
const DefaultImageBlockAfter = 45 * 24 * time.Hour

// imageFreshness maps the raw image-built-at annotation value to the
// optional view fields: the parsed timestamp and the stale flag (image age
// > staleAfter). A missing or malformed value leaves both fields absent;
// malformed values log one warning and never fail the request.
func imageFreshness(log *slog.Logger, raw string, staleAfter time.Duration, now time.Time, attrs ...any) (*time.Time, *bool) {
	if raw == "" {
		return nil, nil
	}
	built, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		if log == nil {
			log = slog.Default()
		}
		log.Warn("ignoring malformed image-built-at annotation",
			append(attrs, "value", raw, "error", err)...)
		return nil, nil
	}
	stale := now.Sub(built) > staleAfter
	return &built, &stale
}

// TemplateHandler implements GET /v1/templates per openapi.yaml.
type TemplateHandler struct {
	catalog    TemplateCatalog
	tenants    TenantResolver
	staleAfter time.Duration
	// blockAfter is the E3 stale-image admission limit
	// (-image-block-after) that backs the imageBlocked view field; <=0
	// disables (imageBlocked is always false).
	blockAfter time.Duration
	now        func() time.Time
	log        *slog.Logger
}

// NewTemplateHandler wires the catalog endpoint.
func NewTemplateHandler(c TemplateCatalog, t TenantResolver) *TemplateHandler {
	return &TemplateHandler{catalog: c, tenants: t,
		staleAfter: DefaultImageStaleAfter, now: time.Now}
}

// WithImageStaleAfter sets the age after which a runtime image reports
// imageStale (the -image-stale-after flag). Non-positive keeps the default.
func (h *TemplateHandler) WithImageStaleAfter(d time.Duration) *TemplateHandler {
	if d > 0 {
		h.staleAfter = d
	}
	return h
}

// WithImageBlockAfter sets the E3 stale-image admission limit that the
// imageBlocked view field mirrors (-image-block-after). d <= 0 disables
// the block; a handler built without it reports imageBlocked false.
func (h *TemplateHandler) WithImageBlockAfter(d time.Duration) *TemplateHandler {
	h.blockAfter = d
	return h
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
	Family            string            `json:"family"`
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
	ImageBuiltAt      *time.Time        `json:"imageBuiltAt,omitempty"`
	ImageStale        *bool             `json:"imageStale,omitempty"`
	// ImageBlocked is true when the image is older than -image-block-after
	// — a create/start on it is refused 409 IMAGE_STALE (E3). Absent when
	// the template carries no parseable image-built-at; never blocks.
	ImageBlocked *bool `json:"imageBlocked,omitempty"`
	// ImageEngines carries the browser engine versions the image was built
	// with (e.g. {"chromium": "...", "firefox": "..."}); absent when the
	// template declares none.
	ImageEngines map[string]string `json:"imageEngines,omitempty"`
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
		builtAt, stale := imageFreshness(h.log, e.ImageBuiltAt, h.staleAfter, h.now(), "template", e.ID)
		var blocked *bool
		if builtAt != nil {
			b := h.blockAfter > 0 && h.now().Sub(*builtAt) > h.blockAfter
			blocked = &b
		}
		out.Items = append(out.Items, templateView{
			ID:           e.ID,
			Name:         e.Name,
			Family:       e.Name,
			Description:  e.Description,
			Revision:     e.Revision,
			Runtime:      e.Runtime,
			Experience:   e.Experience,
			ImageBuiltAt: builtAt,
			ImageStale:   stale,
			ImageBlocked: blocked,
			ImageEngines: e.ImageEngines,
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
