package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.k6.io/k6/errext/exitcodes"
	v6cloudapi "go.k6.io/k6/internal/cloudapi/v6"
	"go.k6.io/k6/internal/cmd"
	"go.k6.io/k6/internal/lib/testutils"
	"go.k6.io/k6/lib/fsext"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK6Cloud(t *testing.T) {
	t.Parallel()
	runCloudTests(t, setupK6CloudCmd)
}

func setupK6CloudCmd(cliFlags []string) []string {
	return append([]string{"k6", "cloud"}, append(cliFlags, "test.js")...)
}

type setupCommandFunc func(cliFlags []string) []string

const testStackURL = "https://app.k6.io"

func runCloudTests(t *testing.T, setupCmd setupCommandFunc) {
	t.Run("TestCloudUserNotAuthenticated", func(t *testing.T) {
		t.Parallel()

		ts := getSimpleCloudTestState(t, nil, setupCmd, nil, nil, nil)
		delete(ts.Env, "K6_CLOUD_TOKEN")
		ts.ExpectedExitCode = -1 // TODO: use a more specific exit code?
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `must first authenticate`)
	})

	t.Run("TestCloudLoggedInWithScriptToken", func(t *testing.T) {
		t.Parallel()

		script := `
		export let options = {
			cloud: {
				token: "asdf",
				name: "my load test",
				projectID: 124,
				note: 124,
			}
		};
		export default function() {};
	`

		ts := getSimpleCloudTestState(t, []byte(script), setupCmd, nil, nil, nil)
		delete(ts.Env, "K6_CLOUD_TOKEN")
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.NotContains(t, stdout, `not logged in`)
		assert.Contains(t, stdout, `execution: cloud`)
		assert.Contains(t, stdout, "output: "+testStackURL+"/runs/123")
		assert.Contains(t, stdout, `test status: Completed`)
	})

	t.Run("TestCloudExitOnRunning", func(t *testing.T) {
		t.Parallel()

		cs := func() v6cloudapi.TestRunProgress {
			return v6cloudapi.TestRunProgress{
				Status:            v6cloudapi.StatusRunning,
				EstimatedDuration: 10,
				ExecutionDuration: 1,
			}
		}

		ts := getSimpleCloudTestState(t, nil, setupCmd, []string{"--exit-on-running", "--log-output=stdout"}, nil, cs)
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `execution: cloud`)
		assert.Contains(t, stdout, "output: "+testStackURL+"/runs/123")
		assert.Contains(t, stdout, `test status: Running`)
	})

	t.Run("TestCloudUploadOnly", func(t *testing.T) {
		t.Parallel()

		ts := getSimpleCloudTestState(t, nil, setupCmd, []string{"--upload-only", "--log-output=stdout"}, nil, nil)
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `execution: cloud`)
		assert.Contains(t, stdout, "output: "+testStackURL+"/runs/456")
		assert.Contains(t, stdout, `test status: Uploaded`)
	})

	t.Run("TestCloudWithConfigOverride", func(t *testing.T) {
		t.Parallel()

		// In v6, the ConfigOverride is no longer used. The URL comes from
		// URLForResults until we switch to TestRunDetailsPageUrl in a later commit.
		ts := getSimpleCloudTestState(t, nil, setupCmd, nil, nil, nil)
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, "execution: cloud")
		assert.Contains(t, stdout, "output: "+testStackURL+"/runs/123")
	})

	// TestCloudWithArchive tests that if k6 uses a static archive with the script inside that has cloud options like:
	//
	//	export let options = {
	//		cloud: {
	//			name: "my load test",
	//			projectID: 124,
	//			note: "lorem ipsum",
	//		}
	//	};
	//
	// actually sends to the cloud the archive with the correct metadata (metadata.json), like:
	//
	//	"clouad": {
	//	    "name": "my load test",
	//	    "note": "lorem ipsum",
	//	    "projectID": 124
	//	}
	t.Run("TestCloudWithArchive", func(t *testing.T) {
		t.Parallel()

		testRunID := 123
		ts := NewGlobalTestState(t)

		archiveUpload := http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
			// check the archive
			file, _, err := req.FormFile("script")
			assert.NoError(t, err)
			assert.NotNil(t, file)

			// temporary write the archive for file system
			data, err := io.ReadAll(file)
			assert.NoError(t, err)

			tmpPath := filepath.Join(ts.Cwd, "archive_to_cloud.tar")
			require.NoError(t, fsext.WriteFile(ts.FS, tmpPath, data, 0o644))

			// check what inside
			require.NoError(t, testutils.Untar(t, ts.FS, tmpPath, "tmp/"))

			metadataRaw, err := fsext.ReadFile(ts.FS, "tmp/metadata.json")
			require.NoError(t, err)

			metadata := struct {
				Options struct {
					Cloud struct {
						Name      string `json:"name"`
						Note      string `json:"note"`
						ProjectID int    `json:"projectID"`
					} `json:"cloud"`
				} `json:"options"`
			}{}

			// then unpacked metadata should not contain any environment variables passed at the moment of archive creation
			require.NoError(t, json.Unmarshal(metadataRaw, &metadata))
			require.Equal(t, "my load test", metadata.Options.Cloud.Name)
			require.Equal(t, "lorem ipsum", metadata.Options.Cloud.Note)
			// projectID is overridden by K6_CLOUD_PROJECT_ID env var (456)
			require.Equal(t, 456, metadata.Options.Cloud.ProjectID)

			// respond with the load test
			writeJSON(resp, http.StatusCreated, loadTestJSON)
		})

		srv := getMockCloud(t, testRunID, archiveUpload, nil)

		data, err := os.ReadFile(filepath.Join("testdata/archives", "archive_v1.0.0_with_cloud_option.tar")) //nolint:forbidigo // it's a test
		require.NoError(t, err)

		require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "archive.tar"), data, 0o644))

		ts.CmdArgs = []string{"k6", "cloud", "--verbose", "--log-output=stdout", "archive.tar"}
		ts.Env["K6_SHOW_CLOUD_LOGS"] = "false" // no mock for the logs yet
		ts.Env["K6_CLOUD_HOST"] = srv.URL
		ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
		ts.Env["K6_CLOUD_TOKEN"] = "foo" // doesn't matter, we mock the cloud
		ts.Env["K6_CLOUD_STACK_ID"] = "123"
		ts.Env["K6_CLOUD_PROJECT_ID"] = "456"
		ts.Env["K6_CLOUD_STACK_URL"] = testStackURL

		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.NotContains(t, stdout, `not logged in`)
		assert.Contains(t, stdout, `execution: cloud`)
		assert.Contains(t, stdout, `hello world from archive`)
		assert.Contains(t, stdout, "output: "+testStackURL+"/runs/123")
		assert.Contains(t, stdout, `test status: Completed`)
	})

	t.Run("TestCloudThresholdsHaveFailed", func(t *testing.T) {
		t.Parallel()

		progressCallback := func() v6cloudapi.TestRunProgress {
			return v6cloudapi.TestRunProgress{
				Status:            v6cloudapi.StatusCompleted,
				Result:            v6cloudapi.ResultFailed,
				EstimatedDuration: 10,
				ExecutionDuration: 10,
			}
		}
		ts := getSimpleCloudTestState(t, nil, setupCmd, nil, nil, progressCallback)
		ts.ExpectedExitCode = int(exitcodes.ThresholdsHaveFailed)

		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `Thresholds have been crossed`)
	})

	t.Run("TestCloudAbortedFailed", func(t *testing.T) {
		t.Parallel()

		// Per the v6 spec, result "failed" always means thresholds breached,
		// even when the test was aborted (e.g. aborted due to threshold).
		progressCallback := func() v6cloudapi.TestRunProgress {
			return v6cloudapi.TestRunProgress{
				Status:            v6cloudapi.StatusAborted,
				Result:            v6cloudapi.ResultFailed,
				EstimatedDuration: 10,
				ExecutionDuration: 10,
			}
		}
		ts := getSimpleCloudTestState(t, nil, setupCmd, nil, nil, progressCallback)
		ts.ExpectedExitCode = int(exitcodes.ThresholdsHaveFailed)

		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `Thresholds have been crossed`)
		assert.Contains(t, stdout, `test status: Aborted`)
	})

	t.Run("TestCloudResultError", func(t *testing.T) {
		t.Parallel()

		progressCallback := func() v6cloudapi.TestRunProgress {
			return v6cloudapi.TestRunProgress{
				Status:            v6cloudapi.StatusCompleted,
				Result:            v6cloudapi.ResultError,
				EstimatedDuration: 10,
				ExecutionDuration: 10,
			}
		}
		ts := getSimpleCloudTestState(t, nil, setupCmd, nil, nil, progressCallback)
		ts.ExpectedExitCode = int(exitcodes.CloudTestRunFailed)

		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `The test has failed`)
	})
}

