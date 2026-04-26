package service

import (
	"math"

	"github.com/shopspring/decimal"
)

const defaultBalanceRechargeMultiplier = 1.0

func normalizeBalanceRechargeMultiplier(multiplier float64) float64 {
	if math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier <= 0 {
		return defaultBalanceRechargeMultiplier
	}
	return multiplier
}

func calculateCreditedBalance(paymentAmount, multiplier float64) float64 {
	return calculateCreditedBalanceWithBonus(paymentAmount, multiplier, 0)
}

func calculateCreditedBalanceWithBonus(paymentAmount, multiplier, bonusPercent float64) float64 {
	bonusMultiplier := 0.0
	if !math.IsNaN(bonusPercent) && !math.IsInf(bonusPercent, 0) && bonusPercent > 0 {
		bonusMultiplier = bonusPercent / 100
	}
	return decimal.NewFromFloat(paymentAmount).
		Mul(decimal.NewFromFloat(normalizeBalanceRechargeMultiplier(multiplier) + bonusMultiplier)).
		Round(2).
		InexactFloat64()
}

func selectRechargeBonusTier(tiers []RechargeBonusTier, paymentAmount float64) *RechargeBonusTier {
	if len(tiers) == 0 || paymentAmount <= 0 || math.IsNaN(paymentAmount) || math.IsInf(paymentAmount, 0) {
		return nil
	}
	var selected *RechargeBonusTier
	for i := range tiers {
		tier := tiers[i]
		if !isValidRechargeBonusTier(tier) {
			continue
		}
		if paymentAmount < tier.MinAmount {
			continue
		}
		if tier.MaxAmount > 0 && paymentAmount > tier.MaxAmount {
			continue
		}
		if selected == nil || tier.MinAmount >= selected.MinAmount {
			selected = &tiers[i]
		}
	}
	return selected
}

func rechargeBonusPercentForAmount(tiers []RechargeBonusTier, paymentAmount float64) float64 {
	if tier := selectRechargeBonusTier(tiers, paymentAmount); tier != nil {
		return tier.BonusPercent
	}
	return 0
}

func calculateGatewayRefundAmount(orderAmount, payAmount, refundAmount float64) float64 {
	if orderAmount <= 0 || payAmount <= 0 || refundAmount <= 0 {
		return 0
	}
	if math.Abs(refundAmount-orderAmount) <= amountToleranceCNY {
		return decimal.NewFromFloat(payAmount).Round(2).InexactFloat64()
	}
	return decimal.NewFromFloat(payAmount).
		Mul(decimal.NewFromFloat(refundAmount)).
		Div(decimal.NewFromFloat(orderAmount)).
		Round(2).
		InexactFloat64()
}
