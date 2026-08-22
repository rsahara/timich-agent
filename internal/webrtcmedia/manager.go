package webrtcmedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/rsahara/timich-agent/internal/catalog"
)

const (
	defaultChunkSize = 64 << 10
	binaryChunkKind  = byte('D')

	progressLogBytes     = int64(8 << 20)
	progressLogInterval  = 5 * time.Second
	slowSendLogThreshold = 250 * time.Millisecond

	maxDataChannelBufferedAmount = 4 << 20
	sendBudgetPollInterval       = 10 * time.Millisecond
)

type OriginalLoader func(request *http.Request, assetID string) (*catalog.UpstreamMediaResponse, error)

type dataChannel interface {
	SendText(string) error
	Send([]byte) error
	BufferedAmount() uint64
}

type Manager struct {
	original OriginalLoader

	mu       sync.Mutex
	sessions map[string]*session
}

type OfferRequest struct {
	SDP        string      `json:"sdp"`
	Type       string      `json:"type"`
	ICEServers []ICEServer `json:"iceServers,omitempty"`
}

type OfferResponse struct {
	SessionID string `json:"sessionId"`
	SDP       string `json:"sdp"`
	Type      string `json:"type"`
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type session struct {
	id        string
	peer      *webrtc.PeerConnection
	done      chan struct{}
	sendMu    sync.Mutex
	requestMu sync.Mutex
	requests  map[string]*activeMediaRequest
}

type activeMediaRequest struct {
	cancel context.CancelFunc
}

type sendMetrics struct {
	bufferedBefore uint64
	bufferedAfter  uint64
	duration       time.Duration
}

type mediaRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Method    string `json:"method,omitempty"`
	AssetID   string `json:"assetId"`
	Range     string `json:"range,omitempty"`
}

type mediaEnvelope struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
}

type mediaCancel struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Reason    string `json:"reason,omitempty"`
}

type mediaHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type mediaHeaders struct {
	Type        string        `json:"type"`
	RequestID   string        `json:"requestId"`
	StatusCode  int           `json:"statusCode"`
	Headers     []mediaHeader `json:"headers"`
	ContentType string        `json:"contentType,omitempty"`
}

type mediaComplete struct {
	Type       string `json:"type"`
	RequestID  string `json:"requestId"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"durationMs"`
}

type mediaError struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

func NewManager(original OriginalLoader) *Manager {
	return &Manager{
		original: original,
		sessions: make(map[string]*session),
	}
}

func (m *Manager) AnswerOffer(ctx context.Context, request OfferRequest) (OfferResponse, error) {
	if strings.TrimSpace(request.SDP) == "" {
		return OfferResponse{}, errors.New("sdp is required")
	}

	config := webrtc.Configuration{ICEServers: pionICEServers(request.ICEServers)}
	peer, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return OfferResponse{}, fmt.Errorf("create peer connection: %w", err)
	}

	sessionID := "rtc-" + uuid.NewString()
	active := &session{
		id:       sessionID,
		peer:     peer,
		done:     make(chan struct{}),
		requests: make(map[string]*activeMediaRequest),
	}

	peer.OnDataChannel(func(channel *webrtc.DataChannel) {
		log.Printf("webrtc media data channel opened label=%q session_id=%q", channel.Label(), sessionID)
		channel.OnMessage(func(message webrtc.DataChannelMessage) {
			if !message.IsString {
				return
			}
			var envelope mediaEnvelope
			if err := json.Unmarshal(message.Data, &envelope); err != nil {
				active.sendJSON(channel, mediaError{
					Type:    "error",
					Code:    "invalid_request",
					Message: "Could not parse the media request.",
				})
				return
			}
			switch envelope.Type {
			case "media_request":
				var mediaRequest mediaRequest
				if err := json.Unmarshal(message.Data, &mediaRequest); err != nil {
					active.sendJSON(channel, mediaError{
						Type:      "error",
						RequestID: envelope.RequestID,
						Code:      "invalid_request",
						Message:   "Could not parse the media request.",
					})
					return
				}
				go m.handleMediaRequest(active, channel, mediaRequest)
			case "media_cancel":
				var cancel mediaCancel
				if err := json.Unmarshal(message.Data, &cancel); err != nil {
					active.sendJSON(channel, mediaError{
						Type:      "error",
						RequestID: envelope.RequestID,
						Code:      "invalid_cancel",
						Message:   "Could not parse the media cancel request.",
					})
					return
				}
				active.cancelMediaRequest(cancel.RequestID, cancel.Reason)
			default:
				return
			}
		})
	})

	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("webrtc media peer state=%s session_id=%q", state.String(), sessionID)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			active.cancelAllMediaRequests("peer_" + state.String())
			m.removeSession(sessionID)
			_ = peer.Close()
		default:
		}
	})

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  request.SDP,
	}
	if err := peer.SetRemoteDescription(offer); err != nil {
		_ = peer.Close()
		return OfferResponse{}, fmt.Errorf("set remote description: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peer)
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		_ = peer.Close()
		return OfferResponse{}, fmt.Errorf("create answer: %w", err)
	}
	if err := peer.SetLocalDescription(answer); err != nil {
		_ = peer.Close()
		return OfferResponse{}, fmt.Errorf("set local description: %w", err)
	}

	select {
	case <-gatherComplete:
	case <-ctx.Done():
		_ = peer.Close()
		return OfferResponse{}, ctx.Err()
	}

	m.mu.Lock()
	m.sessions[sessionID] = active
	m.mu.Unlock()

	local := peer.LocalDescription()
	if local == nil {
		_ = peer.Close()
		return OfferResponse{}, errors.New("local description missing")
	}

	return OfferResponse{
		SessionID: sessionID,
		SDP:       local.SDP,
		Type:      strings.ToLower(local.Type.String()),
	}, nil
}

