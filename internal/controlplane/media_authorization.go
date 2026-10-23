package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	contracts "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Client) mediaAuthority() string {
	if c.hostedBaseURL == "" {
		return ""
	}
	return c.target + "|" + c.hostedBaseURL
}

func (c *Client) checkMediaAuthorization(ctx context.Context, authority, device string, used bool) (time.Duration, error) {
	if authority == "" || authority != c.mediaAuthority() {
		return 0, errors.New("Reach authority mismatch")
	}
	c.leaseMu.RLock()
	conn := c.leaseConn
	c.leaseMu.RUnlock()
	if conn == nil {
		return 0, errors.New("Reach not connected")
	}
	request, err := contracts.NewMessage("MediaAuthorizationRequest")
	if err != nil {
		return 0, err
	}
	nonce := uuid.NewString()
	request.Set(contracts.FieldByName(request, "device_id"), protoreflect.ValueOfString(device))
	request.Set(contracts.FieldByName(request, "nonce"), protoreflect.ValueOfString(nonce))
	request.Set(contracts.FieldByName(request, "used"), protoreflect.ValueOfBool(used))
	response, err := contracts.NewMessage("MediaAuthorizationResponse")
	if err != nil {
		return 0, err
	}
	if err := conn.Invoke(ctx, contracts.CheckMediaAuthorizationFullMethod(), request, response); err != nil {
		return 0, err
	}
	if contracts.StringFieldFromReflect(response, "nonce") != nonce {
		return 0, errors.New("Reach response mismatch")
	}
	switch contracts.StringFieldFromReflect(response, "decision") {
	case "denied":
		return 0, nil
	case "allowed":
		millis := contracts.Int64FieldFromReflect(response, "max_age_millis")
		if millis > 0 && millis <= contracts.MediaAuthorizationMaxAge.Milliseconds() {
			return time.Duration(millis) * time.Millisecond, nil
		}
	}
	return 0, errors.New("Reach authorization unavailable")
}
