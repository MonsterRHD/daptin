package resource

// ResetCachesForTests detaches the process-wide Olric-backed cache handles.
// Test suites that create several independent databases must call it before
// constructing resources for a fresh database, otherwise cached row
// identities from one database could be observed against another. Production
// deployments run against a single database and never need this.
func ResetCachesForTests() {
	OlricCache = nil
}
