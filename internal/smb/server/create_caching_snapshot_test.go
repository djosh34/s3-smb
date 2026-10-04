package server

import (
	"fmt"
	"testing"
)

func TestReplayCachingResponseUsesFreshEffectiveH(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(fmt.Sprintf("acknowledged-%t", acknowledged), func(t *testing.T) {
			checkReplayCachingResponseUsesFreshEffectiveH(t, acknowledged)
		})
	}
}
