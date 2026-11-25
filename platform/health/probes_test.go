package health_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Yulian302/deliverit/platform/health"
)

func TestWithProbes_ServesProbesAndPassesTheRest(t *testing.T) {
	hc := health.New()
	var appGot *http.Request
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appGot = r
		w.WriteHeader(http.StatusTeapot)
	})
	h := health.WithProbes(hc, app)

	for path, want := range map[string]int{"/livez": http.StatusOK, "/readyz": http.StatusOK} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	if appGot != nil {
		t.Error("a probe request reached the app")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/restaurants", nil))
	if rec.Code != http.StatusTeapot || appGot == nil {
		t.Fatalf("app route: status %d, app called %v; want the app to handle it", rec.Code, appGot != nil)
	}
	// The tracing middleware inside app names spans from the route found by
	// the app's own router. If this outer split left a pattern on the
	// request, otelhttp would rename every span after it ("GET /").
	if appGot.Pattern != "" {
		t.Errorf("request reached the app with Pattern %q, want none", appGot.Pattern)
	}
}

func TestWithProbes_ProbesAreGETOnly(t *testing.T) {
	called := false
	h := health.WithProbes(health.New(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/livez", nil))
	if !called {
		t.Error("POST /livez was answered as a probe; only GET/HEAD should be")
	}
}
