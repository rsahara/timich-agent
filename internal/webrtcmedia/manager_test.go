package webrtcmedia

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/rsahara/timich-agent/internal/catalog"
)

func TestNewManagerAndRemoveSession(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil)
	if manager == nil || manager.sessions == nil {
		t.Fatal("NewManager() did not initialize the session store")
	}
	manager.sessions["rtc-1"] = newTestSession("rtc-1")
	manager.sessions["rtc-2"] = newTestSession("rtc-2")

	manager.removeSession("rtc-1")

	if _, ok := manager.sessions["rtc-1"]; ok {
		t.Fatal("removeSession() retained the removed session")
	}
	if _, ok := manager.sessions["rtc-2"]; !ok {
		t.Fatal("removeSession() removed an unrelated session")
	}
}

func TestAnswerOfferRejectsMissingAndMalformedSDP(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil)
	if _, err := manager.AnswerOffer(context.Background(), OfferRequest{SDP: "   "}); err == nil || !strings.Contains(err.Error(), "sdp is required") {
		t.Fatalf("AnswerOffer(blank SDP) error = %v, want required-SDP error", err)
	}
	if _, err := manager.AnswerOffer(context.Background(), OfferRequest{SDP: "not-an-sdp"}); err == nil || !strings.Contains(err.Error(), "set remote description") {
		t.Fatalf("AnswerOffer(malformed SDP) error = %v, want remote-description error", err)
	}
	if got := managerSessionCount(manager); got != 0 {
		t.Fatalf("sessions = %d after rejected offers, want 0", got)
	}
}

