package metrics

import (
	"context"
	"strings"
	"testing"
)

func TestMeterProviderBuilds(t *testing.T) {
	mp, err := MeterProvider()
	if err != nil {
		t.Fatalf("MeterProvider: %v", err)
	}
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
}

func TestHandlerStillExposesNativeNames(t *testing.T) {
	// The four domain metrics keep their exact names on the shared registry.
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	// A plain registered counter always emits a zero series; the HistogramVec
	// would not appear until a label series is observed.
	want := "transaction_post_serialization_retries_total"
	found := false
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), want) {
			found = true
		}
	}
	if !found {
		t.Errorf("native metric %q missing from registry", want)
	}
}
