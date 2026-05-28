// SPDX-FileCopyrightText: GitLab
// SPDX-License-Identifier: Apache-2.0

package google

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
)

func TestExperimentalFaults_Active(t *testing.T) {
	t.Run("zero value is inactive", func(t *testing.T) {
		var f experimentalFaults
		assert.False(t, f.active(), "default-constructed experimentalFaults must be a no-op so production builds aren't affected")
	})
	t.Run("any single field makes it active", func(t *testing.T) {
		assert.True(t, experimentalFaults{BulkInsertStatus: 503}.active())
		assert.True(t, experimentalFaults{BulkInsertOpError: "VM_MIN_COUNT_NOT_REACHED"}.active())
		assert.True(t, experimentalFaults{DiscoverZone: "fail"}.active())
		assert.True(t, experimentalFaults{DeleteInstanceStatus: 503}.active())
	})
}

func TestMaybeFaultBulkInsertCall(t *testing.T) {
	t.Run("no fault when status is zero", func(t *testing.T) {
		f := experimentalFaults{}
		err := f.maybeFaultBulkInsertCall("vm", "us-east1")
		assert.NoError(t, err)
	})
	t.Run("returns googleapi.Error with configured status", func(t *testing.T) {
		f := experimentalFaults{BulkInsertStatus: 503}
		err := f.maybeFaultBulkInsertCall("vm", "us-east1")
		require.Error(t, err)
		var gErr *googleapi.Error
		require.ErrorAs(t, err, &gErr, "must return a googleapi.Error so the driver's existing handling path is exercised")
		assert.Equal(t, 503, gErr.Code)
		assert.Contains(t, gErr.Message, "synthetic")
	})
	t.Run("403 reproduces IAM regression class", func(t *testing.T) {
		f := experimentalFaults{BulkInsertStatus: 403}
		err := f.maybeFaultBulkInsertCall("vm", "us-east1")
		var gErr *googleapi.Error
		require.ErrorAs(t, err, &gErr)
		assert.Equal(t, 403, gErr.Code)
	})
}

func TestMaybeFaultBulkInsertOperation(t *testing.T) {
	t.Run("no fault returns nil error", func(t *testing.T) {
		f := experimentalFaults{}
		assert.NoError(t, f.maybeFaultBulkInsertOperation("vm"))
	})
	t.Run("returns error matching VM_MIN_COUNT_NOT_REACHED shape", func(t *testing.T) {
		f := experimentalFaults{BulkInsertOpError: "VM_MIN_COUNT_NOT_REACHED"}
		err := f.maybeFaultBulkInsertOperation("vm")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "VM_MIN_COUNT_NOT_REACHED")
		assert.Contains(t, err.Error(), "operation error")
		assert.Contains(t, err.Error(), "synthetic")
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
		f := experimentalFaults{DiscoverZone: "fail"}
		handled, instances, err := f.maybeFaultDiscoverZone("vm")
		assert.True(t, handled)
		assert.Nil(t, instances)
		var gErr *googleapi.Error
		require.ErrorAs(t, err, &gErr)
		assert.Equal(t, 403, gErr.Code)
	})
	t.Run("empty mode returns nil result + nil error (eventual-consistency class)", func(t *testing.T) {
		f := experimentalFaults{DiscoverZone: "empty"}
		handled, instances, err := f.maybeFaultDiscoverZone("vm")
		assert.True(t, handled)
		assert.Nil(t, instances)
		assert.NoError(t, err, "empty mode mimics AggregatedList returning zero matches, which is not an HTTP error")
	})
	t.Run("unknown mode warns but does not handle", func(t *testing.T) {
		f := experimentalFaults{DiscoverZone: "garbage"}
		handled, instances, err := f.maybeFaultDiscoverZone("vm")
		assert.False(t, handled, "unknown mode must fall through so a typo doesn't silently break production")
		assert.Nil(t, instances)
		assert.NoError(t, err)
	})
}

func TestMaybeFaultDeleteInstance(t *testing.T) {
	t.Run("no fault returns nil error", func(t *testing.T) {
		f := experimentalFaults{}
		assert.NoError(t, f.maybeFaultDeleteInstance("vm", "us-east1-b"))
	})
	t.Run("returns googleapi.Error with configured status", func(t *testing.T) {
		f := experimentalFaults{DeleteInstanceStatus: 503}
		err := f.maybeFaultDeleteInstance("vm", "us-east1-b")
		require.Error(t, err)
		var gErr *googleapi.Error
		require.ErrorAs(t, err, &gErr, "must be googleapi.Error so isNotFound() in Driver.Remove correctly leaves it as a non-not-found error and propagates the failure up to docker-machine rm")
		assert.Equal(t, 503, gErr.Code)
		assert.Contains(t, gErr.Message, "synthetic")
	})
}
