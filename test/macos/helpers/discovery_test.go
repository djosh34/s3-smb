// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"slices"
	"testing"
)

func TestDifference(t *testing.T) {
	found := []string{"z", "b", "b", "a"}
	listed := []string{"a", "c", "c"}
	for _, test := range []struct{ left, right, want []string }{
		{found, listed, []string{"b", "z"}},
		{listed, found, []string{"c"}},
		{found, found, nil},
		{nil, listed, nil},
		{listed, nil, []string{"a", "c"}},
	} {
		if got := Difference(test.left, test.right); !slices.Equal(got, test.want) {
			t.Fatalf("difference %v minus %v = %v, want %v", test.left, test.right, got, test.want)
		}
	}
	if !slices.Equal(found, []string{"z", "b", "b", "a"}) || !slices.Equal(listed, []string{"a", "c", "c"}) {
		t.Fatal("mutated discovery inputs")
	}
}
