package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegisterRelayCredentialSendsSignedProof(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	var gotAuth string
	var gotBody RelayCredentialRegistration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err = RegisterRelayCredential(
		context.Background(),
		server.URL,
		"agent-home",
		"key-1",
		base64.RawStdEncoding.EncodeToString(publicKey),
		base64.RawStdEncoding.EncodeToString(privateKey),
	)
	if err != nil {
		t.Fatalf("RegisterRelayCredential() error = %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want no shared registration bearer token", gotAuth)
	}
	if gotBody.AgentID != "agent-home" || gotBody.KeyID != "key-1" || gotBody.ProofToken == "" {
		t.Fatal("registration body is missing the expected agent ID, key ID, or signed proof")
	}
}

func TestRunOnceTimesOutStalledDial(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
			return
		}
		close(accepted)
	}()

	originalTimeout := controlPlaneDialTimeout
	controlPlaneDialTimeout = 50 * time.Millisecond
	defer func() { controlPlaneDialTimeout = originalTimeout }()

	client := &Client{
		target:  listener.Addr().String(),
		agentID: "agent-home",
		version: "test",
	}
	err = client.runOnce(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runOnce() error = %v, want deadline exceeded", err)
	}

	select {
	case conn, ok := <-accepted:
		if ok {
			conn.Close()
		}
	default:
	}
}

func TestHandleHostedPairingRequiresHostedBaseURL(t *testing.T) {
	client := &Client{}
	response := client.handleHostedPairing(&RelayRequest{
		FetchID: "fetch-1",
		Method:  http.MethodPost,
		Path:    "/v1/pairing/redeem",
		Body:    []byte(`{"deviceName":"Test iPhone"}`),
	})

	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusInternalServerError)
	}
	if !strings.Contains(string(response.Body), `"error":"hosted_base_url_required"`) {
		t.Fatalf("body = %s, want hosted_base_url_required error", response.Body)
	}
}

func TestHandleMediaRequestRequiresHostedBaseURL(t *testing.T) {
	client := &Client{}
	parsedURL, err := url.ParseRequestURI("/v1/assets/search")
	if err != nil {
		t.Fatalf("ParseRequestURI() error = %v", err)
	}

	response := client.handleMediaRequest(&RelayRequest{
		FetchID: "fetch-2",
		Method:  http.MethodPost,
		Path:    "/v1/assets/search",
		Body:    []byte(`{"collection":{"kind":"timeline"},"page":{"index":0,"size":60}}`),
	}, parsedURL)

	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusInternalServerError)
	}
	if !strings.Contains(string(response.Body), `"error":"hosted_base_url_required"`) {
		t.Fatalf("body = %s, want hosted_base_url_required error", response.Body)
	}
}

func TestHandleMediaRequestRejectsOversizedHostedResponse(t *testing.T) {
	client := &Client{
		hostedBaseURL: "https://timich.runo.jp",
		mediaHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("x"), hostedRelayResponseMaxBodySize+1))
		}),
	}
	parsedURL, err := url.ParseRequestURI("/v1/assets/asset-1/original")
	if err != nil {
		t.Fatalf("ParseRequestURI() error = %v", err)
	}

	response := client.handleMediaRequest(&RelayRequest{
		FetchID: "fetch-3",
		Method:  http.MethodGet,
		Path:    "/v1/assets/asset-1/original",
	}, parsedURL)

	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(string(response.Body), `"error":"hosted_response_too_large"`) {
		t.Fatalf("body = %s, want hosted_response_too_large error", response.Body)
	}
}

