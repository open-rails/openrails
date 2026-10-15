// Package providerrecovery defines the cadence of provider observations.
// Reconciliation reports discrepancies; it does not authorize or block writes.
package providerrecovery

import "time"

const RefreshInterval = 2 * time.Hour
const SafetyLag = 5 * time.Minute
