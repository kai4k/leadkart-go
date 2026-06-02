package platformlead

// Dynamic pricing for a marketplace purchase (ADR 0065). Price = the tier's
// base price minus discounts driven by (a) the buyer tenant's package and
// (b) how many times the lead is already shared (volume) — the platform earns
// from reselling, so each additional buyer is a little cheaper.
//
// The rules here are the starting, fully-explicit defaults the owner can tune;
// the package-discount input is a hook for the subscription concept that lands
// later. Everything is integer paise — no float money.

const (
	// volumeDiscountBpsPerShare is the discount granted per buyer already on
	// the lead, in basis points (1 bp = 0.01%). 500 bp = 5% per prior share.
	volumeDiscountBpsPerShare = 500

	// volumeDiscountBpsCap caps the volume discount alone at 30%.
	volumeDiscountBpsCap = 3000

	// totalDiscountBpsCap caps volume + package discount combined at 50% so a
	// purchase always records a positive amount.
	totalDiscountBpsCap = 5000

	bpsDenominator = 10000
)

// ComputePurchasePricePaisa returns the price a buyer pays, given the tier
// base price, how many buyers already hold the lead (priorShareCount), and the
// buyer's package discount in basis points (0 until the subscription concept
// exists). The result is always >= 1 paisa (the lead_purchases CHECK + the
// LeadPurchase ctor both require a positive amount).
func ComputePurchasePricePaisa(tierBasePaisa int64, priorShareCount, packageDiscountBps int) int64 {
	if tierBasePaisa <= 0 {
		return 0
	}
	volumeBps := priorShareCount * volumeDiscountBpsPerShare
	if volumeBps > volumeDiscountBpsCap {
		volumeBps = volumeDiscountBpsCap
	}
	if packageDiscountBps < 0 {
		packageDiscountBps = 0
	}
	totalBps := volumeBps + packageDiscountBps
	if totalBps > totalDiscountBpsCap {
		totalBps = totalDiscountBpsCap
	}
	price := tierBasePaisa * int64(bpsDenominator-totalBps) / int64(bpsDenominator)
	if price < 1 {
		price = 1
	}
	return price
}