func TestRegisterRelayCredentialRejectsInvalidConfigurationAndReportsServerError(t *testing.T) {
	if err := RegisterRelayCredential(context.Background(), " ", "agent-home", "key-1", "public", "private"); err == nil ||
		!strings.Contains(err.Error(), "URL is empty") {
		t.Fatalf("RegisterRelayCredential(empty URL) error = %v, want empty-URL error", err)
	}
	if err := RegisterRelayCredential(context.Background(), "https://relay.example", "agent-home", "key-1", "public", "invalid"); err == nil {
		t.Fatal("RegisterRelayCredential(invalid private key) error = nil, want signing error")
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("  relay unavailable\n"))
	}))
	defer server.Close()

	err = RegisterRelayCredential(
		context.Background(),
		server.URL+"/",
		"agent-home",
		"key-1",
		base64.RawStdEncoding.EncodeToString(publicKey),
		base64.RawStdEncoding.EncodeToString(privateKey),
	)
	if err == nil || err.Error() != "relay credential registration failed: relay unavailable" {
		t.Fatalf("RegisterRelayCredential(server error) = %v, want trimmed relay failure", err)
	}
}

func TestClientRegisterRelayCredentialReportsSuccessfulSync(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	callbackCount := 0
	var registeredAt time.Time
	client := &Client{
		hostedBaseURL: server.URL,
		agentID:       "agent-home",
		keyID:         "key-1",
		publicKey:     base64.RawStdEncoding.EncodeToString(publicKey),
		privateKey:    base64.RawStdEncoding.EncodeToString(privateKey),
		relayCredentialRegisteredFunc: func(at time.Time) {
			callbackCount++
			registeredAt = at
		},
	}
	startedAt := time.Now().UTC()
	if err := client.registerRelayCredential(context.Background()); err != nil {
		t.Fatalf("registerRelayCredential() error = %v", err)
	}
	completedAt := time.Now().UTC()
	if callbackCount != 1 || registeredAt.Before(startedAt) || registeredAt.After(completedAt) {
		t.Fatalf("registration callback = count %d at %s, want one callback between %s and %s", callbackCount, registeredAt, startedAt, completedAt)
	}

	failedCallback := false
	failedClient := &Client{
		relayCredentialRegisteredFunc: func(time.Time) { failedCallback = true },
	}
	if err := failedClient.registerRelayCredential(context.Background()); err == nil {
		t.Fatal("registerRelayCredential(empty URL) error = nil, want registration error")
	}
	if failedCallback {
		t.Fatal("registration callback ran after failed registration")
	}
}

func TestRelayRegistrationReadyDefaultsAndNormalizesReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		readyFunc  func() (bool, string)
		wantReady  bool
		wantReason string
	}{
		{name: "no readiness hook", wantReady: true},
		{
			name:       "blank reason",
			readyFunc:  func() (bool, string) { return false, "   " },
			wantReason: "setup incomplete",
		},
		{
			name:       "trimmed reason",
			readyFunc:  func() (bool, string) { return true, "  registered  " },
			wantReady:  true,
			wantReason: "registered",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{relayRegistrationReadyFunc: test.readyFunc}
			ready, reason := client.relayRegistrationReady()
			if ready != test.wantReady || reason != test.wantReason {
				t.Fatalf("relayRegistrationReady() = %t/%q, want %t/%q", ready, reason, test.wantReady, test.wantReason)
			}
		})
	}
}

func TestHandleRelayRequestRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		client   *Client
		request  *RelayRequest
		wantID   string
		wantCode string
	}{
		{
			name:     "nil request",
			client:   &Client{},
			wantCode: "invalid_request",
		},
		{
			name:     "invalid path",
			client:   &Client{},
			request:  &RelayRequest{FetchID: "fetch-path", Method: http.MethodGet, Path: "%"},
			wantID:   "fetch-path",
			wantCode: "invalid_request",
		},
		{
			name:     "malformed pairing body",
			client:   &Client{},
			request:  &RelayRequest{FetchID: "fetch-pair", Method: http.MethodPost, Path: "/v1/pairing/redeem", Body: []byte("{")},
			wantID:   "fetch-pair",
			wantCode: "invalid_request",
		},
		{
			name: "invalid HTTP method",
			client: &Client{
				hostedBaseURL: "https://relay.example",
				mediaHandler:  http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
			},
			request:  &RelayRequest{FetchID: "fetch-method", Method: "BAD METHOD", Path: "/v1/assets/search"},
			wantID:   "fetch-method",
			wantCode: "invalid_request",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response := test.client.handleRelayRequest(test.request)
			if response.FetchID != test.wantID || response.StatusCode != http.StatusBadRequest {
				t.Fatalf("handleRelayRequest() = fetch %q status %d, want %q/%d", response.FetchID, response.StatusCode, test.wantID, http.StatusBadRequest)
			}
			if got := relayErrorCode(t, response); got != test.wantCode {
				t.Fatalf("error code = %q, want %q", got, test.wantCode)
			}
		})
	}
}

func TestHandleRelayRequestForwardsMediaRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		fallbackBaseURL     string
		hostedHeader        string
		explicitContentType string
		requestContentType  string
		wantURL             string
		wantContentType     string
	}{
		{
			name:               "hosted header and content-type fallback",
			fallbackBaseURL:    "https://fallback.example/root/",
			hostedHeader:       "  https://relay.example/base///  ",
			requestContentType: "application/json",
			wantURL:            "https://relay.example/base/v1/assets/search?page=2",
			wantContentType:    "application/json",
		},
		{
			name:                "configured base and explicit content type",
			fallbackBaseURL:     "  https://fallback.example/root///  ",
			explicitContentType: "application/custom",
			requestContentType:  "application/json",
			wantURL:             "https://fallback.example/root/v1/assets/search?page=2",
			wantContentType:     "application/custom",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var handled bool
			client := &Client{
				hostedBaseURL: test.fallbackBaseURL,
				mediaHandler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					handled = true
					if got := request.URL.String(); got != test.wantURL {
						t.Errorf("forwarded URL = %q, want %q", got, test.wantURL)
					}
					if got := request.Header.Values("X-Test"); !reflect.DeepEqual(got, []string{"one", "two"}) {
						t.Errorf("X-Test values = %+v, want repeated values", got)
					}
					if got := request.Header.Get("Content-Type"); got != test.wantContentType {
						t.Errorf("Content-Type = %q, want %q", got, test.wantContentType)
					}
					if got := request.Header.Get("X-Forwarded-Proto"); got != "https" {
						t.Errorf("X-Forwarded-Proto = %q, want https", got)
					}
					if got := request.Header.Get("X-Forwarded-Host"); got != request.URL.Host {
						t.Errorf("X-Forwarded-Host = %q, want %q", got, request.URL.Host)
					}
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Errorf("ReadAll(request body) error = %v", err)
					} else if string(body) != "request-body" {
						t.Errorf("request body = %q, want request-body", body)
					}

					w.Header().Set("Content-Type", "application/octet-stream")
					w.Header().Add("X-Response", "one")
					w.Header().Add("X-Response", "two")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write([]byte("response-body"))
				}),
			}
			headers := []Header{
				{Name: "X-Test", Value: "one"},
				{Name: "X-Test", Value: "two"},
				{Name: "X-Forwarded-Proto", Value: "http"},
				{Name: "X-Forwarded-Host", Value: "untrusted.example"},
			}
			if test.hostedHeader != "" {
				headers = append(headers, Header{Name: "x-timich-hosted-base-url", Value: test.hostedHeader})
			}
			if test.explicitContentType != "" {
				headers = append(headers, Header{Name: "Content-Type", Value: test.explicitContentType})
			}

			response := client.handleRelayRequest(&RelayRequest{
				FetchID:     "fetch-media",
				Method:      http.MethodPost,
				Path:        "/v1/assets/search?page=2",
				Headers:     headers,
				Body:        []byte("request-body"),
				ContentType: test.requestContentType,
			})
			if !handled {
				t.Fatal("media handler was not called")
			}
			if response.FetchID != "fetch-media" || response.StatusCode != http.StatusPartialContent ||
				response.ContentType != "application/octet-stream" || string(response.Body) != "response-body" {
				t.Fatalf("relay response = fetch %q status %d type %q body %q, want forwarded response", response.FetchID, response.StatusCode, response.ContentType, response.Body)
			}
			responseHeaders := make(http.Header)
			for _, header := range response.Headers {
				responseHeaders.Add(header.Name, header.Value)
			}
			if got := responseHeaders.Values("X-Response"); !reflect.DeepEqual(got, []string{"one", "two"}) {
				t.Fatalf("response X-Response values = %+v, want repeated values", got)
			}
		})
	}
}

