package provider

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"go.mau.fi/util/retryafter"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Client struct {
	MX *mautrix.Client
}

func clientFromResource(req resource.ConfigureRequest) (*Client, error) {
	if req.ProviderData == nil {
		return nil, nil
	}
	c, ok := req.ProviderData.(*Client)
	if !ok {
		return nil, errors.New("unexpected ProviderData type: expected *provider.Client")
	}
	return c, nil
}

func clientFromDataSource(req datasource.ConfigureRequest) (*Client, error) {
	if req.ProviderData == nil {
		return nil, nil
	}
	c, ok := req.ProviderData.(*Client)
	if !ok {
		return nil, errors.New("unexpected ProviderData type: expected *provider.Client")
	}
	return c, nil
}

// notFoundErr reports whether err is a homeserver 404 or M_NOT_FOUND. Both
// spellings occur, so check for each.
func notFoundErr(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_NOT_FOUND" {
		return true
	}
	return httpErr.Response != nil && httpErr.Response.StatusCode == 404
}

// ambiguousErr reports whether err leaves it unknown whether the homeserver did
// the work.
//
// Only one answer is definite: a 4xx, where the homeserver read the request and
// refused it. Everything else leaves the outcome unknown, and only then is it
// unsafe to simply try again. See issue #55.
//
// The rule is structural rather than a list of errcodes, so it does not depend
// on when a given homeserver validates what.
//
// A homeserver could in principle do the work and then fail a later step with a
// 4xx. Nothing rules that out, so this is the best available reading of an
// error, not a proof.
func ambiguousErr(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		// A transport failure, a timeout, or anything else that never reached
		// a server.
		return true
	}
	if httpErr.Response == nil {
		// mautrix reports a request that got no response this way, and also a
		// request it could not build or marshal.
		return true
	}
	// Anything outside 4xx: a 5xx, where a gateway gave up and cannot say what
	// the homeserver behind it did, or where the homeserver failed after it may
	// have committed the room. Also a 2xx whose body mautrix could not read or
	// parse, which it reports with the successful response attached. The room
	// exists in that case and nothing knows its id, which is the worst outcome
	// of all to stay quiet about.
	status := httpErr.Response.StatusCode
	return status < 400 || status >= 500
}

// rateLimitFallback is the wait used when a homeserver says it is rate limiting
// but gives no hint about how long. rateLimitCap bounds the wait, so a broken or
// hostile Retry-After cannot hang an apply.
const (
	rateLimitFallback = 2 * time.Second
	rateLimitCap      = 30 * time.Second
)

// rateLimitWait reports how long to wait when err is a homeserver rate limit,
// and whether it is one at all.
//
// A rate limit is recognised by errcode or by status, the same two-source shape
// notFoundErr uses, because neither alone is reliable across homeservers.
func rateLimitWait(err error) (time.Duration, bool) {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		return 0, false
	}
	byCode := httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_LIMIT_EXCEEDED"
	byStatus := httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusTooManyRequests
	if !byCode && !byStatus {
		return 0, false
	}

	wait := rateLimitFallback
	// Synapse puts the delay in the body; the header is the specced place.
	if httpErr.RespError != nil {
		if ms, ok := httpErr.RespError.ExtraData["retry_after_ms"].(float64); ok && ms > 0 {
			wait = time.Duration(ms) * time.Millisecond
		}
	}
	if httpErr.Response != nil {
		if header := httpErr.Response.Header.Get("Retry-After"); header != "" {
			wait = retryafter.Parse(header, wait)
		}
	}
	if wait < 0 {
		// An HTTP-date already in the past.
		wait = 0
	}
	if wait > rateLimitCap {
		wait = rateLimitCap
	}
	return wait, true
}

