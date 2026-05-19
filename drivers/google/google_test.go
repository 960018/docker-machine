package google

import (
	"testing"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/stretchr/testify/assert"
)

func TestSetConfigFromFlags(t *testing.T) {
	driver := NewDriver("", "")

	checkFlags := &drivers.CheckDriverOptions{
		FlagsValues: map[string]interface{}{
			"google-project": "PROJECT",
		},
		CreateFlags: driver.GetCreateFlags(),
	}

	err := driver.SetConfigFromFlags(checkFlags)

	assert.NoError(t, err)
	assert.Empty(t, checkFlags.InvalidFlags)
}

func TestSetConfigFromFlags_ProvisionedIopsAndThroughput(t *testing.T) {
	tests := map[string]struct {
		iops               interface{}
		throughput         interface{}
		expectErr          bool
		expectedIops       int
		expectedThroughput int
	}{
		"unset flags default to zero (preserves API defaults)": {
			expectedIops:       0,
			expectedThroughput: 0,
		},
		"positive values are stored on the driver": {
			iops:               3000,
			throughput:         140,
			expectedIops:       3000,
			expectedThroughput: 140,
		},
		"negative iops is rejected": {
			iops:       -1,
			throughput: 140,
			expectErr:  true,
		},
		"negative throughput is rejected": {
			iops:       3000,
			throughput: -1,
			expectErr:  true,
		},
	}

	for tn, tt := range tests {
		t.Run(tn, func(t *testing.T) {
			driver := NewDriver("", "")

			flagsValues := map[string]interface{}{
				"google-project": "PROJECT",
			}
			if tt.iops != nil {
				flagsValues["google-provisioned-iops"] = tt.iops
			}
			if tt.throughput != nil {
				flagsValues["google-provisioned-throughput"] = tt.throughput
			}

			checkFlags := &drivers.CheckDriverOptions{
				FlagsValues: flagsValues,
				CreateFlags: driver.GetCreateFlags(),
			}

			err := driver.SetConfigFromFlags(checkFlags)

			if tt.expectErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedIops, driver.ProvisionedIops)
			assert.Equal(t, tt.expectedThroughput, driver.ProvisionedThroughput)
		})
	}
}

func TestMetadataMapFromStringSlice(t *testing.T) {
	tests := map[string]struct {
		slice          []string
		expectedResult metadataMap
	}{
		"empty slice": {
			slice:          []string{""},
			expectedResult: metadataMap{},
		},
		"missing key=value pair": {
			slice:          []string{"key_1"},
			expectedResult: metadataMap{},
		},
		"key=value pair present": {
			slice:          []string{"key_1=value_1"},
			expectedResult: metadataMap{"key_1": "value_1"},
		},
		"multiple = characters present": {
			slice:          []string{"key_1=value=_1="},
			expectedResult: metadataMap{"key_1": "value=_1="},
		},
		"invalid key value pair": {
			slice:          []string{"key_1="},
			expectedResult: metadataMap{"key_1": ""},
		},
		"multiple metadata items": {
			slice: []string{"key_1=value_1", "key_2=value_2"},
			expectedResult: metadataMap{
				"key_1": "value_1",
				"key_2": "value_2",
			},
		},
	}

	for tn, tt := range tests {
		t.Run(tn, func(t *testing.T) {
			result := metadataMapFromStringSlice(tt.slice)
			assert.Equal(t, tt.expectedResult, result)
		})
	}
}

func TestDriver_HasFallback(t *testing.T) {
	cases := map[string]struct {
		d    Driver
		want bool
	}{
		"all empty":        {Driver{}, false},
		"machine type set": {Driver{FallbackMachineType: "n2d-standard-2"}, true},
		"disk type set":    {Driver{FallbackDiskType: "pd-balanced"}, true},
		"flex list set":    {Driver{FallbackFlexMachineTypes: []string{"n2d-standard-2"}}, true},
		"flex list empty":  {Driver{FallbackFlexMachineTypes: []string{}}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.d.hasFallback())
		})
	}
}

func TestSetConfigFromFlags_FallbackFlags(t *testing.T) {
	driver := NewDriver("", "")

	checkFlags := &drivers.CheckDriverOptions{
		FlagsValues: map[string]interface{}{
			"google-project":                    "PROJECT",
			"google-fallback-machine-type":      "n2d-standard-2",
			"google-fallback-disk-type":         "pd-balanced",
			"google-fallback-flex-machine-type": []string{"n2d-standard-2", "n2-standard-2"},
		},
		CreateFlags: driver.GetCreateFlags(),
	}

	err := driver.SetConfigFromFlags(checkFlags)
	assert.NoError(t, err)
	assert.Empty(t, checkFlags.InvalidFlags)
	assert.Equal(t, "n2d-standard-2", driver.FallbackMachineType)
	assert.Equal(t, "pd-balanced", driver.FallbackDiskType)
	assert.Equal(t, []string{"n2d-standard-2", "n2-standard-2"}, driver.FallbackFlexMachineTypes)
}
