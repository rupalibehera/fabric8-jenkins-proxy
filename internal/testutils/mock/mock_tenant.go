package mock

import (
	"errors"

	"github.com/fabric8-services/fabric8-jenkins-proxy/internal/clients"
)

// Tenant is a simple client for fabric8-tenant.
type Tenant struct {
}

// GetTenantInfo returns a tenant information based on tenant id.
func (t Tenant) GetTenantInfo(tenantID string) (ti clients.TenantInfo, err error) {
	return
}

// GetNamespace mock gets namespace
func (t Tenant) GetNamespace(accessToken string) (namespace clients.Namespace, err error) {
	if accessToken == "ValidToken" {
		namespace = clients.Namespace{
			ClusterURL: "Valid_OpenShift_API_URL",
			Name:       "namespace-jenkins",
			State:      "",
			Type:       "jenkins",
		}
		return
	}

	err = errors.New("Invalid Token")
	return
}
