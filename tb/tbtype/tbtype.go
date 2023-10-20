// Package tbtype contains type definitions for tb API calls.
package tbtype

// FetchResponse is the JSON response of a POST /fetch call.
type FetchResponse struct {
	RemoteRef string `json:"remoteRef,omitempty"`

	Error string `json:"error,omitempty"`

	LocalRef string `json:"localRef,omitempty"`
	Hash     string `json:"hash,omitempty"`
}
