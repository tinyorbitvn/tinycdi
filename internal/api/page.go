package api

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	DefaultPageLimit = 50
	MaxPageLimit     = 500
)

type SortSpec struct {
	Field string
	Desc  bool
}

func (s SortSpec) String() string {
	if s.Desc {
		return "-" + s.Field
	}
	return s.Field
}

// Cursor is the keyset position after the last returned item. Sort pins the
// ordering it was minted under; Key is the sort column's value rendered as a
// string by the list's store function; ID breaks ties.
type Cursor struct {
	Sort string `json:"s"`
	Key  string `json:"k"`
	ID   string `json:"i"`
}

func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func NextCursor(sort SortSpec, key, id string) string {
	return Cursor{Sort: sort.String(), Key: key, ID: id}.Encode()
}

type PageRequest struct {
	Limit int
	Sort  SortSpec
	After *Cursor
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

func parseSort(v string, allowed []string) (SortSpec, bool) {
	s := SortSpec{Field: v}
	if strings.HasPrefix(v, "-") {
		s = SortSpec{Field: v[1:], Desc: true}
	}
	return s, slices.Contains(allowed, s.Field)
}

// ParsePage reads limit, sort and cursor. Every malformed value is a 400
// INVALID_REQUEST; a cursor minted under a different sort is malformed.
func ParsePage(r *http.Request, allowed []string, defaultSort string) (PageRequest, *Error) {
	q := r.URL.Query()
	p := PageRequest{Limit: DefaultPageLimit}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPageLimit {
			return p, NewError(CodeInvalidRequest, "limit must be 1..500")
		}
		p.Limit = n
	}
	sv := q.Get("sort")
	if sv == "" {
		sv = defaultSort
	}
	s, ok := parseSort(sv, allowed)
	if !ok {
		return p, NewError(CodeInvalidRequest, "unsupported sort")
	}
	p.Sort = s
	if v := q.Get("cursor"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		var c Cursor
		if err != nil || json.Unmarshal(raw, &c) != nil || c.ID == "" || c.Sort != s.String() {
			return p, NewError(CodeInvalidRequest, "bad cursor")
		}
		p.After = &c
	}
	return p, nil
}

// pageInMemory serves one keyset page of a list held in memory: it orders
// items by (key, id) — descending when p.Sort.Desc — keeps the rows strictly
// after p.After, and mints NextCursor when rows remain. key renders the sort
// field so that string order is the field's order.
func pageInMemory[T any](items []T, p PageRequest, key, id func(T) string) Page[T] {
	order := func(a, b T) int {
		c := cmp.Or(strings.Compare(key(a), key(b)), strings.Compare(id(a), id(b)))
		if p.Sort.Desc {
			return -c
		}
		return c
	}
	slices.SortFunc(items, order)
	start := 0
	if p.After != nil {
		start = len(items)
		for i, it := range items {
			c := cmp.Or(strings.Compare(key(it), p.After.Key), strings.Compare(id(it), p.After.ID))
			if p.Sort.Desc {
				c = -c
			}
			if c > 0 {
				start = i
				break
			}
		}
	}
	rest := items[start:]
	if len(rest) <= p.Limit {
		return Page[T]{Items: rest}
	}
	last := rest[p.Limit-1]
	return Page[T]{Items: rest[:p.Limit], NextCursor: NextCursor(p.Sort, key(last), id(last))}
}
