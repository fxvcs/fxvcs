package app

// SetUpgradeStepHookForTest installs a hook invoked before each applied
// upgrade step (nil clears it). Tests use it to inject interruptions.
func SetUpgradeStepHookForTest(fn func(index int, step UpgradeStep) error) { upgradeStepHook = fn }
