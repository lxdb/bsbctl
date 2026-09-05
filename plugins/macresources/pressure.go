package macresources

type pressureLevel string

const (
	pressureNormal   pressureLevel = "normal"
	pressureWarning  pressureLevel = "warning"
	pressureCritical pressureLevel = "critical"
)

type pressureValues struct {
	CPU     float64
	Memory  float64
	Network float64
}

type pressureState struct {
	level      pressureLevel
	reason     string
	transition bool
}

type pressureMachine struct {
	config       Config
	level        pressureLevel
	reason       string
	pending      pressureLevel
	pendingCount int
}

func newPressureMachine(config Config) *pressureMachine {
	return &pressureMachine{config: config, level: pressureNormal}
}

func (m *pressureMachine) update(values pressureValues) pressureState {
	target := m.target(values)
	transition := false
	if target == m.level {
		m.pending = ""
		m.pendingCount = 0
	} else {
		if target != m.pending {
			m.pending = target
			m.pendingCount = 0
		}
		m.pendingCount++
		if m.pendingCount >= m.config.SustainSamples {
			m.level = target
			m.pending = ""
			m.pendingCount = 0
			transition = true
		}
	}
	reason := pressureReason(values, m.level, m.config)
	if transition && m.level == pressureNormal {
		reason = "pressure_resolved"
		m.reason = ""
	} else if m.level != pressureNormal {
		if reason != "" {
			m.reason = reason
		}
		reason = m.reason
	}
	return pressureState{level: m.level, reason: reason, transition: transition}
}

func (m *pressureMachine) target(values pressureValues) pressureLevel {
	maximum := max(values.CPU, values.Memory, values.Network)
	switch m.level {
	case pressureWarning:
		if maximum >= m.config.CriticalPercent {
			return pressureCritical
		}
		if maximum < m.config.WarningPercent-m.config.RecoveryMarginPercent {
			return pressureNormal
		}
		return pressureWarning
	case pressureCritical:
		if maximum < m.config.WarningPercent-m.config.RecoveryMarginPercent {
			return pressureNormal
		}
		if maximum < m.config.CriticalPercent-m.config.RecoveryMarginPercent {
			return pressureWarning
		}
		return pressureCritical
	default:
		if maximum >= m.config.CriticalPercent {
			return pressureCritical
		}
		if maximum >= m.config.WarningPercent {
			return pressureWarning
		}
		return pressureNormal
	}
}

func pressureReason(values pressureValues, level pressureLevel, config Config) string {
	if level == pressureNormal {
		return "resource_snapshot"
	}
	threshold := config.WarningPercent
	if level == pressureCritical {
		threshold = config.CriticalPercent
	}
	count := 0
	reason := ""
	for _, value := range []struct {
		percent float64
		reason  string
	}{{values.CPU, "cpu_pressure"}, {values.Memory, "memory_pressure"}, {values.Network, "network_pressure"}} {
		if value.percent >= threshold {
			count++
			reason = value.reason
		}
	}
	if count > 1 {
		return "multiple_pressure"
	}
	if count == 1 {
		return reason
	}
	// Hysteresis can keep pressure active after the value leaves its entry band.
	return ""
}
