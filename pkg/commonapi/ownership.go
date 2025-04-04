package commonapi

const (
	// The key used to create client cache indexes to query for object owners efficiently.
	// See controllers.SetupEndpointIndexWithManager() for an example of usage with Endpoint objects.
	WorkloadOwnerKey = ".metadata.owner"
)
