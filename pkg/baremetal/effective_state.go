package baremetal

func HasCond(st MachineStatus, t ConditionType) (bool, bool) {
	for _, c := range st.Conditions {
		if c.Type == t {
			return true, c.Status
		}
	}
	return false, false
}

// EffectiveState is what your UI/clients should display.
// It keeps your explicit phase diagram tiny and pushes "truth" into Conditions.
func EffectiveState(st MachineStatus, activeRun *Run) MachinePhase {
	// 1) Active run always wins
	if activeRun != nil && (activeRun.Phase == RunPending || activeRun.Phase == RunRunning) {
		return PhaseProvisioning
	}

	// 2) Explicit terminal / gated phases
	switch st.Phase {
	case PhaseRMA, PhaseRetired, PhaseMaintenance:
		return st.Phase
	}

	// 3) Truth-based derived service state
	if _, ok := HasCond(st, CondInCustomerCluster); ok {
		return PhaseInService
	}

	// 4) Imported vs not
	if st.Phase == PhaseFactoryReady {
		return PhaseFactoryReady
	}

	return PhaseReady
}