const loadTestJSON = `{
	"id": 456,
	"project_id": 789,
	"name": "test",
	"baseline_test_run_id": null,
	"created": "2024-01-01T00:00:00Z",
	"updated": "2024-01-01T00:00:00Z"
}`

const testRunJSONTmpl = `{
	"id": %d,
	"test_id": 456,
	"project_id": 789,
	"started_by": null,
	"created": "2024-01-01T00:00:00Z",
	"ended": null,
	"note": "",
	"retention_expiry": null,
	"cost": null,
	"status": "%s",
	"status_details": {"type": "created", "entered": "2024-01-01T00:00:00Z"},
	"status_history": [{"type": "created", "entered": "2024-01-01T00:00:00Z"}],
	"distribution": [],
	"result": %s,
	"result_details": {},
	"options": {},
	"k6_dependencies": {},
	"k6_versions": {},
	"max_vus": null,
	"max_browser_vus": null,
	"estimated_duration": null,
	"execution_duration": 0,
	"test_run_details_page_url": "%s"
}`

func cloudTestStartSimpleV1(tb testing.TB, testRunID int) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
		resp.WriteHeader(http.StatusOK)
		_, err := fmt.Fprintf(resp, `{"reference_id": "%d"}`, testRunID)
		assert.NoError(tb, err)
	})
}

