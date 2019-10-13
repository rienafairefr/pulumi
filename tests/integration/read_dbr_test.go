// Copyright 2016-2018, Pulumi Corporation.  All rights reserved.

package ints

import (
	"testing"

	"github.com/pulumi/pulumi/pkg/testing/integration"
)

// Test that the engine tolerates two deletions of the same URN in the same plan.
func TestReadDBR(t *testing.T) {
	t.Parallel()
	integration.ProgramTest(t, &integration.ProgramTestOptions{
		Dir:          "read/dbr/step1",
		Dependencies: []string{"@pulumi/pulumi"},
		Quick:        true,
		EditDirs: []integration.EditDir{
			{
				Dir:      "read/dbr/step2",
				Additive: true,
			},
			{
				Dir:      "read/dbr/step3",
				Additive: true,
			},
		},
	})
}