func TestHandleMediaRequestRejectsMalformedHostedBaseURL(t *testing.T) {
	t.Parallel()

	parsedURL, err := url.ParseRequestURI("/v1/assets/search")
	if err != nil {
		t.Fatalf("ParseRequestURI() error = %v", err)
	}
	client := &Client{
		hostedBaseURL: "http://[::1",
		mediaHandler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("media handler called for malformed hosted base URL")
		}),
	}
	response := client.handleMediaRequest(&RelayRequest{
		FetchID: "fetch-invalid-base",
		Method:  http.MethodGet,
		Path:    "/v1/assets/search",
	}, parsedURL)
	if response.FetchID != "fetch-invalid-base" || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("handleMediaRequest() = fetch %q status %d, want fetch-invalid-base/400", response.FetchID, response.StatusCode)
	}
	if got := relayErrorCode(t, response); got != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", got)
	}
}

func TestLimitedResponseRecorderBounds(t *testing.T) {
	t.Parallel()

	t.Run("exact capacity and first status", func(t *testing.T) {
		t.Parallel()

		recorder := newLimitedResponseRecorder(4)
		if got := recorder.StatusCode(); got != http.StatusOK {
			t.Fatalf("StatusCode() before write = %d, want %d", got, http.StatusOK)
		}
		recorder.Header().Set("X-Test", "value")
		recorder.WriteHeader(http.StatusCreated)
		recorder.WriteHeader(http.StatusInternalServerError)
		written, err := recorder.Write([]byte("test"))
		if err != nil || written != 4 {
			t.Fatalf("Write(exact capacity) = %d/%v, want 4/nil", written, err)
		}
		if recorder.StatusCode() != http.StatusCreated || recorder.TooLarge() || string(recorder.Body()) != "test" {
			t.Fatalf("recorder = status %d tooLarge %t body %q, want 201/false/test", recorder.StatusCode(), recorder.TooLarge(), recorder.Body())
		}
		if got := recorder.Header().Get("X-Test"); got != "value" {
			t.Fatalf("Header().Get(X-Test) = %q, want value", got)
		}
	})

	t.Run("single write overflow", func(t *testing.T) {
		t.Parallel()

		recorder := newLimitedResponseRecorder(4)
		written, err := recorder.Write([]byte("hello"))
		if written != 0 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Write(overflow) = %d/%v, want 0/%v", written, err, io.ErrShortWrite)
		}
		if recorder.StatusCode() != http.StatusOK || !recorder.TooLarge() || string(recorder.Body()) != "hell" {
			t.Fatalf("recorder = status %d tooLarge %t body %q, want 200/true/hell", recorder.StatusCode(), recorder.TooLarge(), recorder.Body())
		}
	})

	t.Run("overflow after partial body", func(t *testing.T) {
		t.Parallel()

		recorder := newLimitedResponseRecorder(4)
		if written, err := recorder.Write([]byte("ab")); err != nil || written != 2 {
			t.Fatalf("first Write() = %d/%v, want 2/nil", written, err)
		}
		written, err := recorder.Write([]byte("cde"))
		if written != 0 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("second Write() = %d/%v, want 0/%v", written, err, io.ErrShortWrite)
		}
		if !recorder.TooLarge() || string(recorder.Body()) != "abcd" {
			t.Fatalf("recorder = tooLarge %t body %q, want true/abcd", recorder.TooLarge(), recorder.Body())
		}
	})

	t.Run("negative capacity clamps to zero", func(t *testing.T) {
		t.Parallel()

		recorder := newLimitedResponseRecorder(-1)
		written, err := recorder.Write([]byte("x"))
		if written != 0 || !errors.Is(err, io.ErrShortWrite) || !recorder.TooLarge() || len(recorder.Body()) != 0 {
			t.Fatalf("zero-capacity Write() = %d/%v tooLarge %t body %q", written, err, recorder.TooLarge(), recorder.Body())
		}
	})
}