// retryOnRateLimit runs a request that must not be repeated after an ambiguous
// failure, retrying only when the homeserver refused it outright.
//
// mautrix retries transport errors and gateway responses as well as rate
// limits. That is right for every idempotent call this provider makes, and
// wrong for one that is not: a timeout or a 502 leaves it unknown whether the
// homeserver did the work, so repeating it can do the work twice. A 429 is
// different, because the homeserver refused before doing anything. See #51.
//
// The context handed to fn disables mautrix's own retrying.
func retryOnRateLimit[T any](ctx context.Context, attempts int, fn func(context.Context) (T, error)) (T, error) {
	noRetry := mautrix.WithMaxRetries(ctx, 0)
	var zero T
	for attempt := 0; ; attempt++ {
		out, err := fn(noRetry)
		if err == nil || attempt >= attempts {
			return out, err
		}
		wait, limited := rateLimitWait(err)
		if !limited {
			return zero, err
		}
		select {
		case <-ctx.Done():
			// No request is in flight during the wait, and the attempt before
			// it was a refusal, so nothing was done. Carry that refusal out
			// with the cancellation, or a caller reads a bare context error as
			// an unknown outcome. See issue #55.
			return zero, errors.Join(ctx.Err(), err)
		case <-time.After(wait):
		}
	}
}

// destroyHint is appended to every failed-destroy error. Without it a
// practitioner is told the destroy failed and not how to get unstuck.
const destroyHint = " The resource stays in state. Fix the cause and run destroy" +
	" again, or use `terraform state rm` to drop it without touching the homeserver."

// failedDestroy records a destroy that could not do its work.
//
// A homeserver that no longer holds the thing being removed is success: there is
// nothing left to do, so a 404 is ignored. Everything else is a refusal, and
// reporting one as a warning lets a destroy claim it removed something it never
// did, with exit status 0. See issue #45.
func failedDestroy(diags *diag.Diagnostics, summary string, err error) {
	if err == nil || notFoundErr(err) {
		return
	}
	diags.AddError(summary, err.Error()+destroyHint)
}

// getState fetches a state event and unmarshals it into out. Returns (found, error).
// A 404 from the homeserver is treated as found=false, no error.
func getState(ctx context.Context, c *Client, roomID id.RoomID, evtType event.Type, stateKey string, out any) (bool, error) {
	err := c.MX.StateEvent(ctx, roomID, evtType, stateKey, out)
	if err == nil {
		return true, nil
	}
	if notFoundErr(err) {
		return false, nil
	}
	return false, err
}

// getCreateContent fetches the content of m.room.create, which carries the room
// version and the room type.
//
// Content only, through the specced state endpoint. Anything needing the create
// event's sender has to use FullStateEvent, which depends on a ?format=event
// query parameter that is not in the spec.
func getCreateContent(ctx context.Context, c *Client, roomID id.RoomID) (*event.CreateEventContent, bool, error) {
	var create event.CreateEventContent
	found, err := getState(ctx, c, roomID, event.StateCreate, "", &create)
	if err != nil || !found {
		return nil, false, err
	}
	return &create, true, nil
}

// roomVisibility is the body of /_matrix/client/v3/directory/list/room/{roomID},
// in both directions.
type roomVisibility struct {
	Visibility string `json:"visibility"`
}

// getRoomVisibility reads a room's entry in the public room directory. mautrix
// has no wrapper for this endpoint, hence the raw request. A room the directory
// does not know gives found=false rather than an error.
func getRoomVisibility(ctx context.Context, c *Client, roomID id.RoomID) (string, bool, error) {
	var out roomVisibility
	url := c.MX.BuildClientURL("v3", "directory", "list", "room", roomID)
	if _, err := c.MX.MakeRequest(ctx, http.MethodGet, url, nil, &out); err != nil {
		if notFoundErr(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return out.Visibility, true, nil
}

// setRoomVisibility publishes or unpublishes a room in the directory. A
// homeserver may refuse, or accept and not honour it, depending on its
// room_list_publication_rules; callers must read the value back to find out.
func setRoomVisibility(ctx context.Context, c *Client, roomID id.RoomID, visibility string) error {
	url := c.MX.BuildClientURL("v3", "directory", "list", "room", roomID)
	_, err := c.MX.MakeRequest(ctx, http.MethodPut, url, &roomVisibility{Visibility: visibility}, nil)
	return err
}

// sendState is a thin wrapper to reduce boilerplate.
func sendState(ctx context.Context, c *Client, roomID id.RoomID, evtType event.Type, stateKey string, content any) error {
	_, err := c.MX.SendStateEvent(ctx, roomID, evtType, stateKey, content)
	return err
}
