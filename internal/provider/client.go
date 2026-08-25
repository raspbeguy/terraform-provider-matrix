package provider

import (
	"context"
	"errors"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"

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