func TestPionICEServers(t *testing.T) {
	t.Parallel()

	defaultServers := []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
	tests := []struct {
		name     string
		input    []ICEServer
		expected []webrtc.ICEServer
	}{
		{name: "nil uses default", expected: defaultServers},
		{name: "empty entries use default", input: []ICEServer{{}}, expected: defaultServers},
		{
			name: "configured servers preserve credentials",
			input: []ICEServer{
				{},
				{URLs: []string{"stun:one.example", "turn:two.example"}, Username: "user", Credential: "secret"},
			},
			expected: []webrtc.ICEServer{{
				URLs:       []string{"stun:one.example", "turn:two.example"},
				Username:   "user",
				Credential: "secret",
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pionICEServers(test.input); !reflect.DeepEqual(got, test.expected) {
				t.Fatalf("pionICEServers() = %+v, want %+v", got, test.expected)
			}
		})
	}
}

func TestFlattenedHeadersPreservesAllowlistedValues(t *testing.T) {
	t.Parallel()

	header := make(http.Header)
	header.Add("Content-Type", "video/mp4")
	header.Add("Content-Range", "bytes 0-3/4")
	header.Add("ETag", " first ")
	header.Add("ETag", "")
	header.Add("ETag", "second")
	header.Add("X-Private", "do-not-forward")

	want := []mediaHeader{
		{Name: "Content-Type", Value: "video/mp4"},
		{Name: "Content-Range", Value: "bytes 0-3/4"},
		{Name: "ETag", Value: " first "},
		{Name: "ETag", Value: "second"},
	}
	if got := flattenedHeaders(header); !reflect.DeepEqual(got, want) {
		t.Fatalf("flattenedHeaders() = %+v, want %+v", got, want)
	}
	if got := flattenedHeaders(nil); len(got) != 0 {
		t.Fatalf("flattenedHeaders(nil) = %+v, want empty", got)
	}
}

func TestSessionRequestLifecycle(t *testing.T) {
	t.Parallel()

	active := newTestSession("rtc-lifecycle")
	firstCtx, unregisterFirst := active.registerMediaRequest("request-1")
	secondCtx, unregisterSecond := active.registerMediaRequest("request-1")
	assertContextCanceled(t, firstCtx)

	unregisterFirst()
	if len(active.requests) != 1 {
		t.Fatalf("request count after stale unregister = %d, want 1", len(active.requests))
	}
	assertContextActive(t, secondCtx)

	if !active.cancelMediaRequest("  request-1  ", "test") {
		t.Fatal("cancelMediaRequest(existing) = false")
	}
	assertContextCanceled(t, secondCtx)
	if len(active.requests) != 0 {
		t.Fatalf("request count after cancel = %d, want 0", len(active.requests))
	}
	unregisterSecond()

	if active.cancelMediaRequest("", "blank") {
		t.Fatal("cancelMediaRequest(blank) = true")
	}
	if active.cancelMediaRequest("unknown", "missing") {
		t.Fatal("cancelMediaRequest(unknown) = true")
	}

	thirdCtx, unregisterThird := active.registerMediaRequest("request-3")
	fourthCtx, unregisterFourth := active.registerMediaRequest("request-4")
	active.cancelAllMediaRequests("peer_closed")
	assertContextCanceled(t, thirdCtx)
	assertContextCanceled(t, fourthCtx)
	if len(active.requests) != 0 {
		t.Fatalf("request count after cancelAll = %d, want 0", len(active.requests))
	}
	unregisterThird()
	unregisterFourth()

	active.cancelAllMediaRequests("already_empty")
}

func TestSessionWaitForSendBudget(t *testing.T) {
	t.Parallel()

	active := newTestSession("rtc-budget")
	t.Run("exact threshold is allowed", func(t *testing.T) {
		channel := &fakeDataChannel{bufferedDefault: maxDataChannelBufferedAmount}
		if err := active.waitForSendBudget(context.Background(), channel); err != nil {
			t.Fatalf("waitForSendBudget(exact threshold) error = %v", err)
		}
	})

	t.Run("waits until buffer falls", func(t *testing.T) {
		channel := &fakeDataChannel{bufferedAmounts: []uint64{
			maxDataChannelBufferedAmount + 1,
			maxDataChannelBufferedAmount + 1,
			0,
		}}
		if err := active.waitForSendBudget(context.Background(), channel); err != nil {
			t.Fatalf("waitForSendBudget() error = %v", err)
		}
		if calls := channel.bufferedCallCount(); calls != 3 {
			t.Fatalf("BufferedAmount calls = %d, want 3", calls)
		}
	})

	t.Run("cancellation interrupts backpressure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		channel := &fakeDataChannel{bufferedDefault: maxDataChannelBufferedAmount + 1}
		if err := active.waitForSendBudget(ctx, channel); !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForSendBudget(canceled) error = %v, want %v", err, context.Canceled)
		}
	})
}

func TestSessionSendBinary(t *testing.T) {
	t.Parallel()

	active := newTestSession("rtc-send")
	t.Run("records metrics", func(t *testing.T) {
		channel := &fakeDataChannel{bufferedAmounts: []uint64{0, 0, 11, 22}}
		metrics, err := active.sendBinary(context.Background(), channel, []byte("payload"))
		if err != nil {
			t.Fatalf("sendBinary() error = %v", err)
		}
		if metrics.bufferedBefore != 11 || metrics.bufferedAfter != 22 || metrics.duration < 0 {
			t.Fatalf("send metrics = %+v, want buffered 11/22 and non-negative duration", metrics)
		}
		events := channel.snapshotEvents()
		if len(events) != 1 || events[0].kind != "binary" || string(events[0].binary) != "payload" {
			t.Fatalf("events = %+v, want one binary payload", events)
		}
	})

	t.Run("canceled before send", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		channel := &fakeDataChannel{bufferedAmounts: []uint64{0, 0, 11, 22}}
		metrics, err := active.sendBinary(ctx, channel, []byte("payload"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sendBinary(canceled) error = %v, want %v", err, context.Canceled)
		}
		if metrics.bufferedBefore != 11 || metrics.bufferedAfter != 22 {
			t.Fatalf("send metrics = %+v, want buffered 11/22", metrics)
		}
		if events := channel.snapshotEvents(); len(events) != 0 {
			t.Fatalf("events after canceled send = %+v, want none", events)
		}
	})

	t.Run("returns channel error", func(t *testing.T) {
		channelErr := errors.New("send failed")
		channel := &fakeDataChannel{sendErr: channelErr}
		if _, err := active.sendBinary(context.Background(), channel, []byte("payload")); !errors.Is(err, channelErr) {
			t.Fatalf("sendBinary(channel error) = %v, want %v", err, channelErr)
		}
	})
}

func TestHandleMediaRequestRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	loaderCalls := 0
	manager := NewManager(func(*http.Request, string) (*catalog.UpstreamMediaResponse, error) {
		loaderCalls++
		return nil, errors.New("unexpected loader call")
	})
	tests := []struct {
		name    string
		request mediaRequest
	}{
		{name: "missing request ID", request: mediaRequest{AssetID: "asset-1"}},
		{name: "missing asset ID", request: mediaRequest{RequestID: "request-1"}},
		{name: "unsupported method", request: mediaRequest{RequestID: "request-1", AssetID: "asset-1", Method: http.MethodDelete}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			active := newTestSession("rtc-invalid")
			channel := &fakeDataChannel{}

			manager.handleMediaRequest(active, channel, test.request)

			events := channel.snapshotEvents()
			if len(events) != 1 || events[0].kind != "text" {
				t.Fatalf("events = %+v, want one JSON error", events)
			}
			response := decodeJSONEvent[mediaError](t, events[0])
			if response.Type != "error" || response.Code != "invalid_request" {
				t.Fatalf("response = %+v, want invalid_request error", response)
			}
			if len(active.requests) != 0 {
				t.Fatalf("active requests = %d, want 0", len(active.requests))
			}
		})
	}
	if loaderCalls != 0 {
		t.Fatalf("original loader calls = %d, want 0", loaderCalls)
	}
}

func TestHandleMediaRequestStreamsGETResponse(t *testing.T) {
	t.Parallel()

	const requestID = "11111111-1111-1111-1111-111111111111"
	body := &trackingReadCloser{reader: strings.NewReader("data")}
	manager := NewManager(func(request *http.Request, assetID string) (*catalog.UpstreamMediaResponse, error) {
		if request.Method != http.MethodGet {
			t.Errorf("upstream method = %q, want GET", request.Method)
		}
		if assetID != "asset/one" {
			t.Errorf("assetID = %q, want asset/one", assetID)
		}
		if got := request.URL.EscapedPath(); got != "/v1/assets/asset%2Fone/original" {
			t.Errorf("upstream escaped path = %q, want escaped asset ID", got)
		}
		if got := request.Header.Get("Range"); got != "bytes=0-3" {
			t.Errorf("Range = %q, want bytes=0-3", got)
		}
		if got := request.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		return &catalog.UpstreamMediaResponse{
			StatusCode: http.StatusPartialContent,
			Header: http.Header{
				"Content-Type":   []string{"video/mp4"},
				"Content-Length": []string{"4"},
				"Content-Range":  []string{"bytes 0-3/4"},
				"X-Private":      []string{"hidden"},
			},
			Body: body,
		}, nil
	})
	active := newTestSession("rtc-get")
	channel := &fakeDataChannel{}

	manager.handleMediaRequest(active, channel, mediaRequest{
		Type:      "media_request",
		RequestID: requestID,
		AssetID:   "asset/one",
		Range:     " bytes=0-3 ",
	})

	events := channel.snapshotEvents()
	if got := eventKinds(events); !reflect.DeepEqual(got, []string{"text", "binary", "text"}) {
		t.Fatalf("event kinds = %+v, want headers/binary/complete", got)
	}
	headers := decodeJSONEvent[mediaHeaders](t, events[0])
	if headers.Type != "headers" || headers.RequestID != requestID || headers.StatusCode != http.StatusPartialContent || headers.ContentType != "video/mp4" {
		t.Fatalf("headers = %+v, want partial-content metadata", headers)
	}
	if got := headerValues(headers.Headers, "X-Private"); len(got) != 0 {
		t.Fatalf("forwarded X-Private values = %+v, want none", got)
	}
	if got := headerValues(headers.Headers, "Content-Range"); !reflect.DeepEqual(got, []string{"bytes 0-3/4"}) {
		t.Fatalf("Content-Range values = %+v, want range", got)
	}

	binary := events[1].binary
	if len(binary) != 1+36+4 || binary[0] != binaryChunkKind || string(binary[1:37]) != requestID || string(binary[37:]) != "data" {
		t.Fatalf("binary frame = kind %q id %q body %q len %d", binary[0], binary[1:37], binary[37:], len(binary))
	}
	complete := decodeJSONEvent[mediaComplete](t, events[2])
	if complete.Type != "complete" || complete.RequestID != requestID || complete.Bytes != 4 || complete.DurationMS < 0 {
		t.Fatalf("complete = %+v, want four-byte completion", complete)
	}
	if !body.closed {
		t.Fatal("upstream body was not closed")
	}
	if len(active.requests) != 0 {
		t.Fatalf("active requests after completion = %d, want 0", len(active.requests))
	}
}