func (m *Manager) handleMediaRequest(active *session, channel dataChannel, request mediaRequest) {
	startedAt := time.Now()
	requestID := strings.TrimSpace(request.RequestID)
	if requestID == "" || strings.TrimSpace(request.AssetID) == "" {
		active.sendJSON(channel, mediaError{
			Type:      "error",
			RequestID: requestID,
			Code:      "invalid_request",
			Message:   "The media request requires requestId and assetId.",
		})
		return
	}
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		active.sendJSON(channel, mediaError{
			Type:      "error",
			RequestID: requestID,
			Code:      "invalid_request",
			Message:   "The media request method must be GET or HEAD.",
		})
		return
	}
	requestCtx, unregister := active.registerMediaRequest(requestID)
	defer unregister()

	log.Printf(
		"webrtc media request started session_id=%q request_id=%q method=%q asset_id=%q range=%q",
		active.id,
		requestID,
		method,
		request.AssetID,
		request.Range,
	)

	proxyRequest, err := http.NewRequestWithContext(requestCtx, method, "http://timich-webrtc.local/v1/assets/"+url.PathEscape(request.AssetID)+"/original", nil)
	if err != nil {
		active.sendJSON(channel, mediaError{
			Type:      "error",
			RequestID: requestID,
			Code:      "invalid_request",
			Message:   "Could not create the media request.",
		})
		return
	}
	if rangeHeader := strings.TrimSpace(request.Range); rangeHeader != "" {
		proxyRequest.Header.Set("Range", rangeHeader)
	}
	proxyRequest.Header.Set("Accept-Encoding", "identity")

	response, err := m.original(proxyRequest, request.AssetID)
	if err != nil {
		if errors.Is(requestCtx.Err(), context.Canceled) {
			log.Printf(
				"webrtc media request cancelled before upstream response session_id=%q request_id=%q asset_id=%q range=%q",
				active.id,
				requestID,
				request.AssetID,
				request.Range,
			)
			return
		}
		active.sendJSON(channel, mediaError{
			Type:      "error",
			RequestID: requestID,
			Code:      "upstream_failed",
			Message:   "Could not open the original media response.",
		})
		return
	}
	defer response.Body.Close()

	if errors.Is(requestCtx.Err(), context.Canceled) {
		log.Printf(
			"webrtc media request cancelled before header send session_id=%q request_id=%q asset_id=%q range=%q",
			active.id,
			requestID,
			request.AssetID,
			request.Range,
		)
		return
	}

	log.Printf(
		"webrtc media upstream headers session_id=%q request_id=%q asset_id=%q range=%q status=%d content_length=%q content_range=%q accept_ranges=%q content_type=%q",
		active.id,
		requestID,
		request.AssetID,
		request.Range,
		response.StatusCode,
		response.Header.Get("Content-Length"),
		response.Header.Get("Content-Range"),
		response.Header.Get("Accept-Ranges"),
		response.Header.Get("Content-Type"),
	)

	active.sendJSON(channel, mediaHeaders{
		Type:        "headers",
		RequestID:   requestID,
		StatusCode:  response.StatusCode,
		Headers:     flattenedHeaders(response.Header),
		ContentType: response.Header.Get("Content-Type"),
	})

	if method == http.MethodHead {
		durationMS := time.Since(startedAt).Milliseconds()
		log.Printf(
			"webrtc media request completed session_id=%q request_id=%q method=%q asset_id=%q range=%q status=%d bytes=0 duration_ms=%d content_length=%q content_range=%q",
			active.id,
			requestID,
			method,
			request.AssetID,
			request.Range,
			response.StatusCode,
			durationMS,
			response.Header.Get("Content-Length"),
			response.Header.Get("Content-Range"),
		)
		active.sendJSON(channel, mediaComplete{
			Type:       "complete",
			RequestID:  requestID,
			Bytes:      0,
			DurationMS: durationMS,
		})
		return
	}

	buffer := make([]byte, defaultChunkSize)
	var sentBytes int64
	var maxBufferedAmount uint64
	var maxSendMS int64
	var slowSends int
	lastProgressBytes := int64(0)
	lastProgressAt := time.Now()
	for {
		if err := active.waitForSendBudget(requestCtx, channel); err != nil {
			log.Printf(
				"webrtc media request cancelled before read session_id=%q request_id=%q asset_id=%q range=%q sent_bytes=%d err=%v",
				active.id,
				requestID,
				request.AssetID,
				request.Range,
				sentBytes,
				err,
			)
			return
		}
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			if err := requestCtx.Err(); err != nil {
				log.Printf(
					"webrtc media request cancelled after read session_id=%q request_id=%q asset_id=%q range=%q chunk_bytes=%d sent_bytes=%d err=%v",
					active.id,
					requestID,
					request.AssetID,
					request.Range,
					count,
					sentBytes,
					err,
				)
				return
			}
			payload := make([]byte, 1+36+count)
			payload[0] = binaryChunkKind
			copy(payload[1:37], []byte(requestID))
			copy(payload[37:], buffer[:count])
			metrics, err := active.sendBinary(requestCtx, channel, payload)
			sendMS := metrics.duration.Milliseconds()
			if metrics.bufferedAfter > maxBufferedAmount {
				maxBufferedAmount = metrics.bufferedAfter
			}
			if sendMS > maxSendMS {
				maxSendMS = sendMS
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					log.Printf(
						"webrtc media request cancelled during send session_id=%q request_id=%q asset_id=%q range=%q chunk_bytes=%d sent_bytes=%d send_ms=%d buffered_before=%d buffered_after=%d",
						active.id,
						requestID,
						request.AssetID,
						request.Range,
						count,
						sentBytes,
						sendMS,
						metrics.bufferedBefore,
						metrics.bufferedAfter,
					)
					return
				}
				log.Printf(
					"webrtc media send failed session_id=%q request_id=%q asset_id=%q range=%q chunk_bytes=%d sent_bytes=%d send_ms=%d buffered_before=%d buffered_after=%d err=%v",
					active.id,
					requestID,
					request.AssetID,
					request.Range,
					count,
					sentBytes,
					sendMS,
					metrics.bufferedBefore,
					metrics.bufferedAfter,
					err,
				)
				return
			}
			sentBytes += int64(count)
			now := time.Now()
			if metrics.duration >= slowSendLogThreshold {
				slowSends++
				log.Printf(
					"webrtc media send slow session_id=%q request_id=%q asset_id=%q range=%q chunk_bytes=%d sent_bytes=%d send_ms=%d buffered_before=%d buffered_after=%d max_buffered_amount=%d",
					active.id,
					requestID,
					request.AssetID,
					request.Range,
					count,
					sentBytes,
					sendMS,
					metrics.bufferedBefore,
					metrics.bufferedAfter,
					maxBufferedAmount,
				)
			}
			if sentBytes-lastProgressBytes >= progressLogBytes ||
				(sentBytes != lastProgressBytes && now.Sub(lastProgressAt) >= progressLogInterval) {
				log.Printf(
					"webrtc media progress session_id=%q request_id=%q asset_id=%q range=%q sent_bytes=%d duration_ms=%d send_ms=%d buffered_before=%d buffered_after=%d max_buffered_amount=%d max_send_ms=%d",
					active.id,
					requestID,
					request.AssetID,
					request.Range,
					sentBytes,
					time.Since(startedAt).Milliseconds(),
					sendMS,
					metrics.bufferedBefore,
					metrics.bufferedAfter,
					maxBufferedAmount,
					maxSendMS,
				)
				lastProgressBytes = sentBytes
				lastProgressAt = now
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(requestCtx.Err(), context.Canceled) {
			log.Printf(
				"webrtc media request cancelled during read session_id=%q request_id=%q asset_id=%q range=%q sent_bytes=%d err=%v",
				active.id,
				requestID,
				request.AssetID,
				request.Range,
				sentBytes,
				requestCtx.Err(),
			)
			return
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		log.Printf(
			"webrtc media read failed session_id=%q request_id=%q asset_id=%q range=%q sent_bytes=%d err=%v",
			active.id,
			requestID,
			request.AssetID,
			request.Range,
			sentBytes,
			readErr,
		)
		active.sendJSON(channel, mediaError{
			Type:      "error",
			RequestID: requestID,
			Code:      "read_failed",
			Message:   "Could not read the original media response.",
		})
		return
	}

	if errors.Is(requestCtx.Err(), context.Canceled) {
		log.Printf(
			"webrtc media request cancelled before completion session_id=%q request_id=%q asset_id=%q range=%q sent_bytes=%d",
			active.id,
			requestID,
			request.AssetID,
			request.Range,
			sentBytes,
		)
		return
	}

	durationMS := time.Since(startedAt).Milliseconds()
	log.Printf(
		"webrtc media request completed session_id=%q request_id=%q asset_id=%q range=%q status=%d bytes=%d duration_ms=%d max_buffered_amount=%d max_send_ms=%d slow_sends=%d content_length=%q content_range=%q",
		active.id,
		requestID,
		request.AssetID,
		request.Range,
		response.StatusCode,
		sentBytes,
		durationMS,
		maxBufferedAmount,
		maxSendMS,
		slowSends,
		response.Header.Get("Content-Length"),
		response.Header.Get("Content-Range"),
	)
	active.sendJSON(channel, mediaComplete{
		Type:       "complete",
		RequestID:  requestID,
		Bytes:      sentBytes,
		DurationMS: durationMS,
	})
}

