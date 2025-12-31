package config

import "sync"

// ResetForTesting resets the global config state to allow testing different
// configurations in the same test run. This function is only available in test
// builds and should be called between tests that need different configurations.
//
// Example usage:
//
//	func TestWithCustomConfig(t *testing.T) {
//	    t.Cleanup(config.ResetForTesting)
//	    // Set up custom config file...
//	    cfg, err := config.Get()
//	    // ... test with custom config
//	}
func ResetForTesting() {
	globalConfig = nil
	errConfig = nil
	configOnce = sync.Once{}
}
