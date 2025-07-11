package proto

import "slices"

func (s *TunnelSpec) Same(other *TunnelSpec) bool {
	if s == nil || other == nil {
		return s == other
	}
	return s.GetTunnelRef().GetTunnelId() == other.GetTunnelRef().GetTunnelId() &&
		s.GetServerPort() == other.GetServerPort() &&
		s.GetServerAddress() == other.GetServerAddress() &&
		s.GetClientProxyPort() == other.GetClientProxyPort() &&
		slices.Equal(s.GetClientProxyAddresses(), other.GetClientProxyAddresses())
}
