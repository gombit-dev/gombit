package upgrade

import "testing"

// TestPolicyExceptionsAreNeeded: each exception names a listed release, with
// a reason, that the policy would refuse without it, so none outlives what
// it excuses.
func TestPolicyExceptionsAreNeeded(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for version, reason := range policyExceptions {
		if reason == "" {
			t.Errorf("exception %s has no reason", version)
		}
		if _, err := m.Release(version); err != nil {
			t.Errorf("exception %s: %v", version, err)
			continue
		}
		delete(policyExceptions, version)
		err := m.CheckPolicy(version)
		policyExceptions[version] = reason
		if err == nil {
			t.Errorf("exception %s: the policy passes it without the exception; drop it", version)
		}
	}
}