func cloudTestCreateSimple(_ testing.TB) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
		writeJSON(resp, http.StatusCreated, loadTestJSON)
	})
}

func cloudTestStartSimple(_ testing.TB, testRunID int, webAppURL string) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
		writeJSON(resp, http.StatusOK,
			fmt.Sprintf(testRunJSONTmpl, testRunID, "running", `null`, webAppURL))
	})
}

// writeJSON sets Content-Type and writes a JSON body to the response.
func writeJSON(resp http.ResponseWriter, status int, body string) {
	resp.Header().Set("Content-Type", "application/json")
	resp.WriteHeader(status)
	_, _ = fmt.Fprint(resp, body)
}

func v6ValidateOptionsHandler(resp http.ResponseWriter, _ *http.Request) {
	writeJSON(resp, http.StatusOK, `{}`)
}

func v6ProgressHandler(
	testRunID int, webAppURL string, defaultProgress v6cloudapi.TestRunProgress,
	progressCallback func() v6cloudapi.TestRunProgress,
) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
		tp := defaultProgress
		if progressCallback != nil {
			tp = progressCallback()
		}
		resultVal := "null"
		if tp.Result != "" {
			resultVal = fmt.Sprintf(`"%s"`, tp.Result)
		}
		writeJSON(resp, http.StatusOK,
			fmt.Sprintf(testRunJSONTmpl, testRunID, tp.Status, resultVal, webAppURL))
	})
}