func (m *Manager) removeSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}

func (s *session) registerMediaRequest(requestID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	entry := &activeMediaRequest{cancel: cancel}

	s.requestMu.Lock()
	if previous := s.requests[requestID]; previous != nil {
		previous.cancel()
		log.Printf("webrtc media replaced active request session_id=%q request_id=%q", s.id, requestID)
	}
	s.requests[requestID] = entry
	s.requestMu.Unlock()

	return ctx, func() {
		s.requestMu.Lock()
		if current := s.requests[requestID]; current == entry {
			delete(s.requests, requestID)
		}
		s.requestMu.Unlock()
		cancel()
	}
}

func (s *session) cancelMediaRequest(requestID string, reason string) bool {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return false
	}

	s.requestMu.Lock()
	entry := s.requests[requestID]
	if entry != nil {
		delete(s.requests, requestID)
	}
	s.requestMu.Unlock()

	if entry == nil {
		log.Printf(
			"webrtc media cancel ignored for unknown request session_id=%q request_id=%q reason=%q",
			s.id,
			requestID,
			reason,
		)
		return false
	}

	entry.cancel()
	log.Printf("webrtc media request cancel received session_id=%q request_id=%q reason=%q", s.id, requestID, reason)
	return true
}

func (s *session) cancelAllMediaRequests(reason string) {
	s.requestMu.Lock()
	requests := s.requests
	s.requests = make(map[string]*activeMediaRequest)
	s.requestMu.Unlock()

	if len(requests) == 0 {
		return
	}
	for _, request := range requests {
		request.cancel()
	}
	log.Printf("webrtc media cancelled active requests session_id=%q count=%d reason=%q", s.id, len(requests), reason)
}

