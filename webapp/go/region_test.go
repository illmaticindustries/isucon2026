package isureal

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestRegionalBackendAuthorization(t *testing.T) {
	for _, ingress := range []string{"asia", "europe", "america"} {
		for _, userRegion := range []string{"asia", "europe", "america", ""} {
			t.Run(ingress+"/"+userRegion, func(t *testing.T) {
				e := echo.New()
				req := httptest.NewRequest("GET", "/api/feeds/friends", nil)
				// Client-supplied routing hints must not override the listener's region.
				req.Header.Set("X-Country-Code", "US")
				req.Header.Set("X-Isureal-Region", userRegion)
				c := e.NewContext(req, httptest.NewRecorder())
				if userRegion != "" {
					c.Set("session", &Session{UserID: "u", Region: userRegion})
				}
				called := false
				err := RegionMiddleware(&HandlerContext{Config: &Config{Region: ingress}})(func(*echo.Context) error {
					called = true
					return nil
				})(c)
				if userRegion == ingress {
					if err != nil || !called {
						t.Fatalf("valid region rejected: %v", err)
					}
					return
				}
				want := http.StatusForbidden
				if userRegion == "" {
					want = http.StatusUnauthorized
				}
				apiErr, ok := err.(*APIError)
				if called || !ok || apiErr.Status != want {
					t.Fatalf("invalid region/session accepted: called=%v err=%v", called, err)
				}
			})
		}
	}
}
