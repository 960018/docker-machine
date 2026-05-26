// SPDX-FileCopyrightText: GitLab
// SPDX-License-Identifier: Apache-2.0

package google

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	raw "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

func TestExperimentalFaults_Active(t *testing.T) {
	t.Run("zero value is inactive", func(t *testing.T) {
		var f experimentalFaults
		assert.False(t, f.active(), "default-constructed experimentalFaults must be a no-op so production builds aren't affected")
	})
	t.Run("any single field makes it active", func(t *testing.T) {
		assert.True(t, experimentalFaults{bulkInsertStatus: 503}.active())
		assert.True(t, experimentalFaults{bulkInsertOpError: "VM_MIN_COUNT_NOT_REACHED"}.active())
		assert.True(t, experimentalFaults{discoverZone: "fail"}.active())
	})
}

func TestMaybeFaultBulkInsertCall(t *testing.T) {
	t.Run("no fault when status is zero", func(t *testing.T) {
		f := experimentalFaults{}
		err := f.maybeFaultBulkInsertCall("vm", "us-east1")
		assert.NoError(t, err)
	})
	t.Run("returns googleapi.Error with configured status", func(t *testing.T) {
		f := experimentalFaults{bulkInsertStatus: 503}
		err := f.maybeFaultBulkInsertCall("vm", "us-east1")
		require.Error(t, err)
		var gErr *googleapi.Error
		require.ErrorAs(t, err, &gErr, "must return a googleapi.Error so the driver's existing handling path is exercised")
		assert.Equal(t, 503, gErr.Code)
		assert.Contains(t, gErr.Message, "synthetic")
	})
	t.Run("403 reproduces IAM regression class", func(t *testing.T) {
		f := experimentalFaults{bulkInsertStatus: 403}
		err := f.maybeFaultBulkInsertCall("vm", "us-east1")
		var gErr *googleapi.Error
		require.ErrorAs(t, err, &gErr)
		assert.Equal(t, 403, gErr.Code)
	})
}

func TestMaybeFaultBulkInsertOperation(t *testing.T) {
	t.Run("no fault leaves op unchanged", func(t *testing.T) {
		f := experimentalFaults{}
		op := &raw.Operation{Name: "op-1"}
		out := f.maybeFaultBulkInsertOperation(op, "vm")
		assert.Same(t, op, out)
		assert.Nil(t, out.Error)
	})
	t.Run("attaches OperationError matching VM_MIN_COUNT_NOT_REACHED shape", func(t *testing.T) {
		f := experimentalFaults{bulkInsertOpError: "VM_MIN_COUNT_NOT_REACHED"}
		op := &raw.Operation{Name: "op-1"}
		out := f.maybeFaultBulkInsertOperation(op, "vm")
		require.NotNil(t, out.Error)
		require.Len(t, out.Error.Errors, 1)
		assert.Equal(t, "VM_MIN_COUNT_NOT_REACHED", out.Error.Errors[0].Code)
		assert.Contains(t, out.Error.Errors[0].Message, "synthetic")
	})
}

func TestMaybeFaultDiscoverZone(t *testing.T) {
	t.Run("empty mode is no-op", func(t *testing.T) {
		f := experimentalFaults{}
		handled, instances, err := f.maybeFaultDiscoverZone("vm")
		assert.False(t, handled)
		assert.Nil(t, instances)
		assert.NoError(t, err)
	})
	t.Run("fail mode returns synthetic 403", func(t *testing.T) {
		f := experimentalFaults{discoverZone: "fail"}
		handled, instances, err := f.maybeFaultDiscoverZone("vm")
		assert.True(t, handled)
		assert.Nil(t, instances)
		var gErr *googleapi.Error
		require.ErrorAs(t, err, &gErr)
		assert.Equal(t, 403, gErr.Code)
	})
	t.Run("empty mode returns nil result + nil error (eventual-consistency class)", func(t *testing.T) {
		f := experimentalFaults{discoverZone: "empty"}
		handled, instances, err := f.maybeFaultDiscoverZone("vm")
		assert.True(t, handled)
		assert.Nil(t, instances)
		assert.NoError(t, err, "empty mode mimics AggregatedList returning zero matches, which is not an HTTP error")
	})
	t.Run("unknown mode warns but does not handle", func(t *testing.T) {
		f := experimentalFaults{discoverZone: "garbage"}
		handled, instances, err := f.maybeFaultDiscoverZone("vm")
		assert.False(t, handled, "unknown mode must fall through so a typo doesn't silently break production")
		assert.Nil(t, instances)
		assert.NoError(t, err)
	})
}
