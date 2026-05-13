package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		t.Fatalf("body = %+v, want relay credential registration", gotBody)
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
