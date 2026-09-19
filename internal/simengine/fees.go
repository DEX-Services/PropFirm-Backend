package simengine

import "github.com/shopspring/decimal"

// Real, undiscounted exchange taker fee rates — PROP_FIRM_PLAN.md section
// 11: "live/funded accounts pay the exchange's standard trading fees... no
// prop-firm-specific fee waiver or discount tier." These are also charged
// on evaluation-stage (simulated) trades, for the same reason a simulated
// fill should behave like a real one in every way it can: the account's
// balance/equity math (and therefore its loss-limit breach checks) must
// reflect the same cost a real trade would carry, not a fee-free fantasy.
//
// Every simulated fill is charged the taker rate, not maker: a simulated
// position is never actually resting on a real book waiting to be matched
// (see PROP_FIRM_PLAN.md's plain-English simulation explanation) — it
// always "fills" immediately against the live price, which is the taker
// case, not the maker case. There is no simulated equivalent of resting an
// order that earns a lower maker rate.
//
// Sourced from matching-engine/internal/feeconfig's defaultRates (spot
// taker 0.45%, futures taker 0.045%) — kept here as a literal rather than
// proxied live, since these are the exchange's stable, rarely-changed base
// rates and a proxy call on every single trade would add unnecessary
// latency/failure surface to order placement. Update this if the
// exchange's fee_config values are ever intentionally changed.
var (
	SpotTakerFeeRate    = decimal.RequireFromString("0.0045")
	FuturesTakerFeeRate = decimal.RequireFromString("0.00045")
)

// FeeRateFor returns the real taker fee rate for a market type.
func FeeRateFor(market string) decimal.Decimal {
	if market == "FUTURES" {
		return FuturesTakerFeeRate
	}
	return SpotTakerFeeRate
}