func (s *session) sendJSON(channel dataChannel, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	_ = channel.SendText(string(body))
}

func (s *session) waitForSendBudget(ctx context.Context, channel dataChannel) error {
	for channel.BufferedAmount() > maxDataChannelBufferedAmount {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sendBudgetPollInterval):
		}
	}
	return nil
}

func (s *session) sendBinary(ctx context.Context, channel dataChannel, payload []byte) (sendMetrics, error) {
	if err := s.waitForSendBudget(ctx, channel); err != nil {
		return sendMetrics{}, err
	}

	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	if err := s.waitForSendBudget(ctx, channel); err != nil {
		return sendMetrics{}, err
	}
	metrics := sendMetrics{bufferedBefore: channel.BufferedAmount()}
	select {
	case <-ctx.Done():
		metrics.bufferedAfter = channel.BufferedAmount()
		return metrics, ctx.Err()
	default:
	}
	startedAt := time.Now()
	err := channel.Send(payload)
	metrics.duration = time.Since(startedAt)
	metrics.bufferedAfter = channel.BufferedAmount()
	return metrics, err
}

func pionICEServers(servers []ICEServer) []webrtc.ICEServer {
	if len(servers) == 0 {
		return []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		}
	}

	result := make([]webrtc.ICEServer, 0, len(servers))
	for _, server := range servers {
		if len(server.URLs) == 0 {
			continue
		}
		result = append(result, webrtc.ICEServer{
			URLs:       server.URLs,
			Username:   server.Username,
			Credential: server.Credential,
		})
	}
	if len(result) == 0 {
		return []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
	}
	return result
}

func flattenedHeaders(header http.Header) []mediaHeader {
	result := make([]mediaHeader, 0, len(header))
	for _, name := range []string{
		"Content-Type",
		"Content-Length",
		"Content-Range",
		"Accept-Ranges",
		"Cache-Control",
		"ETag",
		"Last-Modified",
		"Content-Disposition",
	} {
		for _, value := range header.Values(name) {
			if strings.TrimSpace(value) != "" {
				result = append(result, mediaHeader{Name: name, Value: value})
			}
		}
	}
	return result
}
