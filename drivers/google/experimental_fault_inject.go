// SPDX-FileCopyrightText: GitLab
// SPDX-License-Identifier: Apache-2.0
//
// EXPERIMENTAL: fault injection for sandbox edge-case reproduction.
//
// This file is not meant for production use. It is a sandbox-only
// affordance that lets a test harness deterministically trigger the
// transient GCP-side failure modes we have observed in the wild:
//
//   - bulkInsert returns a transient 5xx (e.g. 503 backendError)
//   - bulkInsert operation completes with VM_MIN_COUNT_NOT_REACHED
//   - AggregatedList (zone discovery) errors out
//   - AggregatedList returns no instance (eventual-consistency window)
//
// None of these are reachable on a production binary that does not
// set the corresponding driver flags. The default value of every
// fault flag is the empty value, which short-circuits to a no-op.
//
// Flags (all opt-in, all default to no-op):
//   --google-fault-bulk-insert-status=503
//   --google-fault-bulk-insert-op-error=VM_MIN_COUNT_NOT_REACHED
//   --google-fault-discover-zone=fail        (force AggregatedList to error)
//   --google-fault-discover-zone=empty       (force AggregatedList to return zero matches)
//
// The do-not-ship flags live in a single struct on the Driver so they
// are easy to grep for and to strip from the release branch.

package google

import (
	"fmt"

	"github.com/docker/machine/libmachine/log"
	raw "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// experimentalFaults is a sandbox-only knob set. Populated from the
// driver flags during SetConfigFromFlags. Never touched on a production
// driver invocation (default values are zero / empty everywhere).
type experimentalFaults struct {
	bulkInsertStatus  int    // 0 = no fault
	bulkInsertOpError string // "" = no fault
	discoverZone      string // "" = no fault; "fail" or "empty"
}

// active returns true if any fault knob is set. Used to gate the
// per-site Warn-once log so we never noise a real production driver.
func (f experimentalFaults) active() bool {
	return f.bulkInsertStatus != 0 || f.bulkInsertOpError != "" || f.discoverZone != ""
}

// maybeFaultBulkInsertCall returns a synthetic googleapi.Error when
// the operator has configured --google-fault-bulk-insert-status.
// Otherwise returns nil and the real call proceeds.
func (f experimentalFaults) maybeFaultBulkInsertCall(instanceName, region string) error {
	if f.bulkInsertStatus == 0 {
		return nil
	}
	log.Warnf("FAULT-INJECT: bulkInsert for %q in %q -> synthetic HTTP %d", instanceName, region, f.bulkInsertStatus)
	return &googleapi.Error{
		Code:    f.bulkInsertStatus,
		Message: fmt.Sprintf("synthetic fault: HTTP %d (sandbox)", f.bulkInsertStatus),
		// Empty Body and Header on purpose: real GCP 503s have
		// trace IDs etc; we don't need those to exercise the path.
	}
}

// maybeFaultBulkInsertOperation returns a synthetic operation error
// attached to the Operation result, mirroring what GCP attaches when
// e.g. VM_MIN_COUNT_NOT_REACHED fires. The real driver's
// waitForRegionOp checks op.Error and surfaces it; this helper lets
// the test harness force that path.
//
// Returns the modified Operation, or the original if no fault.
func (f experimentalFaults) maybeFaultBulkInsertOperation(op *raw.Operation, instanceName string) *raw.Operation {
	if f.bulkInsertOpError == "" {
		return op
	}
	log.Warnf("FAULT-INJECT: bulkInsert operation for %q -> synthetic op error %q", instanceName, f.bulkInsertOpError)
	op.Error = &raw.OperationError{
		Errors: []*raw.OperationErrorErrors{
			{
				Code:    f.bulkInsertOpError,
				Message: fmt.Sprintf("synthetic fault: %s (sandbox)", f.bulkInsertOpError),
			},
		},
	}
	return op
}

// maybeFaultDiscoverZone is invoked at the AggregatedList call site.
// Returns (handled, instances, err). When handled is true the caller
// uses the returned slice + error as the result of the lookup.
//
//   - mode "fail":  handled=true, instances=nil, err=synthetic 403
//   - mode "empty": handled=true, instances=nil, err=nil (no match found)
//   - default:      handled=false, caller proceeds with the real call
func (f experimentalFaults) maybeFaultDiscoverZone(instanceName string) (bool, []*raw.Instance, error) {
	switch f.discoverZone {
	case "":
		return false, nil, nil
	case "fail":
		log.Warnf("FAULT-INJECT: discoverInstanceZone(%q) -> synthetic HTTP 403", instanceName)
		return true, nil, &googleapi.Error{
			Code:    403,
			Message: "synthetic fault: AggregatedList forbidden (sandbox)",
		}
	case "empty":
		log.Warnf("FAULT-INJECT: discoverInstanceZone(%q) -> synthetic empty result", instanceName)
		return true, nil, nil
	default:
		// Unknown mode: warn once but do not fail the call.
		log.Warnf("FAULT-INJECT: unknown --google-fault-discover-zone=%q, ignoring", f.discoverZone)
		return false, nil, nil
	}
}
