package google

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	raw "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

func TestSetConfigFromFlags_InstanceGroupManager(t *testing.T) {
	driver := NewDriver("", "")
	checkFlags := &drivers.CheckDriverOptions{
		FlagsValues: map[string]interface{}{
			"google-project":                "PROJECT",
			"google-instance-group-manager": "my-mig",
		},
		CreateFlags: driver.GetCreateFlags(),
	}

	require.NoError(t, driver.SetConfigFromFlags(checkFlags))
	assert.Empty(t, checkFlags.InvalidFlags)
	assert.Equal(t, "my-mig", driver.InstanceGroupManager)
	assert.True(t, driver.usesMIG(), "usesMIG must be true once the flag is set")
}

func TestUsesMIG_DefaultsToFalse(t *testing.T) {
	// Zero-value driver must not accidentally take the MIG path. Regression
	// guard against future struct-init changes.
	d := &Driver{}
	assert.False(t, d.usesMIG())
	c := &ComputeUtil{}
	assert.False(t, c.usesMIG())
}

// migTestServer returns an httptest server that handles whichever MIG / op /
// instance endpoints the test routes through, and a `requests` slice
// recording every path that came in. Handlers respond with DONE operations
// where applicable so the wait-for-op loop terminates quickly.
type recordedRequest struct {
	method string
	path   string
	body   string
}

func newMIGTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, recorded *[]recordedRequest)) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var recorded []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorded = append(recorded, recordedRequest{r.Method, r.URL.Path, string(body)})
		handler(w, r, &recorded)
	}))
	return srv, &recorded
}

func newComputeUtilForTest(t *testing.T, endpoint string, ig string) *ComputeUtil {
	t.Helper()
	svc, err := raw.NewService(context.Background(), option.WithoutAuthentication(), option.WithEndpoint(endpoint))
	require.NoError(t, err)
	return &ComputeUtil{
		zone:                    "us-east1-c",
		instanceName:            "test-instance-abc",
		project:                 "test-project",
		service:                 svc,
		instanceGroupManager:    ig,
		operationBackoffFactory: testBackoffFactory(),
	}
}

// testBackoffFactory returns the smallest plausible backoff so wait-for-op
// loops don't add seconds to test runtime.
func testBackoffFactory() *backoffFactory {
	return &backoffFactory{
		InitialInterval:     1, // 1ns; the test server returns DONE on the first poll anyway
		RandomizationFactor: 0,
		Multiplier:          2,
		MaxInterval:         1,
		MaxElapsedTime:      1,
	}
}

func TestCreateInstanceViaMIG_IssuesCreateInstancesRequest(t *testing.T) {
	// Verifies that calling createInstanceViaMIG with a configured MIG
	// (a) targets the right MIG endpoint, (b) supplies the docker-machine
	// instance name in the per-instance config, and (c) waits for the
	// returned operation. We surface a wait-for-op error to short-circuit
	// the subsequent post-create steps (instance lookup, firewall tag, SSH
	// upload) — those are already covered by tests on the direct path and
	// reusing them would just add mocking weight.
	srv, recorded := newMIGTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *[]recordedRequest) {
		switch {
		case strings.Contains(r.URL.Path, "/instanceGroupManagers/my-mig/createInstances"):
			respondJSON(t, w, &raw.Operation{Name: "op-create-123", Status: "PENDING"})
		case strings.Contains(r.URL.Path, "/operations/op-create-123/wait"):
			// Return a DONE op with an error so the wait short-circuits.
			respondJSON(t, w, &raw.Operation{
				Name:   "op-create-123",
				Status: "DONE",
				Error:  &raw.OperationError{Errors: []*raw.OperationErrorErrors{{Code: "TEST_SHORTCIRCUIT", Message: "stop here"}}},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	c := newComputeUtilForTest(t, srv.URL, "my-mig")
	err := c.createInstanceViaMIG(&Driver{})

	require.Error(t, err, "expected short-circuit error from the operation wait")
	assert.Contains(t, err.Error(), "TEST_SHORTCIRCUIT")

	// Confirm the right MIG endpoint was hit with the configured name.
	var sawCreate, sawWait bool
	for _, r := range *recorded {
		if r.method == "POST" && strings.HasSuffix(r.path, "/instanceGroupManagers/my-mig/createInstances") {
			sawCreate = true
			var req raw.InstanceGroupManagersCreateInstancesRequest
			require.NoError(t, json.Unmarshal([]byte(r.body), &req))
			require.Len(t, req.Instances, 1)
			assert.Equal(t, "test-instance-abc", req.Instances[0].Name,
				"per-instance config must carry the docker-machine instance name")
		}
		if strings.Contains(r.path, "/operations/op-create-123/wait") {
			sawWait = true
		}
	}
	assert.True(t, sawCreate, "createInstances endpoint was not called")
	assert.True(t, sawWait, "wait-for-op was not called against the returned operation")
}

func TestDeleteInstanceViaMIG_IssuesDeleteInstancesRequest(t *testing.T) {
	srv, recorded := newMIGTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *[]recordedRequest) {
		switch {
		case strings.Contains(r.URL.Path, "/instanceGroupManagers/my-mig/deleteInstances"):
			respondJSON(t, w, &raw.Operation{Name: "op-delete-456", Status: "PENDING"})
		case strings.Contains(r.URL.Path, "/operations/op-delete-456/wait"):
			respondJSON(t, w, &raw.Operation{Name: "op-delete-456", Status: "DONE"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	c := newComputeUtilForTest(t, srv.URL, "my-mig")
	require.NoError(t, c.deleteInstanceViaMIG())

	var sawDelete bool
	for _, r := range *recorded {
		if r.method == "POST" && strings.HasSuffix(r.path, "/instanceGroupManagers/my-mig/deleteInstances") {
			sawDelete = true
			var req raw.InstanceGroupManagersDeleteInstancesRequest
			require.NoError(t, json.Unmarshal([]byte(r.body), &req))
			require.Len(t, req.Instances, 1)
			assert.Contains(t, req.Instances[0], "zones/us-east1-c/instances/test-instance-abc",
				"delete request must reference the instance by its zonal URL")
			assert.True(t, req.SkipInstancesOnValidationError,
				"skipInstancesOnValidationError must be set so manual-cleanup races don't error")
		}
	}
	assert.True(t, sawDelete, "deleteInstances endpoint was not called")
}

func respondJSON(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(v))
}
