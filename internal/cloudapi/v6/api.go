package cloudapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"

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

// CreateCloudTest creates a new cloud test with the provided name and script archive.
func (c *Client) CreateCloudTest(
	ctx context.Context, name string, projectID int64, arcData []byte,
) (lt *k6cloud.LoadTestApiModel, err error) {
	projectID32, err := toInt32(projectID)
	if err != nil {
		return nil, fmt.Errorf("converting project ID: %w", err)
	}

	req := c.apiClient.LoadTestsAPI.ProjectsLoadTestsCreate(c.authCtx(ctx), projectID32).
		Name(name).
		Script(io.NopCloser(bytes.NewReader(arcData))).
		XStackId(c.stackID)

	loadTest, res, rerr := req.Execute()
	defer closeResponse(res, &err)
	if err := CheckResponse(res, rerr); err != nil {
		return nil, fmt.Errorf("creating cloud test: %w", err)
	}

	return loadTest, nil
}

// updateCloudTest updates an existing cloud test with the provided script archive.
func (c *Client) updateCloudTest(ctx context.Context, testID int32, arcData []byte) (err error) {
	req := c.apiClient.LoadTestsAPI.LoadTestsScriptUpdate(c.authCtx(ctx), testID).
		Body(io.NopCloser(bytes.NewReader(arcData))).
		XStackId(c.stackID)

	res, rerr := req.Execute()
	defer closeResponse(res, &err)
	if err := CheckResponse(res, rerr); err != nil {
		return fmt.Errorf("updating cloud test script: %w", err)
	}
	return nil
}

// FetchCloudTestByName retrieves a cloud test by its name within the specified project.
func (c *Client) FetchCloudTestByName(
	ctx context.Context, name string, projectID int64,
) (lt *k6cloud.LoadTestApiModel, err error) {
	projectID32, err := toInt32(projectID)
	if err != nil {
		return nil, fmt.Errorf("converting project ID: %w", err)
	}

	req := c.apiClient.LoadTestsAPI.ProjectsLoadTestsRetrieve(c.authCtx(ctx), projectID32).
		XStackId(c.stackID).
		Name(name)

	loadTests, res, rerr := req.Execute()
	defer closeResponse(res, &err)
	if err := CheckResponse(res, rerr); err != nil {
		return nil, fmt.Errorf("fetching cloud test by name: %w", err)
	}

	idx := slices.IndexFunc(loadTests.Value, func(t k6cloud.LoadTestApiModel) bool {
		return t.Name == name
	})
	if idx < 0 {
		return nil, fmt.Errorf("load test %q not found in project", name)
	}
	return &loadTests.Value[idx], nil
}

// CreateOrUpdateCloudTest creates a new cloud test or updates an existing one
// if a test with the same name already exists.
func (c *Client) CreateOrUpdateCloudTest(
	ctx context.Context, name string, projectID int64, arc *lib.Archive,
) (*k6cloud.LoadTestApiModel, error) {
	var buf bytes.Buffer
	if err := arc.Write(&buf); err != nil {
		return nil, fmt.Errorf("writing archive: %w", err)
	}
	arcData := buf.Bytes()

	test, err := c.CreateCloudTest(ctx, name, projectID, arcData)
	if err != nil {
		var rErr ResponseError
		if !errors.As(err, &rErr) || rErr.Response.StatusCode != http.StatusConflict {
			return nil, err
		}

		test, err = c.FetchCloudTestByName(ctx, name, projectID)
		if err != nil {
			return nil, err
		}

		if err := c.updateCloudTest(ctx, test.Id, arcData); err != nil {
			return nil, err
		}
	}

	return test, nil
}
