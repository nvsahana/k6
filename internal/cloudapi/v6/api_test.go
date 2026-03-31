package cloudapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	k6cloud "github.com/grafana/k6-cloud-openapi-client-go/k6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.k6.io/k6/internal/build"
	"go.k6.io/k6/internal/lib/testutils"
	"go.k6.io/k6/lib"
	"go.k6.io/k6/lib/fsext"
	"go.k6.io/k6/lib/types"
	"gopkg.in/guregu/null.v3"
)

// testProjectID is used across tests as the project ID in mock API requests.
const testProjectID = 789

func TestValidateToken(t *testing.T) {
	t.Parallel()

	t.Run("successful token validation", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify the authorization header
			authHeader := r.Header.Get("Authorization")
			assert.Equal(t, "Bearer test-token", authHeader)

			// Verify the stack URL
			stackURL := r.Header.Get("X-Stack-Url")
			assert.Equal(t, stackURL, "https://stack.grafana.net")

			w.Header().Add("Content-Type", "application/json")
			fprint(t, w, `{
				"stack_id": 123,
				"default_project_id": 456
			}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken(t.Context(), "https://stack.grafana.net")
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, int32(123), resp.StackId)
		assert.Equal(t, int32(456), resp.DefaultProjectId)
	})

	t.Run("unauthorized token should fail", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fprint(t, w, `{
				"error": {
					"code": "error",
					"message": "Invalid token"
				}
			}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "invalid-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken(t.Context(), "https://stack.grafana.net")
		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "(401/error) Invalid token")
	})

	t.Run("network error should fail", func(t *testing.T) {
		t.Parallel()
		// Use an invalid URL to simulate network error
		client, err := NewClient(testutils.NewLogger(t), "test-token", "http://invalid-url-that-does-not-exist", "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken(t.Context(), "https://stack.grafana.net")
		assert.Error(t, err)
		assert.Nil(t, resp)
	})

	t.Run("missing stack URL should fail", func(t *testing.T) {
		t.Parallel()
		client, err := NewClient(testutils.NewLogger(t), "test-token", "http://example.com", "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken(t.Context(), "")
		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, "stack URL is required to validate token", err.Error())
	})

	t.Run("invalid stack URL should fail", func(t *testing.T) {
		t.Parallel()
		client, err := NewClient(testutils.NewLogger(t), "test-token", "http://example.com", "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken(t.Context(), "://invalid-url")
		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "invalid stack URL")
	})
}

func TestValidateOptions(t *testing.T) {
	t.Parallel()

	t.Run("successful options validation", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			assert.Equal(t, "123", r.Header.Get("X-Stack-Id"))

			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)

			var validateOptions k6cloud.ValidateOptionsRequest
			err = json.Unmarshal(b, &validateOptions)
			require.NoError(t, err)

			duration := validateOptions.Options.AdditionalProperties["duration"]
			assert.Equal(t, "1m0s", duration)

			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		opts := lib.Options{
			Duration: types.NullDurationFrom(60 * time.Second),
		}
		err = client.ValidateOptions(t.Context(), testProjectID, opts)
		require.NoError(t, err)
	})

	t.Run("validation error", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fprint(t, w, `{
				"error": {
					"code": "error",
					"message": "Invalid VUs number"
				}
			}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		opts := lib.Options{
			VUs: null.IntFrom(-1),
		}
		err = client.ValidateOptions(t.Context(), testProjectID, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Invalid VUs number")
	})
}

func TestCreateCloudTest(t *testing.T) {
	t.Parallel()

	t.Run("successful test creation", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			assert.Equal(t, "123", r.Header.Get("X-Stack-Id"))

			formData := parseFormData(t, r)
			testName := formData["name"]

			assert.Contains(t, r.URL.Path, "789")

			w.Header().Add("Content-Type", "application/json")
			fprint(t, w, fmt.Sprintf(`{
				"id": 456,
				"name": "%s",
				"project_id": 789,
				"baseline_test_run_id": null,
				"created": "2024-01-01T00:00:00Z",
				"updated": "2024-01-01T00:00:00Z"
			}`, testName))
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		arcData := createTestArchiveBytes(t)
		result, err := client.CreateCloudTest(t.Context(), "test-name", testProjectID, arcData)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, int32(456), result.Id)
		assert.Equal(t, "test-name", result.Name)
		assert.Equal(t, int32(789), result.ProjectId)
	})
}

func TestFetchCloudTestByName(t *testing.T) {
	t.Parallel()

	t.Run("test found", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "test-name", r.URL.Query().Get("name"))

			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{
				"value": [
					{
						"id": 456,
						"name": "test-name",
						"project_id": 789,
						"baseline_test_run_id": null,
						"created": "2024-01-01T00:00:00Z",
						"updated": "2024-01-01T00:00:00Z"
					}
				]
			}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		result, err := client.FetchCloudTestByName(t.Context(), "test-name", testProjectID)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, int32(456), result.Id)
	})

	t.Run("no matching test", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{
				"value": [
					{
						"id": 456,
						"name": "other-test",
						"project_id": 789,
						"baseline_test_run_id": null,
						"created": "2024-01-01T00:00:00Z",
						"updated": "2024-01-01T00:00:00Z"
					}
				]
			}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		_, err = client.FetchCloudTestByName(t.Context(), "my-test", testProjectID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"my-test" not found in project`)
	})
}

