package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestPrehookBridgeIDStaysOnePathSegment pins that the bridge session id
// gateViaPrehook puts in the permission-gate URL occupies exactly one path
// segment on the wire.
//
// Why it matters here specifically: the gate is fail-closed, so an id that
// slides into a neighbouring route does not open a hole — it denies. What it
// does instead is make a denial ambiguous, because "the rule engine said no"
// and "the request never reached the rule engine" arrive as the same non-2xx.
// Escaping keeps the request addressed at the engine.
//
// It asserts r.RequestURI, NOT r.URL.Path. Go's server has already decoded
// %2F back to a slash by the time it fills URL.Path, so a URL.Path assertion
// reads identically whether or not the client escaped anything.
func TestPrehookBridgeIDStaysOnePathSegment(t *testing.T) {
	ids := []struct {
		name string
		id   string
	}{
		{"well formed", "br_01HXYZ"},
		{"fragment", "br#frag"},
		{"query", "br?allow=1"},
		{"extra segment", "a/b"},
		{"climbs out of the collection", "../permission"},
		{"space", "br one"},
		{"already percent encoded", "br%2Fb"},
	}

	for _, probe := range ids {
		t.Run(probe.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.RequestURI
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"hookSpecificOutput":{"permissionDecision":"allow"}}`))
			}))
			defer srv.Close()

			gateViaPrehook(context.Background(), srv.URL, probe.id, "Bash", map[string]any{}, "tu_1")

			want := "/permission/codex-prehook/" + url.PathEscape(probe.id)
			if got != want {
				t.Errorf("id %q addressed %q, want %q", probe.id, got, want)
			}
		})
	}
}
