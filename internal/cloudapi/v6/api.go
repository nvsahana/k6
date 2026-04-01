package cloudapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	k6cloud "github.com/grafana/k6-cloud-openapi-client-go/k6"
	"go.k6.io/k6/lib"
)

// ValidateOptions sends the provided options to the cloud for validation.
func (c *Client) ValidateOptions(ctx context.Context, projectID int64, options lib.Options) (err error) {
	raw, rerr := json.Marshal(options)
	if rerr != nil {
		return fmt.Errorf("marshaling options: %w", rerr)
	}
	var generic map[string]any
	if rerr := json.Unmarshal(raw, &generic); rerr != nil {
		return fmt.Errorf("unmarshaling options: %w", rerr)
	}

	projectID32, rerr := toInt32(projectID)
	if rerr != nil {
		return fmt.Errorf("converting project ID: %w", rerr)
	}

	validateOptions := &k6cloud.ValidateOptionsRequest{
		ProjectId: *k6cloud.NewNullableInt32(&projectID32),
		Options: k6cloud.Options{
			AdditionalProperties: generic,
		},
	}

	req := c.apiClient.LoadTestsAPI.
		ValidateOptions(c.authCtx(ctx)).
		ValidateOptionsRequest(validateOptions).
		XStackId(c.stackID)
	_, httpRes, rerr := req.Execute()
	defer closeResponse(httpRes, &err)

	return CheckResponse(httpRes, rerr)
}

// ValidateToken calls the endpoint to validate the Client's token and returns the result.
func (c *Client) ValidateToken(ctx context.Context, stackURL string) (_ *k6cloud.AuthenticationResponse, err error) {
	if stackURL == "" {
		return nil, errors.New("stack URL is required to validate token")
	}

	if _, err := url.Parse(stackURL); err != nil {
		return nil, fmt.Errorf("invalid stack URL: %w", err)
	}

	req := c.apiClient.AuthorizationAPI.
		Auth(c.authCtx(ctx)).
		XStackUrl(stackURL)

	resp, httpRes, rerr := req.Execute()
	defer closeResponse(httpRes, &err)
	if err := CheckResponse(httpRes, rerr); err != nil {
		return nil, fmt.Errorf("validating token: %w", err)
	}

	return resp, nil
}
