// Package jmaptest holds the fake JMAP server pieces that this module's tests share.
package jmaptest

import (
	"encoding/json"
	"net/http"
)

// callParts is a JMAP method call's name and arguments; the call ID is not needed.
const callParts = 2

// Call returns the name and arguments of a JMAP request's first method call.
// A malformed request yields an empty name, which the fake servers answer as unknown.
func Call(req *http.Request) (string, map[string]any) {
	var body struct {
		MethodCalls [][]json.RawMessage `json:"methodCalls"`
	}

	var (
		method string
		args   map[string]any
	)

	err := json.NewDecoder(req.Body).Decode(&body)
	if err != nil || len(body.MethodCalls) == 0 || len(body.MethodCalls[0]) < callParts {
		return method, args
	}

	_ = json.Unmarshal(body.MethodCalls[0][0], &method)
	_ = json.Unmarshal(body.MethodCalls[0][1], &args)

	return method, args
}

// WriteJSON answers with value, or with a server error the client under test then reports.
func WriteJSON(writer http.ResponseWriter, value any) {
	err := json.NewEncoder(writer).Encode(value)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}