func TestHandleMediaRequestHEADSkipsBody(t *testing.T) {
	t.Parallel()

	const requestID = "22222222-2222-2222-2222-222222222222"
	body := &trackingReadCloser{reader: strings.NewReader("must-not-read")}
	manager := NewManager(func(request *http.Request, _ string) (*catalog.UpstreamMediaResponse, error) {
		if request.Method != http.MethodHead {
			t.Errorf("upstream method = %q, want HEAD", request.Method)
		}
		return &catalog.UpstreamMediaResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Length": []string{"13"}},
			Body:       body,
		}, nil
	})
	active := newTestSession("rtc-head")
	channel := &fakeDataChannel{}

	manager.handleMediaRequest(active, channel, mediaRequest{
		RequestID: requestID,
		AssetID:   "asset-2",
		Method:    " head ",
	})

	events := channel.snapshotEvents()
	if got := eventKinds(events); !reflect.DeepEqual(got, []string{"text", "text"}) {
		t.Fatalf("event kinds = %+v, want headers/complete", got)
	}
	complete := decodeJSONEvent[mediaComplete](t, events[1])
	if complete.Bytes != 0 || complete.RequestID != requestID {
		t.Fatalf("complete = %+v, want zero-byte HEAD completion", complete)
	}
	if body.readCalls != 0 || !body.closed {
		t.Fatalf("HEAD body reads/closed = %d/%t, want 0/true", body.readCalls, body.closed)
	}
}

