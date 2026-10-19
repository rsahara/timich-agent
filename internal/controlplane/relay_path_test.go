package controlplane

import (
	"net/http"
	"net/url"
	"testing"
)

func TestRelayRejectsHostedBaseRouteSubstitution(t *testing.T) {
	for _, base := range []string{
		"https://relay.example/v1/assets/asset-1/original?ignored=",
		"https://relay.example/v1/assets/asset-1/original#",
		"https://relay.example/v1/assets/asset-1/original#ignored",
		"https://relay.example?",
		"https://relay.example?route=original",
		"https://relay.example/base",
		"https://relay.example/%2f",
		"https://user@relay.example",
		"https:relay.example",
		"//relay.example",
		"ftp://relay.example",
	} {
		for _, source := range []string{"header", "fallback"} {
			t.Run(source+"/"+base, func(t *testing.T) {
				handled := false
				client := &Client{hostedBaseURL: "https://fallback.example", mediaHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handled = true
					t.Errorf("unsafe base reached media handler at %q", r.URL.String())
				})}
				request := &RelayRequest{FetchID: "fetch-scope", Method: http.MethodGet, Path: "/v1/assets/search/capabilities"}
				if source == "header" {
					request.Headers = []Header{{Name: "x-timich-hosted-base-url", Value: base}}
				} else {
					client.hostedBaseURL = base
				}
				response := client.handleRelayRequest(request)
				if handled || response.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d, handled = %v, want 400 without dispatch", response.StatusCode, handled)
				}
				if got := relayErrorCode(t, response); got != "invalid_request" {
					t.Errorf("error code = %q", got)
				}
			})
		}
	}
}

func TestRelayPreservesAuthorizedRequestTarget(t *testing.T) {
	for _, target := range []string{
		"/v1/assets/search/capabilities",
		"/v1/assets/asset-1/preview",
		"/v1/assets/asset-1/detail_preview",
		"/v1/assets/asset-1/original",
		"/v1/assets/a%2Fb/preview?value=a%26b&value=c%2Fd",
		"/v1/assets/search/capabilities?",
	} {
		t.Run(target, func(t *testing.T) {
			want, err := url.ParseRequestURI(target)
			if err != nil {
				t.Fatal(err)
			}
			handled := false
			client := &Client{hostedBaseURL: "  https://relay.example///  ", mediaHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handled = true
				if r.URL.Path != want.Path || r.URL.RawPath != want.RawPath || r.URL.RawQuery != want.RawQuery || r.URL.ForceQuery != want.ForceQuery {
					t.Errorf("request target changed: got %q, want %q", r.URL.RequestURI(), target)
				}
				if r.URL.Scheme != "https" || r.URL.Host != "relay.example" {
					t.Error("forwarded authority changed")
				}
				w.WriteHeader(http.StatusNoContent)
			})}
			response := client.handleRelayRequest(&RelayRequest{FetchID: "fetch-control", Method: http.MethodGet, Path: target})
			if !handled || response.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, handled = %v", response.StatusCode, handled)
			}
		})
	}
}
