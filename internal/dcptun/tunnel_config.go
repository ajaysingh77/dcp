package dcptun

type TunnelProxyConfig struct {
	// The address for the control endpoint of the server-side tunnel proxy.
	ServerControlAddress string `json:"server_control_address"`

	// The port for the control endpoint of the server-side tunnel proxy.
	ServerControlPort int32 `json:"server_control_port"`

	// The address for the control endpoint of the client-side tunnel proxy.
	// In the context of the server-side proxy, this is the address that the server-side proxy will be using
	// to connect to the control endpoint of the client-side proxy.
	// In the context of the client-side proxy, this is the address that the client-side proxy will be listening on.
	ClientControlAddress string `json:"client_control_address"`

	// The port for the control endpoint of the client-side tunnel proxy.
	// In the context of the server-side proxy, this is the port that the server-side proxy will be using
	// to connect to the control endpoint of the client-side proxy.
	// In the context of the client-side proxy, this is the port that the client-side proxy will be listening on.
	ClientControlPort int32 `json:"client_control_port"`

	// The address for the data endpoint of the client-side tunnel proxy.
	// In the context of the server-side proxy, this is the address that the server-side proxy will be using
	// to connect to the data endpoint of the client-side proxy.
	// In the context of the client-side proxy, this is the address that the client-side proxy will be listening on.
	ClientDataAddress string `json:"client_data_address"`

	// The port for the data endpoint of the client-side tunnel proxy.
	// In the context of the server-side proxy, this is the port that the server-side proxy will be using
	// to connect to the data endpoint of the client-side proxy.
	// In the context of the client-side proxy, this is the port that the client-side proxy will be listening on.
	ClientDataPort int32 `json:"client_data_port"`
}
