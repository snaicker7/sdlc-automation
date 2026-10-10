// Package httpapi exposes the store as a loopback-only JSON REST API.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"docs/internal/store"
)

// maxBodyBytes caps a request body; content is separately limited to store.MaxContentBytes.
const maxBodyBytes = 2 << 20

type api struct{ store *store.Store }

// NewHandler returns the API handler for s, wrapped in the Host and Origin guards.
func NewHandler(s *store.Store) http.Handler {
	a := &api{store: s}
	mux := http.NewServeMux()
	mux.Handle("POST /documents", requireJSON(a.create))
	mux.HandleFunc("GET /documents", a.list)
	mux.HandleFunc("GET /documents/{id}", a.get)
	mux.HandleFunc("DELETE /documents/{id}", a.delete)
	return hostGuard(originGuard(jsonRouteErrors(mux)))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":"encode response"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(append(b, '\n')) // a failed write means the client went away; nothing to do
}

func writeErrorMsg(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeError maps a store error to a status code.
func writeError(w http.ResponseWriter, err error) {
	var ve *store.ValidationError
	status := http.StatusInternalServerError
	switch {
	case errors.As(err, &ve):
		status = http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrLockTimeout):
		status = http.StatusServiceUnavailable
	}
	writeErrorMsg(w, status, err.Error())
}

func (a *api) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in store.Input
	err := dec.Decode(&in)
	if err == nil {
		if _, err = dec.Token(); err == nil {
			err = errors.New("unexpected data after JSON value")
		} else if err == io.EOF {
			err = nil
		}
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErrorMsg(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body must be at most %d bytes", maxBodyBytes))
			return
		}
		writeErrorMsg(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	doc, err := a.store.Add(r.Context(), in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

func (a *api) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var text string
	qs, qSet := q["q"]
	if qSet {
		text = qs[0]
	}
	f, err := store.ParseFilter(q["tag"], text, qSet)
	if err != nil {
		writeError(w, err)
		return
	}
	docs, err := a.store.List(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

func parseID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, &store.ValidationError{Field: "id", Message: fmt.Sprintf("%q is not a number", raw)}
	}
	return id, nil
}

func (a *api) get(w http.ResponseWriter, r *http.Request) {
	a.withID(w, r, func(ctx context.Context, id int64) {
		doc, err := a.store.Get(ctx, id)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, doc)
	})
}

func (a *api) delete(w http.ResponseWriter, r *http.Request) {
	a.withID(w, r, func(ctx context.Context, id int64) {
		if err := a.store.Delete(ctx, id); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (a *api) withID(w http.ResponseWriter, r *http.Request, f func(context.Context, int64)) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	f(r.Context(), id)
}