func TestFlattenHTTPHeadersPreservesRepeatedValues(t *testing.T) {
	t.Parallel()

	input := make(http.Header)
	input.Add("Content-Type", "application/json")
	input.Add("Set-Cookie", "one=1")
	input.Add("Set-Cookie", "two=2")

	flattened := flattenHTTPHeaders(input)
	rebuilt := make(http.Header)
	for _, header := range flattened {
		rebuilt.Add(header.Name, header.Value)
	}
	if !reflect.DeepEqual(rebuilt, input) {
		t.Fatalf("rebuilt headers = %+v, want %+v", rebuilt, input)
	}
	if got := flattenHTTPHeaders(nil); len(got) != 0 {
		t.Fatalf("flattenHTTPHeaders(nil) = %+v, want empty", got)
	}
}

func TestHostedBaseURLFromHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		headers  []Header
		fallback string
		want     string
	}{
		{name: "fallback", fallback: "  https://fallback.example/root///  ", want: "https://fallback.example/root"},
		{
			name:     "case-insensitive header",
			headers:  []Header{{Name: "x-timich-hosted-base-url", Value: "  https://relay.example/base///  "}},
			fallback: "https://fallback.example",
			want:     "https://relay.example/base",
		},
		{
			name: "first matching header wins",
			headers: []Header{
				{Name: "X-Timich-Hosted-Base-URL", Value: "https://first.example/"},
				{Name: "X-Timich-Hosted-Base-URL", Value: "https://second.example/"},
			},
			fallback: "https://fallback.example",
			want:     "https://first.example",
		},
		{
			name:     "blank explicit header",
			headers:  []Header{{Name: "X-Timich-Hosted-Base-URL", Value: "   "}},
			fallback: "https://fallback.example",
			want:     "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := hostedBaseURLFromHeaders(test.headers, test.fallback); got != test.want {
				t.Fatalf("hostedBaseURLFromHeaders() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestJSONErrorResponseContract(t *testing.T) {
	t.Parallel()

	response := jsonErrorResponse("fetch-error", http.StatusConflict, "conflict", "Request conflicted.")
	if response.FetchID != "fetch-error" || response.StatusCode != http.StatusConflict || response.ContentType != "application/json" {
		t.Fatalf("jsonErrorResponse() = fetch %q status %d type %q, want expected metadata", response.FetchID, response.StatusCode, response.ContentType)
	}
	if !bytes.HasSuffix(response.Body, []byte("\n")) {
		t.Fatalf("jsonErrorResponse() body = %q, want newline terminator", response.Body)
	}
	if len(response.Headers) != 1 || response.Headers[0] != (Header{Name: "Content-Type", Value: "application/json"}) {
		t.Fatalf("jsonErrorResponse() headers = %+v, want JSON content type", response.Headers)
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("Unmarshal(error response) error = %v", err)
	}
	if !reflect.DeepEqual(payload, map[string]string{"error": "conflict", "message": "Request conflicted."}) {
		t.Fatalf("error payload = %+v, want code and message", payload)
	}
}

func relayErrorCode(t *testing.T, response RelayResponse) string {
	t.Helper()

	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("Unmarshal(relay error) error = %v; body = %q", err, response.Body)
	}
	return payload.Error
}
