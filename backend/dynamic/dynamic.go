package dynamic

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"github.com/rancher/longhorn/types"
)

type Factory struct {
	factories map[string]types.BackendFactory
}

func New(factories map[string]types.BackendFactory) types.BackendFactory {
	return &Factory{
		factories: factories,
	}
}

func (d *Factory) factoryAndAddress(address string) (types.BackendFactory, string) {
	parts := strings.SplitN(address, "://", 2)
	if len(parts) == 2 {
		return d.factories[parts[0]], parts[1]
	}
	return nil, ""
}

func (d *Factory) Create(address string) (types.Backend, error) {
	if factory, address := d.factoryAndAddress(address); factory != nil {
		return factory.Create(address)
	}

	return nil, fmt.Errorf("Create: Failed to find factory for %s", address)
}

func (d *Factory) Wait(address string) error {
	if factory, address := d.factoryAndAddress(address); factory != nil {
		return factory.Wait(address)
	}
	return fmt.Errorf("Wait: Failed to find factory for %s", address)
}

func (d *Factory) WaitAll(addresses ...string) error {
	wg := &sync.WaitGroup{}
	errCh := make(chan error)
	for _, replica := range addresses {
		wg.Add(1)
		go func(replica string) {
			defer wg.Done()
			errCh <- d.Wait(replica)
		}(replica)
	}
	go func() {
		wg.Wait()
		close(errCh)
	}()
	var waitErr error
	for err := range errCh {
		if waitErr == nil {
			waitErr = err
		}
	}
	return errors.Wrap(waitErr, "error waiting for replicas")
}
