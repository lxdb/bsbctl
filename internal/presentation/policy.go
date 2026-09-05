package presentation

// PolicyConfig is daemon-owned arbitration policy for one plugin channel. It
// is never sent to a plugin process.
type PolicyConfig struct {
	Policy                string `json:"policy"`
	DevicePriority        int    `json:"device_priority,omitempty"`
	HoldMS                int    `json:"hold_ms,omitempty"`
	CooldownMS            int    `json:"cooldown_ms,omitempty"`
	RequiresAck           bool   `json:"requires_ack,omitempty"`
	ActivationAction      string `json:"activation_action,omitempty"`
	RotationIntervalMS    int    `json:"rotation_interval_ms,omitempty"`
	RotationJitterPercent int    `json:"rotation_jitter_percent,omitempty"`
}
