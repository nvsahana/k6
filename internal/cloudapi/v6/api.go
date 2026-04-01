package cloudapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"time"

	k6cloud "github.com/grafana/k6-cloud-openapi-client-go/k6"
	"go.k6.io/k6/lib"
)

// V6 test run statuses per the OpenAPI spec (StatusApiModel).
const (
	StatusCreated           = "created"
	StatusQueued            = "queued"
	StatusInitializing      = "initializing"
	StatusRunning           = "running"
	StatusProcessingMetrics = "processing_metrics"
	StatusCompleted         = "completed"
	StatusAborted           = "aborted"

	ResultFailed = "failed"
	ResultError  = "error"
)

// TestRunProgress maps a subset of the v6 test run response to track
// execution progress.
type TestRunProgress struct {
	Status            string
	Result            string
	EstimatedDuration int32
	ExecutionDuration int32
}

// IsTerminal reports whether the test run status is a terminal state.
func (p TestRunProgress) IsTerminal() bool {
	switch p.Status {
	case StatusCompleted, StatusAborted:
		return true
	default:
		return false
	}
}

// Progress computes execution_duration / estimated_duration,
// clamped to [0, 1]. Returns 0 if estimated_duration is zero or negative.
func (p TestRunProgress) Progress() float64 {
	if p.EstimatedDuration <= 0 || p.ExecutionDuration < 0 {
		return 0
	}
	return math.Min(float64(p.ExecutionDuration)/float64(p.EstimatedDuration), 1.0)
}

// FetchTestRun calls GET /cloud/v6/test_runs/{id} and returns the test run progress.
// Transient 502/503 errors are retried up to MaxRetries times.
func (c *Client) FetchTestRun(ctx context.Context, testRunID int64) (_ *TestRunProgress, err error) {
	testRunID32, err := toInt32(testRunID)
	if err != nil {
		return nil, fmt.Errorf("converting test run ID: %w", err)
	}

	var lastErr error
	for attempt := range c.retries + 1 {
		progress, status, fetchErr := c.fetchTestRunOnce(ctx, testRunID32)
		if fetchErr == nil {
			return progress, nil
		}
		if status != http.StatusBadGateway && status != http.StatusServiceUnavailable {
			return nil, fetchErr
		}
		lastErr = fetchErr
		if attempt < c.retries {
			c.logger.WithField("attempt", attempt+1).
				Warnf("Transient %d error fetching test run, retrying...", status)
			retryTimer := time.NewTimer(c.retryInterval)
			select {
			case <-ctx.Done():
				retryTimer.Stop()
				return nil, fmt.Errorf("fetching test run: %w", ctx.Err())
			case <-retryTimer.C:
			}
		}
	}
	return nil, lastErr
}

func (c *Client) fetchTestRunOnce(
	ctx context.Context, testRunID int32,
) (_ *TestRunProgress, status int, err error) {
	req := c.apiClient.TestRunsAPI.
		TestRunsRetrieve(c.authCtx(ctx), testRunID).
		XStackId(c.stackID)

	resp, res, rerr := req.Execute()
	defer closeResponse(res, &err)

	if res != nil {
		status = res.StatusCode
	}

	if err := CheckResponse(res, rerr); err != nil {
		return nil, status, err
	}

	progress := &TestRunProgress{
		Status:            resp.Status,
		ExecutionDuration: resp.ExecutionDuration,
	}
	if resp.Result.IsSet() && resp.Result.Get() != nil {
		progress.Result = *resp.Result.Get()
	}
	if resp.EstimatedDuration.IsSet() && resp.EstimatedDuration.Get() != nil {
		progress.EstimatedDuration = *resp.EstimatedDuration.Get()
	}

	return progress, status, nil
}

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

	validateOptions := k6cloud.NewValidateOptionsRequest(k6cloud.Options{
		AdditionalProperties: generic,
	})
	validateOptions.ProjectId = *k6cloud.NewNullableInt32(&projectID32)

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

// randomStrHex returns a hex string which can be used
// for session token id or idempotency key.
func randomStrHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartCloudTestRun starts a new cloud test run for a given test.
func (c *Client) StartCloudTestRun(
	ctx context.Context, loadTestID int32,
) (ltr *k6cloud.StartLoadTestResponse, err error) {
	req := c.apiClient.LoadTestsAPI.LoadTestsStart(c.authCtx(ctx), loadTestID).
		XStackId(c.stackID).
		K6IdempotencyKey(randomStrHex())

	loadTestRun, res, rerr := req.Execute()
	defer closeResponse(res, &err)
	if err := CheckResponse(res, rerr); err != nil {
		return nil, fmt.Errorf("starting cloud test run: %w", err)
	}

	return loadTestRun, nil
}

// CreateAndStartCloudTestRun creates a new cloud test (or updates it if it already exists) and starts a new test run.
func (c *Client) CreateAndStartCloudTestRun(
	ctx context.Context, name string, projectID int64, arc *lib.Archive,
) (*k6cloud.StartLoadTestResponse, error) {
	loadTest, err := c.CreateOrUpdateCloudTest(ctx, name, projectID, arc)
	if err != nil {
		return nil, err
	}

	return c.StartCloudTestRun(ctx, loadTest.Id)
}

// StopCloudTestRun tells the cloud to stop the test with the provided testRunID.
func (c *Client) StopCloudTestRun(ctx context.Context, testRunID int64) (err error) {
	testRunID32, err := toInt32(testRunID)
	if err != nil {
		return fmt.Errorf("converting test run ID: %w", err)
	}

	req := c.apiClient.TestRunsAPI.TestRunsAbort(c.authCtx(ctx), testRunID32).XStackId(c.stackID)
	res, rerr := req.Execute()
	defer closeResponse(res, &err)
	if err := CheckResponse(res, rerr); err != nil {
		return fmt.Errorf("stopping cloud test run: %w", err)
	}
	return nil
}
