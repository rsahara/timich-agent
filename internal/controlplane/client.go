package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	contractscontrolplane "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"github.com/rsahara/timich-agent/internal/controlplanetransport"
	"github.com/rsahara/timich-agent/internal/mediaapi"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/store"
	"google.golang.org/grpc"
)

const (
	heartbeatInterval              = 15 * time.Second
	hostedRelayResponseMaxBodySize = controlplanetransport.MaxRelayMessageBytes
)

type Client struct {
	version                       string
	target                        string
	hostedBaseURL                 string
	agentID                       string
	runtime                       *runtimestate.AgentRuntime
	mediaHandler                  http.Handler
	tlsConfig                     controlplanetransport.TLSConfig
	tokenSigner                   *controlplanetransport.TokenSigner
	keyID                         string
	publicKey                     string
	privateKey                    string
	relayRegistrationReadyFunc    func() (bool, string)
	relayCredentialRegisteredFunc func(time.Time)
}

type TLSConfig = controlplanetransport.TLSConfig

type ClientOptions struct {
	TLS                           TLSConfig
	KeyID                         string
	PublicKey                     string
	PrivateKey                    string
	RelayRegistrationReadyFunc    func() (bool, string)
	RelayCredentialRegisteredFunc func(time.Time)
}

type RelayCredentialRegistration struct {
	AgentID    string `json:"agentId"`
	KeyID      string `json:"keyId"`
	PublicKey  string `json:"publicKey"`
	ProofToken string `json:"proofToken"`
}

func NewClient(
	version string,
	target string,
	hostedBaseURL string,
	runtime *runtimestate.AgentRuntime,
	options ClientOptions,
) *Client {
	status := runtime.StatusResponse()
	return &Client{
		version:                       version,
		target:                        strings.TrimSpace(target),
		hostedBaseURL:                 strings.TrimRight(strings.TrimSpace(hostedBaseURL), "/"),
		agentID:                       status.AgentID,
		runtime:                       runtime,
		mediaHandler:                  mediaapi.NewMux(runtime),
		tlsConfig:                     options.TLS,
		tokenSigner:                   controlplanetransport.NewTokenSigner(status.AgentID, options.KeyID, options.PrivateKey, controlplanetransport.DefaultTokenTTL),
		keyID:                         strings.TrimSpace(options.KeyID),
		publicKey:                     strings.TrimSpace(options.PublicKey),
		privateKey:                    strings.TrimSpace(options.PrivateKey),
		relayRegistrationReadyFunc:    options.RelayRegistrationReadyFunc,
		relayCredentialRegisteredFunc: options.RelayCredentialRegisteredFunc,
	}
}

func RegisterRelayCredential(
	ctx context.Context,
	serverURL string,
	agentID string,
	keyID string,
	publicKey string,
	privateKey string,
) error {
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if serverURL == "" {
		return errors.New("relay server URL is empty")
	}
	proofToken, err := contractscontrolplane.MintControlPlaneToken(
		privateKey,
		keyID,
		agentID,
		time.Now().UTC(),
		controlplanetransport.DefaultTokenTTL,
	)
	if err != nil {
		return err
	}
	body, err := json.Marshal(RelayCredentialRegistration{
		AgentID:    strings.TrimSpace(agentID),
		KeyID:      strings.TrimSpace(keyID),
		PublicKey:  strings.TrimSpace(publicKey),
		ProofToken: proofToken,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/v1/agents/relay-credentials", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		limited, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return errors.New("relay credential registration failed: " + strings.TrimSpace(string(limited)))
	}
	return nil
}

func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if ready, reason := c.relayRegistrationReady(); !ready {
			log.Printf("timich-agent relay registration waiting for setup: %s", reason)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(15 * time.Second):
			}
			backoff = time.Second
			continue
		}

		if err := c.registerRelayCredential(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("timich-agent relay credential registration retrying after error: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("timich-agent control plane reconnecting after error: %v", err)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) relayRegistrationReady() (bool, string) {
	if c.relayRegistrationReadyFunc == nil {
		return true, ""
	}
	ready, reason := c.relayRegistrationReadyFunc()
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "setup incomplete"
	}
	return ready, reason
}

func (c *Client) registerRelayCredential(ctx context.Context) error {
	attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := RegisterRelayCredential(
		attemptCtx,
		c.hostedBaseURL,
		c.agentID,
		c.keyID,
		c.publicKey,
		c.privateKey,
	); err != nil {
		return err
	}
	if c.relayCredentialRegisteredFunc != nil {
		c.relayCredentialRegisteredFunc(time.Now().UTC())
	}
	return nil
}

