package config

import "fmt"

const (
	SelectorStrategySessions = "sessions"

	SelectorSessionsOnFullReject = "reject"
	SelectorSessionsOnFullQueue  = "queue"
)

// validateSessionsSelector enforces the sessions strategy rules: every
// target is a local model (session state lives in this process), slot and
// timing settings are sane, and all targets stay resident at the same time.
func validateSessionsSelector(config Config, selectorID string, selector SelectorConfig) error {
	localTargets := make([]string, 0, len(selector.Targets))
	for i, target := range selector.Targets {
		realName, local := config.RealModelName(target)
		if !local {
			return fmt.Errorf("selectors.%s.targets[%d] must resolve to a local model for strategy %q", selectorID, i, selector.Strategy)
		}
		localTargets = append(localTargets, realName)
	}

	if selector.Settings.MaxSessionsPerTarget < 1 {
		return fmt.Errorf("selectors.%s.settings.maxSessionsPerTarget must be >= 1", selectorID)
	}
	if selector.Settings.SessionIdleTimeout <= 0 {
		return fmt.Errorf("selectors.%s.settings.sessionIdleTimeout must be a positive duration", selectorID)
	}
	switch selector.Settings.OnFull {
	case SelectorSessionsOnFullReject, SelectorSessionsOnFullQueue:
	default:
		return fmt.Errorf("selectors.%s.settings.onFull: unknown mode %q (valid: reject, queue)", selectorID, selector.Settings.OnFull)
	}
	if selector.Settings.OnFull == SelectorSessionsOnFullQueue && selector.Settings.QueueTimeout <= 0 {
		return fmt.Errorf("selectors.%s.settings.queueTimeout must be a positive duration when onFull is %q", selectorID, SelectorSessionsOnFullQueue)
	}
	if selector.Settings.OnFull == SelectorSessionsOnFullReject && selector.Settings.RetryAfter <= 0 {
		return fmt.Errorf("selectors.%s.settings.retryAfter must be a positive duration when onFull is %q", selectorID, SelectorSessionsOnFullReject)
	}
	return validateSessionsCoexistence(config, selectorID, localTargets)
}

// validateSessionsCoexistence ensures the targets of a sessions selector all
// live in one group with swap: false (or one expanded matrix set), so the
// group holds all of them in RAM at the same time.
func validateSessionsCoexistence(config Config, selectorID string, targets []string) error {
	if len(targets) <= 1 {
		return nil
	}

	if config.Routing.Router.Use == "matrix" {
		matrix := config.Routing.Router.Settings.Matrix
		if matrix != nil && matrix.program != nil && matrix.program.CanContainAll(targets) {
			return nil
		}
		return fmt.Errorf("selectors.%s.targets must all appear together in one expanded matrix set", selectorID)
	}

	groupOf := make(map[string]string, len(config.Models))
	for groupID, group := range config.Routing.Router.Settings.Groups {
		for _, member := range group.Members {
			groupOf[member] = groupID
		}
	}

	groupID := groupOf[targets[0]]
	if groupID == "" {
		return fmt.Errorf("selectors.%s target %q is not in a routing group", selectorID, targets[0])
	}
	if config.Routing.Router.Settings.Groups[groupID].Swap {
		return fmt.Errorf("selectors.%s sessions targets must share a group with swap: false", selectorID)
	}
	for _, target := range targets[1:] {
		if groupOf[target] != groupID {
			return fmt.Errorf("selectors.%s sessions targets must share one routing group", selectorID)
		}
	}
	return nil
}