func TestCreateOrUpdateCloudTest(t *testing.T) {
	t.Parallel()

	t.Run("creates new test", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fprint(t, w, `{
				"id": 456,
				"name": "test-name",
				"project_id": 789,
				"baseline_test_run_id": null,
				"created": "2024-01-01T00:00:00Z",
				"updated": "2024-01-01T00:00:00Z"
			}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		arc := createTestArchive(t)
		result, err := client.CreateOrUpdateCloudTest(t.Context(), "test-name", testProjectID, arc)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, int32(456), result.Id)
	})

	t.Run("updates on conflict", func(t *testing.T) {
		t.Parallel()
		getCalled := false
		updateCalled := false

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			switch r.Method {
			case http.MethodPost:
				w.WriteHeader(http.StatusConflict)
				fprint(t, w, `{"error": {"code": "error", "message": "conflict"}}`)
			case http.MethodGet:
				getCalled = true
				fprint(t, w, `{
					"value": [{
						"id": 456, "name": "test-name", "project_id": 789,
						"baseline_test_run_id": null,
						"created": "2024-01-01T00:00:00Z", "updated": "2024-01-01T00:00:00Z"
					}]
				}`)
			case http.MethodPut:
				updateCalled = true
				w.WriteHeader(http.StatusNoContent)
			}
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		arc := createTestArchive(t)
		result, err := client.CreateOrUpdateCloudTest(t.Context(), "test-name", testProjectID, arc)
		require.NoError(t, err)
		assert.Equal(t, int32(456), result.Id)
		assert.True(t, getCalled)
		assert.True(t, updateCalled)
	})
}

func fprint(t *testing.T, w io.Writer, s string) int {
	n, err := fmt.Fprint(w, s)
	require.NoError(t, err)
	return n
}

func createTestArchive(t *testing.T) *lib.Archive {
	t.Helper()
	fs := fsext.NewMemMapFs()
	err := fsext.WriteFile(fs, "/path/to/a.js", []byte(`// a contents`), 0o644)
	require.NoError(t, err)

	return &lib.Archive{
		Type:        "js",
		K6Version:   build.Version,
		Options:     lib.Options{},
		FilenameURL: &url.URL{Scheme: "file", Path: "/path/to/a.js"},
		Data:        []byte(`// a contents`),
		PwdURL:      &url.URL{Scheme: "file", Path: "/path/to"},
		Filesystems: map[string]fsext.Fs{
			"file": fs,
		},
	}
}

func createTestArchiveBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, createTestArchive(t).Write(&buf))
	return buf.Bytes()
}

func parseFormData(t *testing.T, r *http.Request) map[string]string {
	t.Helper()

	require.NoError(t, r.ParseMultipartForm(32<<20))

	formData := make(map[string]string)
	for key, values := range r.MultipartForm.Value {
		if len(values) > 0 {
			formData[key] = values[0]
		}
	}

	return formData
}