func (c *Client) runOnce(ctx context.Context) error {
	target, dialOption, err := controlplanetransport.DialOptionForTarget(c.target, c.tlsConfig, c.tokenSigner != nil)
	if err != nil {
		return err
	}

	dialOptions := []grpc.DialOption{
		grpc.WithBlock(),
		dialOption,
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(controlplanetransport.MaxRelayMessageBytes),
			grpc.MaxCallSendMsgSize(controlplanetransport.MaxRelayMessageBytes),
		),
	}
	if c.tokenSigner != nil {
		dialOptions = append(dialOptions, grpc.WithPerRPCCredentials(c.tokenSigner))
	}

	conn, err := grpc.DialContext(
		ctx,
		target,
		dialOptions...,
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := conn.NewStream(ctx, controlplanetransport.ControlStreamDesc, OpenControlStreamFullMethod())
	if err != nil {
		return err
	}

	hello, err := controlplanetransport.NewHelloEvent(c.agentID, c.version)
	if err != nil {
		return err
	}
	if err := stream.SendMsg(hello); err != nil {
		return err
	}

	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go c.heartbeatLoop(heartbeatCtx, stream)

	for {
		command, err := newMessage("ServerCommand")
		if err != nil {
			return err
		}
		if err := stream.RecvMsg(command); err != nil {
			return err
		}

		_, _, relayRequest, err := parseServerCommand(command)
		if err != nil || relayRequest == nil {
			continue
		}

		result := c.handleRelayRequest(relayRequest)
		uploadRequest, err := newUploadRequest(result)
		if err != nil {
			return err
		}
		uploadResponse, err := newMessage("FetchResultUploadResponse")
		if err != nil {
			return err
		}
		if err := conn.Invoke(ctx, UploadFetchResultFullMethod(), uploadRequest, uploadResponse); err != nil {
			return err
		}
	}
}

func (c *Client) heartbeatLoop(ctx context.Context, stream grpc.ClientStream) {
	sendHeartbeat := func() error {
		heartbeat, err := newHeartbeatEvent(c.agentID, "online")
		if err != nil {
			return err
		}
		return stream.SendMsg(heartbeat)
	}

	if err := sendHeartbeat(); err != nil {
		return
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sendHeartbeat(); err != nil {
				return
			}
		}
	}
}

func (c *Client) handleRelayRequest(request *RelayRequest) RelayResponse {
	if request == nil {
		return jsonErrorResponse("", http.StatusBadRequest, "invalid_request", "The control-plane request is not valid.")
	}

	parsedURL, err := url.ParseRequestURI(request.Path)
	if err != nil {
		return jsonErrorResponse(request.FetchID, http.StatusBadRequest, "invalid_request", "The control-plane request path is not valid.")
	}

	switch {
	case request.Method == http.MethodPost && parsedURL.Path == "/v1/pairing/redeem":
		return c.handleHostedPairing(request)
	default:
		return c.handleMediaRequest(request, parsedURL)
	}
}

func (c *Client) handleHostedPairing(request *RelayRequest) RelayResponse {
	var payload struct {
		DeviceName string `json:"deviceName"`
	}
	if err := json.Unmarshal(request.Body, &payload); err != nil {
		return jsonErrorResponse(request.FetchID, http.StatusBadRequest, "invalid_request", "Could not parse the pairing redemption request.")
	}

	hostedBaseURL := hostedBaseURLFromHeaders(request.Headers, c.hostedBaseURL)
	if hostedBaseURL == "" {
		return jsonErrorResponse(
			request.FetchID,
			http.StatusInternalServerError,
			"hosted_base_url_required",
			"The relay server URL is not configured for this pairing request.",
		)
	}

	response, err := c.createHostedSession(payload.DeviceName, hostedBaseURL)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDeviceLimitReached):
			return jsonErrorResponse(request.FetchID, http.StatusConflict, "device_limit_reached", "The local agent has reached its paired-device limit.")
		default:
			return jsonErrorResponse(request.FetchID, http.StatusInternalServerError, "pairing_failed", "Could not create the remote browsing app session.")
		}
	}

	body, err := json.Marshal(response)
	if err != nil {
		return jsonErrorResponse(request.FetchID, http.StatusInternalServerError, "pairing_failed", "Could not create the remote browsing app session.")
	}
	return RelayResponse{
		FetchID:     request.FetchID,
		StatusCode:  http.StatusCreated,
		Headers:     []Header{{Name: "Content-Type", Value: "application/json"}},
		Body:        append(body, '\n'),
		ContentType: "application/json",
	}
}

