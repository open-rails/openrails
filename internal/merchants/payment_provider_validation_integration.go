//go:build integration

package merchants

// SetCredentialProbeEndpointsForIntegration points live credential probes at
// integration-test provider servers. It is excluded from production builds.
func (s *Service) SetCredentialProbeEndpointsForIntegration(nmiQueryURL, ccbillDataLinkBaseURL string) {
	if s == nil {
		return
	}
	if nmiQueryURL != "" {
		s.nmiCredentialProbeQueryURL = nmiQueryURL
	}
	if ccbillDataLinkBaseURL != "" {
		s.ccbillCredentialProbeBaseURL = ccbillDataLinkBaseURL
	}
}

// SetNMIProbeV5EndpointForIntegration confines the simulation probe to the
// local integration gateway when testing ordinary account credential rotation.
func (s *Service) SetNMIProbeV5EndpointForIntegration(baseURL string) {
	if s != nil {
		s.nmiProbeV5BaseURL = baseURL
	}
}
