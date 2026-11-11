package config

// LabConfig is the Lab configuration (MM138, docs/design/OPERATOR_LAB.md,
// ADR-0021). With LAB_EMAILS empty (the default) the Lab does not exist: every
// /api/lab route answers 404.
type LabConfig struct {
	// Emails is LAB_EMAILS, lowercased. Operator tier alone is not enough: the
	// caller's verified email must also be on this list.
	Emails []string
	// DailySpendCapUSD and MonthlySpendCapUSD are the Lab sub-caps, checked on
	// top of the burst caps (LAB_DAILY_SPEND_CAP_USD, LAB_MONTHLY_SPEND_CAP_USD).
	DailySpendCapUSD   float64
	MonthlySpendCapUSD float64
	// AllowLocal lets a FitsHome model run on an idle home card (LAB_ALLOW_LOCAL).
	AllowLocal bool
	// SessionIdleSeconds is how long a warm Lab instance waits for the next
	// run of the same model before it terminates (LAB_SESSION_IDLE_SECONDS).
	SessionIdleSeconds int
}

func loadLabConfig() LabConfig {
	return LabConfig{
		Emails:             splitCSVLower(getEnv("LAB_EMAILS", "")),
		DailySpendCapUSD:   getEnvFloat("LAB_DAILY_SPEND_CAP_USD", 10),
		MonthlySpendCapUSD: getEnvFloat("LAB_MONTHLY_SPEND_CAP_USD", 100),
		AllowLocal:         getEnvBool("LAB_ALLOW_LOCAL", true),
		SessionIdleSeconds: getEnvInt("LAB_SESSION_IDLE_SECONDS", 120),
	}
}
