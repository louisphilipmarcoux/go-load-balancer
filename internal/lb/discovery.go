package lb

import (
	"fmt"
	"log/slog"
	"time"

	consul "github.com/hashicorp/consul/api"
)

// ConsulClient struct (Unchanged)
type ConsulClient struct {
	client *consul.Client
}

// NewConsulClient (Unchanged)
func NewConsulClient(addr string) (ServiceDiscoverer, error) {
	config := consul.DefaultConfig()
	config.Address = addr
	client, err := consul.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create consul client: %w", err)
	}
	if _, err := client.Agent().Self(); err != nil {
		return nil, fmt.Errorf("failed to connect to consul: %w", err)
	}
	slog.Info("Connected to Consul for service discovery", "addr", addr)
	return &ConsulClient{client: client}, nil
}

// WatchService (CHANGED)
// Now decides which update function to call
func (c *ConsulClient) WatchService(
	serviceName string,
	pool *BackendPool,
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig, // This is the key: L4 passes nil, L7 does not
) {
	go func() {
		lastIndex := uint64(0)
		for {
			slog.Debug("Checking Consul for service updates", "service", serviceName)
			services, meta, err := c.client.Health().Service(serviceName, "", true, &consul.QueryOptions{
				WaitIndex: lastIndex,
			})
			if err != nil {
				slog.Error("Failed to query Consul for service", "service", serviceName, "error", err)
				time.Sleep(10 * time.Second)
				continue
			}
			lastIndex = meta.LastIndex

			newBackends := make(map[string]*BackendConfig)
			for _, service := range services {
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

			// --- THIS IS THE NEW LOGIC ---
			if poolCfg != nil {
				// We have a pool config, so this is L7
				pool.UpdateL7Backends(newBackends, cbCfg, poolCfg)
			} else {
				// No pool config, so this is L4
				pool.UpdateL4Backends(newBackends, cbCfg)
			}
		}
	}()
}