func getMockCloud(
	t *testing.T, testRunID int,
	createLoadTest http.Handler, progressCallback func() v6cloudapi.TestRunProgress,
) *httptest.Server {
	if createLoadTest == nil {
		createLoadTest = cloudTestCreateSimple(t)
	}

	defaultWebAppURL := fmt.Sprintf("%s/runs/%d", testStackURL, testRunID)

	defaultProgress := v6cloudapi.TestRunProgress{
		Status:            v6cloudapi.StatusCompleted,
		EstimatedDuration: 10,
		ExecutionDuration: 10,
	}

	srv := getTestServer(t, map[string]http.Handler{
		"POST ^/cloud/v6/validate_options$":                           http.HandlerFunc(v6ValidateOptionsHandler),
		`POST ^/cloud/v6/projects/\d+/load_tests$`:                    createLoadTest,
		`PUT ^/cloud/v6/load_tests/\d+/script$`:                       http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) { resp.WriteHeader(http.StatusNoContent) }),
		`POST ^/cloud/v6/load_tests/\d+/start$`:                       cloudTestStartSimple(t, testRunID, defaultWebAppURL),
		fmt.Sprintf("GET ^/cloud/v6/test_runs/%d$", testRunID):        v6ProgressHandler(testRunID, defaultWebAppURL, defaultProgress, progressCallback),
		fmt.Sprintf("POST ^/cloud/v6/test_runs/%d/abort$", testRunID): http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) { resp.WriteHeader(http.StatusNoContent) }),
		`GET ^/cloud/v6/projects/\d+/load_tests`: http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
			writeJSON(resp, http.StatusOK, fmt.Sprintf(`{"value": [%s]}`, loadTestJSON))
		}),
	})

	t.Cleanup(srv.Close)

	return srv
}

func getSimpleCloudTestState(
	t *testing.T,
	script []byte,
	setupCmd setupCommandFunc,
	cliFlags []string,
	startHandler http.Handler,
	progressCallback func() v6cloudapi.TestRunProgress,
) *GlobalTestState {
	if script == nil {
		script = []byte(`export default function() {}`)
	}

	if cliFlags == nil {
		cliFlags = []string{"--verbose", "--log-output=stdout"}
	}

	var srv *httptest.Server
	if startHandler != nil {
		defaultProgress := v6cloudapi.TestRunProgress{
			Status:            v6cloudapi.StatusCompleted,
			EstimatedDuration: 10,
			ExecutionDuration: 10,
		}
		srv = getTestServer(t, map[string]http.Handler{
			"POST ^/cloud/v6/validate_options$":        http.HandlerFunc(v6ValidateOptionsHandler),
			`POST ^/cloud/v6/projects/\d+/load_tests$`: cloudTestCreateSimple(t),
			`PUT ^/cloud/v6/load_tests/\d+/script$`:    http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) { resp.WriteHeader(http.StatusNoContent) }),
			`POST ^/cloud/v6/load_tests/\d+/start$`:    startHandler,
			"GET ^/cloud/v6/test_runs/123$":            v6ProgressHandler(123, testStackURL+"/runs/123", defaultProgress, progressCallback),
			"POST ^/cloud/v6/test_runs/123/abort$":     http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) { resp.WriteHeader(http.StatusNoContent) }),
			`GET ^/cloud/v6/projects/\d+/load_tests`: http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
				writeJSON(resp, http.StatusOK, fmt.Sprintf(`{"value": [%s]}`, loadTestJSON))
			}),
		})
	} else {
		srv = getMockCloud(t, 123, nil, progressCallback)
	}

	t.Cleanup(srv.Close)

	ts := NewGlobalTestState(t)
	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"), script, 0o644))
	ts.CmdArgs = setupCmd(cliFlags)
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false" // no mock for the logs yet
	ts.Env["K6_CLOUD_HOST"] = srv.URL
	ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo" // doesn't matter, we mock the cloud
	ts.Env["K6_CLOUD_STACK_ID"] = "123"
	ts.Env["K6_CLOUD_PROJECT_ID"] = "456"
	ts.Env["K6_CLOUD_STACK_URL"] = testStackURL

	return ts
}
