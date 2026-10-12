package account

// Keys stored in Record.Ext. They are part of the on-disk format and are also
// written by the admin API, so they must not be renamed.
const (
	ExtCooldownUntil    = "cooldown_until" // unix ms; COOLING ends at this time
	ExtCooldownReason   = "cooldown_reason"
	ExtDisabledAt       = "disabled_at" // unix ms
	ExtDisabledReason   = "disabled_reason"
	ExtExpiredAt        = "expired_at" // unix ms
	ExtExpiredReason    = "expired_reason"
	ExtForbiddenStrikes = "forbidden_strikes"
	ExtConsole429Count  = "console_429_count"
	ExtConsole429LastAt = "console_429_last_at"
)

// State reasons stored in Record.StateReason and the Ext reason keys.
const (
	ReasonInvalidCredentials = "invalid_credentials"
	ReasonOperatorDisabled   = "operator_disabled"
	ReasonConsole429         = "console_429_threshold_exceeded"
)

// failureExtKeys are the Ext keys removed by Patch.ClearFailures.
var failureExtKeys = [...]string{
	ExtCooldownUntil, ExtCooldownReason,
	ExtDisabledAt, ExtDisabledReason,
	ExtExpiredAt, ExtExpiredReason,
	ExtForbiddenStrikes,
}

// consoleRecoveryExtKeys are removed when an expired console account is
// recovered.
var consoleRecoveryExtKeys = [...]string{
	ExtExpiredAt, ExtExpiredReason,
	ExtConsole429Count, ExtConsole429LastAt,
}

// deleteExtKeys removes keys from an ext map.
func deleteExtKeys(ext map[string]any, keys ...string) {
	for _, k := range keys {
		delete(ext, k)
	}
}
