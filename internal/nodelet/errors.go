package nodelet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/arugula-salad/loam/pkg/api"
)

// Error is a refusal the node protocol has a code for (docs/API.md, "Errors").
// Anything else that goes wrong is a 500 with code "internal".
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter int // seconds; capacity only
}

func (e *Error) Error() string { return e.Message }

func errorf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func notFound(what, id string) *Error {
	return errorf(http.StatusNotFound, api.CodeNotFound, "no %s %s on this node", what, id)
}

func conflict(format string, a ...any) *Error {
	return errorf(http.StatusConflict, api.CodeConflict, format, a...)
}

func invalid(format string, a ...any) *Error {
	return errorf(http.StatusBadRequest, api.CodeInvalid, format, a...)
}

func exists(what, id string) *Error {
	return errorf(http.StatusConflict, api.CodeAlreadyExists, "%s %s already exists on this node", what, id)
}

func unavailable(format string, a ...any) *Error {
	return errorf(http.StatusServiceUnavailable, api.CodeSandboxUnavailable, format, a...)
}

// capacityRetry is the Retry-After on a refusal for lack of memory: a VM that
// goes idle frees its share within minutes, not seconds.
const capacityRetry = 30

func capacity(format string, a ...any) *Error {
	e := errorf(http.StatusServiceUnavailable, api.CodeCapacity, format, a...)
	e.RetryAfter = capacityRetry
	return e
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Status: http.StatusInternalServerError, Code: "internal", Message: err.Error()}
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	writeJSON(w, e.Status, api.Error{Error: api.ErrorDetail{Code: e.Code, Message: e.Message}})
}
