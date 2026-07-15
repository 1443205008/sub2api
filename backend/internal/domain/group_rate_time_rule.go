package domain

// GroupRateTimeRule applies an additional token billing multiplier during a
// daily time window. Start is inclusive, End is exclusive, and windows may
// cross midnight.
type GroupRateTimeRule struct {
	Start      string  `json:"start"`
	End        string  `json:"end"`
	Multiplier float64 `json:"multiplier"`
}