func TestHandleMediaRequestReportsUpstreamAndReadErrors(t *testing.T) {
	t.Parallel()

	t.Run("upstream failure", func(t *testing.T) {
		manager := NewManager(func(*http.Request, string) (*catalog.UpstreamMediaResponse, error) {
			return nil, errors.New("upstream unavailable")
		})
		active := newTestSession("rtc-upstream-error")
		channel := &fakeDataChannel{}

		manager.handleMediaRequest(active, channel, mediaRequest{RequestID: "request-upstream", AssetID: "asset-1"})

		events := channel.snapshotEvents()
		if len(events) != 1 {
			t.Fatalf("events = %+v, want one error", events)
		}
		response := decodeJSONEvent[mediaError](t, events[0])
		if response.Code != "upstream_failed" || response.RequestID != "request-upstream" {
			t.Fatalf("response = %+v, want upstream_failed", response)
		}
	})

	t.Run("body read failure", func(t *testing.T) {
		readErr := errors.New("read failed")
		body := &chunkErrorReadCloser{chunk: []byte("part"), err: readErr}
		manager := NewManager(func(*http.Request, string) (*catalog.UpstreamMediaResponse, error) {
			return &catalog.UpstreamMediaResponse{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		})
		active := newTestSession("rtc-read-error")
		channel := &fakeDataChannel{}

		manager.handleMediaRequest(active, channel, mediaRequest{
			RequestID: "33333333-3333-3333-3333-333333333333",
			AssetID:   "asset-3",
		})

		events := channel.snapshotEvents()
		if got := eventKinds(events); !reflect.DeepEqual(got, []string{"text", "binary", "text"}) {
			t.Fatalf("event kinds = %+v, want headers/chunk/error", got)
		}
		response := decodeJSONEvent[mediaError](t, events[2])
		if response.Code != "read_failed" {
			t.Fatalf("response = %+v, want read_failed", response)
		}
		if !body.closed {
			t.Fatal("failed upstream body was not closed")
		}
	})
}

func TestHandleMediaRequestStopsAfterSendFailure(t *testing.T) {
	t.Parallel()

	sendErr := errors.New("channel send failed")
	body := &trackingReadCloser{reader: strings.NewReader("data")}
	manager := NewManager(func(*http.Request, string) (*catalog.UpstreamMediaResponse, error) {
		return &catalog.UpstreamMediaResponse{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	active := newTestSession("rtc-send-error")
	channel := &fakeDataChannel{sendErr: sendErr}

	manager.handleMediaRequest(active, channel, mediaRequest{
		RequestID: "44444444-4444-4444-4444-444444444444",
		AssetID:   "asset-4",
	})

	if got := eventKinds(channel.snapshotEvents()); !reflect.DeepEqual(got, []string{"text", "binary"}) {
		t.Fatalf("event kinds = %+v, want headers and failed binary attempt", got)
	}
	if !body.closed {
		t.Fatal("upstream body was not closed after send failure")
	}
	if len(active.requests) != 0 {
		t.Fatalf("active requests after send failure = %d, want 0", len(active.requests))
	}
}

func TestHandleMediaRequestCancellationStopsUpstream(t *testing.T) {
	t.Parallel()

	const requestID = "request-cancel-upstream"
	requestStarted := make(chan *http.Request, 1)
	manager := NewManager(func(request *http.Request, _ string) (*catalog.UpstreamMediaResponse, error) {
		requestStarted <- request
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	active := newTestSession("rtc-cancel-upstream")
	channel := &fakeDataChannel{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.handleMediaRequest(active, channel, mediaRequest{RequestID: requestID, AssetID: "asset-5"})
	}()

	proxyRequest := <-requestStarted
	if !active.cancelMediaRequest(requestID, "client_cancel") {
		t.Fatal("cancelMediaRequest() = false")
	}
	<-done

	if !errors.Is(proxyRequest.Context().Err(), context.Canceled) {
		t.Fatalf("proxy context error = %v, want %v", proxyRequest.Context().Err(), context.Canceled)
	}
	if events := channel.snapshotEvents(); len(events) != 0 {
		t.Fatalf("events after upstream cancellation = %+v, want none", events)
	}
}

func TestHandleMediaRequestCancellationInterruptsBackpressure(t *testing.T) {
	t.Parallel()

	const requestID = "request-cancel-budget"
	body := &trackingReadCloser{reader: strings.NewReader("data")}
	manager := NewManager(func(*http.Request, string) (*catalog.UpstreamMediaResponse, error) {
		return &catalog.UpstreamMediaResponse{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	active := newTestSession("rtc-cancel-budget")
	textSent := make(chan struct{}, 1)
	channel := &fakeDataChannel{
		bufferedDefault: maxDataChannelBufferedAmount + 1,
		textSent:        textSent,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.handleMediaRequest(active, channel, mediaRequest{RequestID: requestID, AssetID: "asset-6"})
	}()

	<-textSent
	if !active.cancelMediaRequest(requestID, "client_cancel") {
		t.Fatal("cancelMediaRequest() = false")
	}
	<-done

	if got := eventKinds(channel.snapshotEvents()); !reflect.DeepEqual(got, []string{"text"}) {
		t.Fatalf("event kinds = %+v, want headers only", got)
	}
	if !body.closed {
		t.Fatal("upstream body was not closed after backpressure cancellation")
	}
}

func TestSessionSendJSONIgnoresMarshalAndChannelErrors(t *testing.T) {
	t.Parallel()

	active := newTestSession("rtc-json")
	channel := &fakeDataChannel{}
	active.sendJSON(channel, make(chan int))
	if events := channel.snapshotEvents(); len(events) != 0 {
		t.Fatalf("events after marshal error = %+v, want none", events)
	}

	channel.sendTextErr = errors.New("text send failed")
	active.sendJSON(channel, mediaEnvelope{Type: "test"})
	if events := channel.snapshotEvents(); len(events) != 1 || events[0].kind != "text" {
		t.Fatalf("events after text send error = %+v, want attempted text send", events)
	}
}

type fakeDataChannelEvent struct {
	kind   string
	text   string
	binary []byte
}

type fakeDataChannel struct {
	mu              sync.Mutex
	events          []fakeDataChannelEvent
	bufferedAmounts []uint64
	bufferedDefault uint64
	bufferedCalls   int
	sendTextErr     error
	sendErr         error
	textSent        chan struct{}
}

func (f *fakeDataChannel) SendText(body string) error {
	f.mu.Lock()
	f.events = append(f.events, fakeDataChannelEvent{kind: "text", text: body})
	err := f.sendTextErr
	notify := f.textSent
	f.mu.Unlock()
	if notify != nil {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	return err
}

func (f *fakeDataChannel) Send(body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fakeDataChannelEvent{kind: "binary", binary: append([]byte(nil), body...)})
	return f.sendErr
}

func (f *fakeDataChannel) BufferedAmount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.bufferedCalls
	f.bufferedCalls++
	if index < len(f.bufferedAmounts) {
		return f.bufferedAmounts[index]
	}
	return f.bufferedDefault
}

func (f *fakeDataChannel) snapshotEvents() []fakeDataChannelEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]fakeDataChannelEvent, len(f.events))
	for index, event := range f.events {
		result[index] = event
		result[index].binary = append([]byte(nil), event.binary...)
	}
	return result
}

func (f *fakeDataChannel) bufferedCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bufferedCalls
}

type trackingReadCloser struct {
	reader    io.Reader
	readCalls int
	closed    bool
}

func (r *trackingReadCloser) Read(payload []byte) (int, error) {
	r.readCalls++
	return r.reader.Read(payload)
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type chunkErrorReadCloser struct {
	chunk  []byte
	err    error
	read   bool
	closed bool
}

func (r *chunkErrorReadCloser) Read(payload []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	return copy(payload, r.chunk), r.err
}

func (r *chunkErrorReadCloser) Close() error {
	r.closed = true
	return nil
}

func newTestSession(id string) *session {
	return &session{
		id:       id,
		done:     make(chan struct{}),
		requests: make(map[string]*activeMediaRequest),
	}
}

func assertContextCanceled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want %v", ctx.Err(), context.Canceled)
		}
	default:
		t.Fatal("context is not canceled")
	}
}

func assertContextActive(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("context unexpectedly done: %v", ctx.Err())
	default:
	}
}

func decodeJSONEvent[T any](t *testing.T, event fakeDataChannelEvent) T {
	t.Helper()
	var payload T
	if event.kind != "text" {
		t.Fatalf("event kind = %q, want text", event.kind)
	}
	if err := json.Unmarshal([]byte(event.text), &payload); err != nil {
		t.Fatalf("decode channel JSON: %v body=%q", err, event.text)
	}
	return payload
}

func eventKinds(events []fakeDataChannelEvent) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, event.kind)
	}
	return result
}

func headerValues(headers []mediaHeader, name string) []string {
	var result []string
	for _, header := range headers {
		if header.Name == name {
			result = append(result, header.Value)
		}
	}
	return result
}

func managerSessionCount(manager *Manager) int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return len(manager.sessions)
}
