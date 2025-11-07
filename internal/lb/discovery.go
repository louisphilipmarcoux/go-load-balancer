// in internal/lb/discovery.go
package lb

import (
	"fmt"
	"log/slog"
	"time"

	consul "github.com/hashicorp/consul/api"
)

// ConsulClient wraps the Consul API client
type ConsulClient struct {
	client *consul.Client
}

// NewConsulClient creates and pings a new Consul client
func NewConsulClient(addr string) (*ConsulClient, error) {
	config := consul.DefaultConfig()
	config.Address = addr
	client, err := consul.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create consul client: %w", err)
	}

	// Ping Consul to ensure connectivity
	if _, err := client.Agent().Self(); err != nil {
		return nil, fmt.Errorf("failed to connect to consul: %w", err)
	}

	slog.Info("Connected to Consul for service discovery", "addr", addr)
	return &ConsulClient{client: client}, nil
}

// WatchService polls Consul for changes to a service and updates a BackendPool
func (c *ConsulClient) WatchService(
	serviceName string,
	pool *BackendPool,
	cbCfg *CircuitBreakerConfig, // <-- ADD
	poolCfg *ConnectionPoolConfig,
) {
	go func() {
		lastIndex := uint64(0)
		for {
			slog.Debug("Checking Consul for service updates", "service", serviceName)
			services, meta, err := c.client.Health().Service(serviceName, "", true, &consul.QueryOptions{
				WaitIndex: lastIndex, // This is a long poll
			})
			if err != nil {
				slog.Error("Failed to query Consul for service", "service", serviceName, "error", err)
				// Don't overwhelm Consul on error
				time.Sleep(10 * time.Second)
				continue
			}
			// Update lastIndex to enable long-polling
			lastIndex = meta.LastIndex

			// Convert Consul services to our BackendConfig
			newBackends := make(map[string]*BackendConfig)
			for _, service := range services {
				// Use the address if available, otherwise the node address
				addr := service.Service.Address
				if addr == "" {
					addr = service.Node.Address
				}

				be := &BackendConfig{
					Addr:   fmt.Sprintf("%s:%d", addr, service.Service.Port),
					Weight: service.Service.Weights.Passing,
				}
				if be.Weight == 0 {
					be.Weight = 1
				}
				newBackends[be.Addr] = be
			}

			// Atomically update the backend pool with the new list
			pool.UpdateBackends(newBackends, cbCfg, poolCfg)
		}
	}()
}
