package provider

import (
	"context"
	"errors"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
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

// getState fetches a state event and unmarshals it into out. Returns (found, error).
// A 404 from the homeserver is treated as found=false, no error.
func getState(ctx context.Context, c *Client, roomID id.RoomID, evtType event.Type, stateKey string, out any) (bool, error) {
	err := c.MX.StateEvent(ctx, roomID, evtType, stateKey, out)
	if err == nil {
		return true, nil
	}
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_NOT_FOUND" {
			return false, nil
		}
		if httpErr.Response != nil && httpErr.Response.StatusCode == 404 {
			return false, nil
		}
	}
	return false, err
}

// sendState is a thin wrapper to reduce boilerplate.
func sendState(ctx context.Context, c *Client, roomID id.RoomID, evtType event.Type, stateKey string, content any) error {
	_, err := c.MX.SendStateEvent(ctx, roomID, evtType, stateKey, content)
	return err
}

