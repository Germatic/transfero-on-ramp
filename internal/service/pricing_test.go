package service

import (
	"math"
	"testing"
)

// defaultParams matches the operator-spec defaults landed 2026-05-28:
// spot_markup_pct=0.36, d0_markup_pct=0.20, basis_threshold_pct=0.25.
var defaultParams = PricingParams{
	SpotMarkupPct:     0.36,
	D0MarkupPct:       0.20,
	BasisThresholdPct: 0.25,
}

func TestComputeCustomerPrice(t *testing.T) {
	cases := []struct {
		name           string
		spot, d0       float64
		params         PricingParams
		expectRegime   string
		expectPrice    float64
		tolerance      float64
	}{
		{
			name:         "basis=1.0 (D0==Spot) → spot anchor",
			spot:         5.00,
			d0:           5.00,
			params:       defaultParams,
			expectRegime: PriceRegimeSpotAnchor,
			expectPrice:  5.018,
			tolerance:    1e-9,
		},
		{
			name:         "basis just below threshold (1.00225) → spot anchor",
			spot:         5.00,
			d0:           5.01125,
			params:       defaultParams,
			expectRegime: PriceRegimeSpotAnchor,
			expectPrice:  5.018,
			tolerance:    1e-9,
		},
		{
			name:         "basis exactly at threshold (1.00250) → spot anchor (inclusive)",
			spot:         5.00,
			d0:           5.0125,
			params:       defaultParams,
			expectRegime: PriceRegimeSpotAnchor,
			expectPrice:  5.018,
			tolerance:    1e-9,
		},
		{
			name:         "basis just above threshold → d0 anchor",
			spot:         5.00,
			d0:           5.01251,
			params:       defaultParams,
			expectRegime: PriceRegimeD0Anchor,
			expectPrice:  5.01251 * 1.0020,
			tolerance:    1e-9,
		},
		{
			name:         "basis 1.010 (well into high regime) → d0 anchor",
			spot:         5.00,
			d0:           5.05,
			params:       defaultParams,
			expectRegime: PriceRegimeD0Anchor,
			expectPrice:  5.0601,
			tolerance:    1e-9,
		},
		{
			name:         "§96.7 overnight case (spot=5.0473 d0=5.0701 basis=1.00452) → d0 anchor",
			spot:         5.0473,
			d0:           5.0701,
			params:       defaultParams,
			expectRegime: PriceRegimeD0Anchor,
			expectPrice:  5.0701 * 1.0020,
			tolerance:    1e-6,
		},
		{
			name:         "custom params: very small threshold flips even tiny basis to d0",
			spot:         5.00,
			d0:           5.00005, // basis = 1.00001
			params:       PricingParams{SpotMarkupPct: 0.36, D0MarkupPct: 0.20, BasisThresholdPct: 0.0001},
			expectRegime: PriceRegimeD0Anchor,
			expectPrice:  5.00005 * 1.0020,
			tolerance:    1e-9,
		},
		{
			name:         "custom params: huge threshold keeps everything in spot anchor",
			spot:         5.00,
			d0:           5.10, // basis = 1.020
			params:       PricingParams{SpotMarkupPct: 0.36, D0MarkupPct: 0.20, BasisThresholdPct: 5.00},
			expectRegime: PriceRegimeSpotAnchor,
			expectPrice:  5.018,
			tolerance:    1e-9,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, regime := computeCustomerPrice(tc.spot, tc.d0, tc.params)
			if regime != tc.expectRegime {
				t.Errorf("regime: got %q, want %q", regime, tc.expectRegime)
			}
			if math.Abs(got-tc.expectPrice) > tc.tolerance {
				t.Errorf("price: got %.10f, want %.10f (Δ=%.3g, tol=%.0e)", got, tc.expectPrice, got-tc.expectPrice, tc.tolerance)
			}
		})
	}
}

// TestComputeCustomerPriceMonotonicInD0 asserts a structural invariant: the
// customer price is monotonically non-decreasing in D0 for a fixed Spot. This
// catches regressions where the regime switch produces a customer-price step
// that goes the wrong way.
func TestComputeCustomerPriceMonotonicInD0(t *testing.T) {
	spot := 5.00
	prev := 0.0
	for d0 := spot; d0 <= spot*1.05; d0 += 0.001 {
		got, _ := computeCustomerPrice(spot, d0, defaultParams)
		if got+1e-9 < prev {
			t.Fatalf("non-monotonic at d0=%.4f: got %.6f, prev %.6f", d0, got, prev)
		}
		prev = got
	}
}

// TestComputeCustomerPriceBoundaryStep confirms the deliberate discontinuity
// at the regime boundary documented in CONTEXT.md §99: crossing D0/Spot=1.0025
// causes a ~9 bps jump in customer price (32.7 bps over spot in the high
// regime vs the smooth 36 bps in the low regime ceiling).
func TestComputeCustomerPriceBoundaryStep(t *testing.T) {
	spot := 5.00
	belowBasis := 1.0 + defaultParams.BasisThresholdPct/100 // 1.0025
	aboveBasis := belowBasis + 1e-6

	priceBelow, regBelow := computeCustomerPrice(spot, spot*belowBasis, defaultParams)
	priceAbove, regAbove := computeCustomerPrice(spot, spot*aboveBasis, defaultParams)

	if regBelow != PriceRegimeSpotAnchor {
		t.Errorf("at boundary: expected spot_anchor, got %q", regBelow)
	}
	if regAbove != PriceRegimeD0Anchor {
		t.Errorf("just above boundary: expected d0_anchor, got %q", regAbove)
	}
	if priceAbove <= priceBelow {
		t.Errorf("expected step UP at boundary (low %.6f → high %.6f)", priceBelow, priceAbove)
	}
	// Documented ~9 bps step (45.05 − 36 = 9.05 bps over spot at the boundary).
	stepBps := (priceAbove - priceBelow) / spot * 10000
	if stepBps < 5 || stepBps > 15 {
		t.Errorf("boundary step out of expected band: %.2f bps (want ~9 bps)", stepBps)
	}
}

func TestEffectiveMarkupPct(t *testing.T) {
	cases := []struct {
		regime string
		want   float64
	}{
		{PriceRegimeSpotAnchor, defaultParams.SpotMarkupPct},
		{PriceRegimeD0Anchor, defaultParams.D0MarkupPct},
		{"unknown", defaultParams.SpotMarkupPct}, // unknown regime defaults to spot markup
	}
	for _, tc := range cases {
		if got := effectiveMarkupPct(tc.regime, defaultParams); got != tc.want {
			t.Errorf("effectiveMarkupPct(%q) = %v, want %v", tc.regime, got, tc.want)
		}
	}
}