func (c *Client) handleMediaRequest(request *RelayRequest, parsedURL *url.URL) RelayResponse {
	baseURL := hostedBaseURLFromHeaders(request.Headers, c.hostedBaseURL)
	if baseURL == "" {
		return jsonErrorResponse(
			request.FetchID,
			http.StatusInternalServerError,
			"hosted_base_url_required",
			"The relay server URL is not configured for this relay request.",
		)
	}

	targetURL, err := url.Parse(strings.TrimRight(baseURL, "/") + parsedURL.RequestURI())
	if err != nil {
		return jsonErrorResponse(request.FetchID, http.StatusBadRequest, "invalid_request", "The control-plane request path is not valid.")
	}

	httpRequest, err := http.NewRequest(request.Method, targetURL.String(), bytes.NewReader(request.Body))
	if err != nil {
		return jsonErrorResponse(request.FetchID, http.StatusBadRequest, "invalid_request", "The control-plane request could not be created.")
	}
	for _, header := range request.Headers {
		httpRequest.Header.Add(header.Name, header.Value)
	}
	httpRequest.Header.Set("X-Forwarded-Proto", targetURL.Scheme)
	httpRequest.Header.Set("X-Forwarded-Host", targetURL.Host)
	if request.ContentType != "" && httpRequest.Header.Get("Content-Type") == "" {
		httpRequest.Header.Set("Content-Type", request.ContentType)
	}

	recorder := newLimitedResponseRecorder(hostedRelayResponseMaxBodySize)
	c.mediaHandler.ServeHTTP(recorder, httpRequest)
	if recorder.TooLarge() {
		return jsonErrorResponse(
			request.FetchID,
			http.StatusRequestEntityTooLarge,
			"hosted_response_too_large",
			"The Remote Browsing media response is too large.",
		)
	}

	return RelayResponse{
		FetchID:     request.FetchID,
		StatusCode:  recorder.StatusCode(),
		Headers:     flattenHTTPHeaders(recorder.Header()),
		Body:        recorder.Body(),
		ContentType: recorder.Header().Get("Content-Type"),
	}
}

type limitedResponseRecorder struct {
	header       http.Header
	body         bytes.Buffer
	statusCode   int
	maxBodyBytes int
	tooLarge     bool
}

func newLimitedResponseRecorder(maxBodyBytes int) *limitedResponseRecorder {
	return &limitedResponseRecorder{
		header:       make(http.Header),
		maxBodyBytes: max(0, maxBodyBytes),
	}
}

func (r *limitedResponseRecorder) Header() http.Header {
	return r.header
}

func (r *limitedResponseRecorder) WriteHeader(statusCode int) {
	if r.statusCode != 0 {
		return
	}
	r.statusCode = statusCode
}

func (r *limitedResponseRecorder) Write(payload []byte) (int, error) {
	if r.statusCode == 0 {
		r.WriteHeader(http.StatusOK)
	}
	remaining := r.maxBodyBytes - r.body.Len()
	if remaining <= 0 || len(payload) > remaining {
		r.tooLarge = true
		if remaining > 0 {
			_, _ = r.body.Write(payload[:remaining])
		}
		return 0, io.ErrShortWrite
	}
	return r.body.Write(payload)
}

func (r *limitedResponseRecorder) StatusCode() int {
	if r.statusCode == 0 {
		return http.StatusOK
	}
	return r.statusCode
}

func (r *limitedResponseRecorder) Body() []byte {
	return r.body.Bytes()
}

func (r *limitedResponseRecorder) TooLarge() bool {
	return r.tooLarge
}

func (c *Client) createHostedSession(deviceName string, hostedBaseURL string) (any, error) {
	return c.runtime.CreateHostedSession(deviceName, hostedBaseURL)
}

func flattenHTTPHeaders(header http.Header) []Header {
	headers := make([]Header, 0, len(header))
	for name, values := range header {
		for _, value := range values {
			headers = append(headers, Header{Name: name, Value: value})
		}
	}
	return headers
}

func hostedBaseURLFromHeaders(headers []Header, fallback string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Name, "X-Timich-Hosted-Base-URL") {
			return strings.TrimRight(strings.TrimSpace(header.Value), "/")
		}
	}
	return strings.TrimRight(strings.TrimSpace(fallback), "/")
}

func jsonErrorResponse(fetchID string, statusCode int, code string, message string) RelayResponse {
	body, _ := json.Marshal(map[string]string{
		"error":   code,
		"message": message,
	})
	return RelayResponse{
		FetchID:     fetchID,
		StatusCode:  statusCode,
		Headers:     []Header{{Name: "Content-Type", Value: "application/json"}},
		Body:        append(body, '\n'),
		ContentType: "application/json",
	}
}
