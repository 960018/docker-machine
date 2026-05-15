package google

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	assert.False(t, d.usesRegionalMIG())
	c := &ComputeUtil{}
	assert.False(t, c.usesMIG())
	assert.False(t, c.usesRegionalMIG())
}

func TestSetConfigFromFlags_RegionalMIG(t *testing.T) {
	driver := NewDriver("", "")
	checkFlags := &drivers.CheckDriverOptions{
		FlagsValues: map[string]interface{}{
			"google-project":                       "PROJECT",
			"google-region-instance-group-manager": "my-regional-mig",
			"google-region":                        "us-east1",
		},
		CreateFlags: driver.GetCreateFlags(),
	}

	require.NoError(t, driver.SetConfigFromFlags(checkFlags))
	assert.Empty(t, checkFlags.InvalidFlags)
	assert.Equal(t, "my-regional-mig", driver.RegionInstanceGroupManager)
	assert.Equal(t, "us-east1", driver.Region)
	assert.True(t, driver.usesMIG())
	assert.True(t, driver.usesRegionalMIG())
}

func TestSetConfigFromFlags_RegionalMIGRequiresRegion(t *testing.T) {
	// Regional MIGs need the region to issue API calls and to compute the
	// instance self-link. Omitting --google-region is a config error worth
	// surfacing before any job tries to run.
	driver := NewDriver("", "")
	checkFlags := &drivers.CheckDriverOptions{
		FlagsValues: map[string]interface{}{
			"google-project":                       "PROJECT",
			"google-region-instance-group-manager": "my-regional-mig",
		},
		CreateFlags: driver.GetCreateFlags(),
	}

	err := driver.SetConfigFromFlags(checkFlags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires --google-region")
}

func TestSetConfigFromFlags_ZonalAndRegionalMIGAreMutuallyExclusive(t *testing.T) {
	driver := NewDriver("", "")
	checkFlags := &drivers.CheckDriverOptions{
		FlagsValues: map[string]interface{}{
			"google-project":                       "PROJECT",
			"google-instance-group-manager":        "zonal",
			"google-region-instance-group-manager": "regional",
			"google-region":                        "us-east1",
		},
		CreateFlags: driver.GetCreateFlags(),
	}

	err := driver.SetConfigFromFlags(checkFlags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestRegion_PrefersExplicitOverDerivedFromZone(t *testing.T) {
	// In regional MIG mode, zone is empty until post-create discovery; the
	// derive-from-zone fallback would panic. Verify the explicit region
	// takes precedence and the fallback still works for zonal mode.
	c := &ComputeUtil{regionExplicit: "europe-west4"}
	assert.Equal(t, "europe-west4", c.region())

	c = &ComputeUtil{zone: "us-east1-c"}
	assert.Equal(t, "us-east1", c.region())
}

func TestZoneFromInstanceURL(t *testing.T) {
	tests := map[string]struct {
		input   string
		want    string
		wantErr bool
	}{
		"standard self-link": {
			input: "https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-c/instances/foo",
			want:  "us-east1-c",
		},
		"relative form": {
			input: "projects/p/zones/europe-west4-a/instances/bar",
			want:  "europe-west4-a",
		},
		"no zones segment": {
			input:   "projects/p/regions/us-east1/instances/x",
			wantErr: true,
		},
		"malformed (truncated after zone)": {
			input:   "https://www.googleapis.com/compute/v1/projects/p/zones/us-east1-c",
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := zoneFromInstanceURL(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func newComputeUtilForRegionalTest(t *testing.T, endpoint, region, mig string) *ComputeUtil {
	t.Helper()
	svc, err := raw.NewService(context.Background(), option.WithoutAuthentication(), option.WithEndpoint(endpoint))
	require.NoError(t, err)
	return &ComputeUtil{
		// zone deliberately empty: regional MIG mode discovers it post-create.
		instanceName:               "test-instance-abc",
		project:                    "test-project",
		service:                    svc,
		regionExplicit:             region,
		regionInstanceGroupManager: mig,
		operationBackoffFactory:    testBackoffFactory(),
	}
}

func TestCreateInstanceViaRegionalMIG_IssuesCreateAndDiscoversZone(t *testing.T) {
	// Verifies the regional happy path through the point where zone
	// discovery completes:
	//   1. POST .../regions/us-east1/instanceGroupManagers/<mig>/createInstances
	//   2. Wait on a region operation
	//   3. ListManagedInstances on the MIG, server-side filtered to our
	//      name, returns one ManagedInstance whose `instance` URL carries
	//      the zone
	//   4. Look the instance up via Instances.Get in the discovered zone
	// We short-circuit at step 4 with a 500 so we don't have to mock the
	// firewall-tag / SSH-metadata downstream, which is shared code path
	// already exercised in other tests.
	srv, recorded := newMIGTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *[]recordedRequest) {
		switch {
		case strings.Contains(r.URL.Path, "/regions/us-east1/instanceGroupManagers/my-rmig/createInstances"):
			respondJSON(t, w, &raw.Operation{Name: "op-rcreate-1", Status: "PENDING"})
		case strings.Contains(r.URL.Path, "/regions/us-east1/operations/op-rcreate-1/wait"):
			respondJSON(t, w, &raw.Operation{Name: "op-rcreate-1", Status: "DONE"})
		case strings.Contains(r.URL.Path, "/regions/us-east1/instanceGroupManagers/my-rmig/listManagedInstances"):
			// CurrentAction "NONE" means the underlying VM is fully
			// provisioned. The create path will not call Instances.Get
			// until this is the case; see waitForMIGInstanceReady.
			respondJSON(t, w, &raw.RegionInstanceGroupManagersListInstancesResponse{
				ManagedInstances: []*raw.ManagedInstance{{
					Instance:      "https://www.googleapis.com/compute/v1/projects/test-project/zones/us-east1-d/instances/test-instance-abc",
					CurrentAction: "NONE",
				}},
			})
		case strings.Contains(r.URL.Path, "/zones/us-east1-d/instances/test-instance-abc"):
			// short-circuit downstream post-create steps
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	c := newComputeUtilForRegionalTest(t, srv.URL, "us-east1", "my-rmig")
	d := &Driver{Region: "us-east1", RegionInstanceGroupManager: "my-rmig"}

	err := c.createInstanceViaMIG(d)
	require.Error(t, err, "expected short-circuit error from the post-discovery Instances.Get")

	// Zone discovery must have updated both ComputeUtil and Driver state.
	assert.Equal(t, "us-east1-d", c.zone, "ComputeUtil zone must reflect MIG placement")
	assert.Equal(t, "us-east1-d", d.Zone, "Driver.Zone must persist the discovered zone")

	var sawCreate, sawWait, sawList bool
	var listQuery string
	for _, r := range *recorded {
		switch {
		case r.method == "POST" && strings.HasSuffix(r.path, "/regions/us-east1/instanceGroupManagers/my-rmig/createInstances"):
			sawCreate = true
			var req raw.RegionInstanceGroupManagersCreateInstancesRequest
			require.NoError(t, json.Unmarshal([]byte(r.body), &req))
			require.Len(t, req.Instances, 1)
			assert.Equal(t, "test-instance-abc", req.Instances[0].Name)
		case strings.Contains(r.path, "/regions/us-east1/operations/op-rcreate-1/wait"):
			sawWait = true
		case strings.Contains(r.path, "/regions/us-east1/instanceGroupManagers/my-rmig/listManagedInstances"):
			sawList = true
			listQuery = r.rawQuery
		}
	}
	assert.True(t, sawCreate, "regional createInstances was not called")
	assert.True(t, sawWait, "regional operations.wait was not called")
	assert.True(t, sawList, "listManagedInstances was not called for zone discovery")
	// URL-encoded form of `name eq "test-instance-abc"`. The presence of
	// the filter is what bounds the response under quota; an unfiltered
	// list against a busy MIG returns the entire instance set in one
	// response (PAGELESS default).
	assert.Contains(t, listQuery, "filter=name+eq+%22test-instance-abc%22",
		"listManagedInstances must carry a server-side name filter to bound the response under quota")
}

func TestCreateInstanceViaRegionalMIG_WaitsForCurrentActionNone(t *testing.T) {
	// Regression guard for a real bug surfaced against gitlab-runner-sandbox:
	// the regional CreateInstances Operation completing does not mean the
	// underlying VM is visible to Instances.Get. The MIG accepts the
	// request and starts provisioning, but the synchronous post-create
	// steps (firewall tag, SSH-key push) race and 404 if we don't wait
	// for currentAction to reach "NONE".
	//
	// Test shape: listManagedInstances returns CurrentAction "CREATING"
	// the first time it's called and "NONE" thereafter. The create must
	// poll past the first response, and Instances.Get must only fire
	// after the MIG reports "NONE".
	var listCalls int
	srv, _ := newMIGTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *[]recordedRequest) {
		switch {
		case strings.Contains(r.URL.Path, "/regions/us-east1/instanceGroupManagers/my-rmig/createInstances"):
			respondJSON(t, w, &raw.Operation{Name: "op-rcreate-2", Status: "PENDING"})
		case strings.Contains(r.URL.Path, "/regions/us-east1/operations/op-rcreate-2/wait"):
			respondJSON(t, w, &raw.Operation{Name: "op-rcreate-2", Status: "DONE"})
		case strings.Contains(r.URL.Path, "/regions/us-east1/instanceGroupManagers/my-rmig/listManagedInstances"):
			listCalls++
			action := "CREATING"
			if listCalls > 1 {
				action = "NONE"
			}
			respondJSON(t, w, &raw.RegionInstanceGroupManagersListInstancesResponse{
				ManagedInstances: []*raw.ManagedInstance{{
					Instance:      "https://www.googleapis.com/compute/v1/projects/test-project/zones/us-east1-c/instances/test-instance-abc",
					CurrentAction: action,
				}},
			})
		case strings.Contains(r.URL.Path, "/zones/us-east1-c/instances/test-instance-abc"):
			// Short-circuit post-create lookups; we just need to verify
			// the poll ran past the "CREATING" response before reaching
			// this Instances.Get.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	c := newComputeUtilForRegionalTest(t, srv.URL, "us-east1", "my-rmig")
	// testBackoffFactory's 1ns MaxElapsedTime means the poll loop would
	// hit Stop after a single iteration. Give it just enough slack to
	// allow at least two polls; the test still finishes in milliseconds.
	c.operationBackoffFactory = &backoffFactory{
		InitialInterval:     1,
		RandomizationFactor: 0,
		Multiplier:          1,
		MaxInterval:         1,
		MaxElapsedTime:      time.Second,
	}
	d := &Driver{Region: "us-east1", RegionInstanceGroupManager: "my-rmig"}

	err := c.createInstanceViaMIG(d)
	require.Error(t, err, "expected short-circuit error from the post-stability Instances.Get")

	assert.GreaterOrEqual(t, listCalls, 2,
		"listManagedInstances must be polled at least twice: once seeing CREATING, then again seeing NONE")
	assert.Equal(t, "us-east1-c", c.zone,
		"zone discovery happens off the same listManagedInstances response that confirms stability")
}

func TestDeleteInstanceViaRegionalMIG_IssuesRegionalDelete(t *testing.T) {
	srv, recorded := newMIGTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *[]recordedRequest) {
		switch {
		case strings.Contains(r.URL.Path, "/regions/us-east1/instanceGroupManagers/my-rmig/deleteInstances"):
			respondJSON(t, w, &raw.Operation{Name: "op-rdel-1", Status: "PENDING"})
		case strings.Contains(r.URL.Path, "/regions/us-east1/operations/op-rdel-1/wait"):
			respondJSON(t, w, &raw.Operation{Name: "op-rdel-1", Status: "DONE"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	c := newComputeUtilForRegionalTest(t, srv.URL, "us-east1", "my-rmig")
	// Simulate the post-create state: zone has already been resolved and
	// persisted (this is what newComputeUtil reads back from Driver.Zone
	// on the next docker-machine invocation).
	c.zone = "us-east1-d"

	require.NoError(t, c.deleteInstanceViaMIG())

	var sawDelete bool
	for _, r := range *recorded {
		if r.method == "POST" && strings.HasSuffix(r.path, "/regions/us-east1/instanceGroupManagers/my-rmig/deleteInstances") {
			sawDelete = true
			var req raw.RegionInstanceGroupManagersDeleteInstancesRequest
			require.NoError(t, json.Unmarshal([]byte(r.body), &req))
			require.Len(t, req.Instances, 1)
			assert.Contains(t, req.Instances[0], "zones/us-east1-d/instances/test-instance-abc",
				"regional delete must reference the instance by its discovered zonal URL")
			assert.True(t, req.SkipInstancesOnValidationError)
		}
	}
	assert.True(t, sawDelete, "regional deleteInstances was not called")
}

// migTestServer returns an httptest server that handles whichever MIG / op /
// instance endpoints the test routes through, and a `requests` slice
// recording every path that came in. Handlers respond with DONE operations
// where applicable so the wait-for-op loop terminates quickly.
type recordedRequest struct {
	method   string
	path     string
	rawQuery string
	body     string
}

func newMIGTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, recorded *[]recordedRequest)) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var recorded []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorded = append(recorded, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(body)})
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
